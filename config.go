package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore-plugin-db-postgres/internal/pgop"
	"github.com/pengtang1024/kore-plugin-db-postgres/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// embeddedInfo 是随二进制发布的内嵌清单：只含握手字段（manifest.json），不含会随打包回填的 size_bytes。
//
//go:embed manifest.json
var embeddedInfo []byte

// 默认实例的环境变量兜底（配置文件不存在时才生效，优先级 file > env > default）。
// KORE_PG_INSTANCES / KORE_PG_CONFIG / KORE_PG_ROOT 这条多实例路径由
// pgop.LoadEnvInstances 完整保留，配置文件只负责「默认实例」。
const (
	// EnvHost 是默认实例地址。
	EnvHost = "KORE_PG_HOST"
	// EnvPort 是默认实例端口。
	EnvPort = "KORE_PG_PORT"
	// EnvUser 是默认实例用户名。
	EnvUser = "KORE_PG_USER"
	// EnvPasswordEnv 是默认实例密码所在的环境变量名（不是密码本身）。
	EnvPasswordEnv = "KORE_PG_PASSWORD_ENV"
	// EnvIncludeSystem 决定库列表默认是否包含系统模板库。
	EnvIncludeSystem = "KORE_PG_DATABASES_INCLUDE_SYSTEM"

	defaultHost        = "127.0.0.1"
	defaultPort        = 5432
	defaultUser        = "postgres"
	defaultPasswordEnv = "KORE_PG_PASSWORD"
	// maxPasswordLen 防止异常大字段撑爆配置文件；密码内容本身不做字符集限制。
	maxPasswordLen = 1024
)

// manifest 是 info.json 中插件运行期真正需要的字段，其余字段归内核校验。
type manifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Protocol     string   `json:"protocol"`
	RequiresKore string   `json:"requires_kore"`
	Capabilities []string `json:"capabilities"`
}

// fileValues 是 <KORE_STATE_DIR>/config.json 的显式形状（G-24：不引入 object）。
//
// 密码有两种来源：password 直接保存在本机 0600 文件（界面填写，config.get 只回 "***"）；
// password_env 指定环境变量名（老用户兼容）。两者同时存在时 password 优先。
type fileValues struct {
	Host                   string `json:"host"`
	Port                   int    `json:"port"`
	User                   string `json:"user"`
	PasswordEnv            string `json:"password_env"`
	Password               string `json:"password,omitempty"`
	DatabasesIncludeSystem bool   `json:"databases_include_system"`
}

// config 是插件运行期配置：配置契约（config.json）+ 环境变量 + 内嵌清单。
type config struct {
	socket       string
	id           string
	token        string
	version      string
	requiresKore string
	capabilities []string
	startedAt    time.Time
	// stateDir 是插件状态目录（KORE_STATE_DIR），配置落盘为 <stateDir>/config.json。
	stateDir string
	// store 是配置文件的 0600 原子读写器。
	store *plugincfg.Store
	// values / source 是 config.get 的当前值与来源（file / env / default）。
	values fileValues
	source string
	// envInstances 是 KORE_PG_INSTANCES / KORE_PG_CONFIG 提供的附加实例。
	envInstances []pgop.InstanceConfig
	// envErr 记录多实例路径的解析错误；config.set 成功后不再阻断默认实例。
	envErr error
	// pg 是执行面配置；config.set 后在同一进程内替换 Instances，下一次调用即生效。
	pg pgop.Config
	// client 是执行通道（psql / docker exec）与超时；config.set 不需要重建它。
	client *pgop.Client
}

// loadConfig 读取配置契约文件、默认实例环境变量、多实例路径与内嵌 manifest。
func loadConfig() (config, error) {
	var m manifest
	if err := json.Unmarshal(embeddedInfo, &m); err != nil {
		return config{}, fmt.Errorf("解析内嵌 manifest.json: %w", err)
	}
	if m.Protocol != pluginapi.Protocol {
		return config{}, fmt.Errorf("info.json.protocol=%q 与 SDK 协议 %q 不一致", m.Protocol, pluginapi.Protocol)
	}
	if err := plugincfg.ValidateSchema(pgSchema()); err != nil {
		return config{}, fmt.Errorf("config schema 非法: %w", err)
	}

	stateDir := strings.TrimSpace(os.Getenv("KORE_STATE_DIR"))
	store := plugincfg.NewStore(stateDir)
	values, source, err := resolveValues(store)
	if err != nil {
		return config{}, err
	}

	settings, err := pgop.LoadClientSettings()
	if err != nil {
		return config{}, err
	}
	envInstances, envErr := pgop.LoadEnvInstances()

	includeDefault := store.Exists() || primaryEnvSet()
	pgCfg := pgop.Config{
		Instances:       buildInstances(values, includeDefault, envInstances),
		Admin:           hasAdminCapability(m.Capabilities),
		PsqlBin:         settings.PsqlBin,
		DockerBin:       settings.DockerBin,
		DockerContainer: settings.DockerContainer,
		ConnectTimeout:  settings.ConnectTimeout,
		QueryTimeout:    settings.QueryTimeout,
		IncludeSystem:   values.DatabasesIncludeSystem,
	}
	if envErr != nil {
		pgCfg.Err = envErr
	}

	cfg := config{
		socket:       os.Getenv("KORE_PLUGIN_SOCK"),
		id:           os.Getenv("KORE_PLUGIN_ID"),
		token:        os.Getenv("KORE_PLUGIN_TOKEN"),
		version:      m.Version,
		requiresKore: m.RequiresKore,
		capabilities: m.Capabilities,
		startedAt:    time.Now(),
		stateDir:     stateDir,
		store:        store,
		values:       values,
		source:       source,
		envInstances: envInstances,
		envErr:       envErr,
		pg:           pgCfg,
	}
	cfg.client = pgop.NewClient(pgCfg)
	if cfg.id == "" {
		cfg.id = m.ID
	}
	if cfg.socket == "" {
		return config{}, fmt.Errorf("缺少环境变量 KORE_PLUGIN_SOCK")
	}
	return cfg, nil
}

// hasAdminCapability 判断 manifest 是否声明了 system:admin。
// 写方法的插件侧门控用它：内核授权之外再自检一次，避免清单被改小后仍能写库。
func hasAdminCapability(capabilities []string) bool {
	for _, capability := range capabilities {
		if capability == "system:admin" {
			return true
		}
	}
	return false
}

// defaultValues 是代码默认值：本机 5432、postgres、密码取 KORE_PG_PASSWORD。
func defaultValues() fileValues {
	return fileValues{
		Host:        defaultHost,
		Port:        defaultPort,
		User:        defaultUser,
		PasswordEnv: defaultPasswordEnv,
	}
}

// primaryEnvSet 判断是否通过 KORE_PG_HOST 等变量声明了默认实例。
func primaryEnvSet() bool {
	for _, key := range []string{EnvHost, EnvPort, EnvUser, EnvPasswordEnv, EnvIncludeSystem} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// resolveValues 实现优先级 config.json > KORE_PG_* > 代码默认值。
func resolveValues(store *plugincfg.Store) (fileValues, string, error) {
	if store.Exists() {
		raw, _, err := store.Read()
		if err != nil {
			return fileValues{}, "", err
		}
		var values fileValues
		if err := strictDecode(raw, &values); err != nil {
			return fileValues{}, "", fmt.Errorf("解析插件配置 %s: %w", store.Path(), err)
		}
		normalized, err := validateValues(values)
		if err != nil {
			return fileValues{}, "", fmt.Errorf("插件配置 %s 非法: %w", store.Path(), err)
		}
		return normalized, plugincfg.SourceFile, nil
	}

	values := defaultValues()
	source := plugincfg.SourceDefault
	if host := strings.TrimSpace(os.Getenv(EnvHost)); host != "" {
		values.Host = host
		source = plugincfg.SourceEnv
	}
	if raw := strings.TrimSpace(os.Getenv(EnvPort)); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return fileValues{}, "", fmt.Errorf("%s=%q 非法：必须是整数", EnvPort, raw)
		}
		values.Port = port
		source = plugincfg.SourceEnv
	}
	if user := strings.TrimSpace(os.Getenv(EnvUser)); user != "" {
		values.User = user
		source = plugincfg.SourceEnv
	}
	if env := strings.TrimSpace(os.Getenv(EnvPasswordEnv)); env != "" {
		values.PasswordEnv = env
		source = plugincfg.SourceEnv
	}
	if raw := strings.TrimSpace(os.Getenv(EnvIncludeSystem)); raw != "" {
		include, err := strconv.ParseBool(raw)
		if err != nil {
			return fileValues{}, "", fmt.Errorf("%s=%q 非法：必须是 true/false", EnvIncludeSystem, raw)
		}
		values.DatabasesIncludeSystem = include
		source = plugincfg.SourceEnv
	}
	normalized, err := validateValues(values)
	if err != nil {
		return fileValues{}, "", err
	}
	return normalized, source, nil
}

// validateValues 校验并归一化配置值；启动加载与 config.set 共用同一套规则。
func validateValues(values fileValues) (fileValues, error) {
	host := strings.TrimSpace(values.Host)
	if err := pgop.ValidateHost(host); err != nil {
		return fileValues{}, fmt.Errorf("主机地址非法: %w", err)
	}
	port := values.Port
	if port == 0 {
		port = defaultPort
	}
	if port < 1 || port > 65535 {
		return fileValues{}, fmt.Errorf("端口 %d 超出 [1, 65535]", values.Port)
	}
	user := strings.TrimSpace(values.User)
	if user == "" {
		user = defaultUser
	}
	if err := pgop.ValidateUser(user); err != nil {
		return fileValues{}, fmt.Errorf("用户名非法: %w", err)
	}
	passwordEnv := strings.TrimSpace(values.PasswordEnv)
	if passwordEnv != "" {
		if err := pgop.ValidatePasswordEnv(passwordEnv); err != nil {
			return fileValues{}, fmt.Errorf("密码环境变量非法: %w", err)
		}
	}
	if err := pgop.ValidatePassword(values.Password); err != nil {
		return fileValues{}, err
	}
	return fileValues{
		Host:                   host,
		Port:                   port,
		User:                   user,
		PasswordEnv:            passwordEnv,
		Password:               values.Password,
		DatabasesIncludeSystem: values.DatabasesIncludeSystem,
	}, nil
}

// buildInstances 先放配置/环境变量描述的默认实例，再追加多实例路径。
// 多实例里名为 default 的条目会被默认实例覆盖（config.json > 环境变量）。
func buildInstances(values fileValues, includeDefault bool, envInstances []pgop.InstanceConfig) []pgop.InstanceConfig {
	instances := make([]pgop.InstanceConfig, 0, len(envInstances)+1)
	if includeDefault {
		instances = append(instances, pgop.InstanceConfig{
			Name:        pgop.DefaultInstanceName,
			Host:        values.Host,
			Port:        values.Port,
			User:        values.User,
			PasswordEnv: values.PasswordEnv,
			Password:    values.Password,
		})
	}
	for _, instance := range envInstances {
		if includeDefault && instance.Name == pgop.DefaultInstanceName {
			continue
		}
		instances = append(instances, instance)
	}
	return instances
}

// pgSchema 是 config.get 返回的表单描述。
func pgSchema() plugincfg.Schema {
	return plugincfg.Schema{
		Title: "PostgreSQL 连接设置",
		Fields: []plugincfg.Field{
			{
				Key:         "host",
				Label:       "数据库地址",
				Type:        plugincfg.TypeString,
				Default:     json.RawMessage(`"` + defaultHost + `"`),
				Required:    true,
				Help:        "主机名或 IP，本机数据库通常填 127.0.0.1",
				Placeholder: defaultHost,
			},
			{
				Key:         "port",
				Label:       "端口",
				Type:        plugincfg.TypeInt,
				Default:     json.RawMessage(`5432`),
				Required:    true,
				Min:         intPointer(1),
				Max:         intPointer(65535),
				Help:        "PostgreSQL 服务端口，默认 5432（Docker 映射端口如 15432 也可以填这里）",
				Placeholder: "5432",
			},
			{
				Key:      "user",
				Label:    "用户名",
				Type:     plugincfg.TypeString,
				Default:  json.RawMessage(`"` + defaultUser + `"`),
				Required: true,
				Help:     "连接数据库使用的账号，默认超级用户 postgres",
			},
			{
				Key:         "password",
				Label:       "密码",
				Type:        plugincfg.TypeString,
				Default:     json.RawMessage(`""`),
				Required:    false,
				Sensitive:   true,
				Help:        "只保存在本机配置文件（0600），不回显、不写日志；留空则按下面的“密码环境变量”读取",
				Placeholder: "填写数据库密码；本机免密登录留空",
			},
			{
				Key:         "password_env",
				Label:       "密码环境变量（高级）",
				Type:        plugincfg.TypeString,
				Default:     json.RawMessage(`"` + defaultPasswordEnv + `"`),
				Required:    false,
				Help:        "密码也可从该环境变量读取，例：KORE_PG_PASSWORD；上方“密码”优先",
				Placeholder: defaultPasswordEnv,
			},
			{
				Key:      "databases_include_system",
				Label:    "库列表包含模板库",
				Type:     plugincfg.TypeBool,
				Default:  json.RawMessage(`false`),
				Required: false,
				Help:     "勾选后包含 template0 / template1（postgres 维护库始终显示）",
			},
		},
	}
}

// intPointer 返回字段 min/max 需要的指针。
func intPointer(value int) *int { return &value }

// strictDecode 解码 JSON 并拒绝未知字段与尾随内容，避免拼错字段被静默忽略。
func strictDecode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("JSON 对象之后存在多余内容")
	}
	return nil
}
