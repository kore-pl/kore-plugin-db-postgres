package pgop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 实例配置来源（按优先级）：KORE_PG_INSTANCES 环境变量 → KORE_PG_CONFIG 文件
// → <KORE_PG_ROOT>/instances.json。三处都没有时视为「未配置附加实例」，不报错。
const (
	// EnvInstances 直接携带 JSON 数组配置。
	EnvInstances = "KORE_PG_INSTANCES"
	// EnvConfig 指向 JSON 配置文件路径。
	EnvConfig = "KORE_PG_CONFIG"
	// EnvRoot 是插件数据根目录，供缺省 instances.json 使用。
	EnvRoot = "KORE_PG_ROOT"
	// EnvBin 覆盖 psql 客户端路径（缺省 PATH 中的 psql）。
	EnvBin = "KORE_PG_BIN"
	// EnvDockerBin 覆盖 docker 客户端路径（缺省 PATH 中的 docker）。
	EnvDockerBin = "KORE_PG_DOCKER_BIN"
	// EnvDockerContainer 固定 docker exec 回退使用的容器名（缺省自动发现）。
	EnvDockerContainer = "KORE_PG_DOCKER_CONTAINER"
	// EnvConnectTimeout 覆盖元数据方法（instances/databases/status）的整体预算。
	EnvConnectTimeout = "KORE_PG_CONNECT_TIMEOUT"
	// EnvQueryTimeout 覆盖 db-postgres.query 的整体预算。
	EnvQueryTimeout = "KORE_PG_TIMEOUT"
	// EnvDumpBin 覆盖 pg_dump 客户端路径（缺省 PATH 中的 pg_dump）。
	EnvDumpBin = "KORE_PG_DUMP_BIN"
	// EnvRestoreBin 覆盖 pg_restore 客户端路径（缺省 PATH 中的 pg_restore）。
	EnvRestoreBin = "KORE_PG_RESTORE_BIN"
	// EnvExportDir 覆盖备份文件的白名单根目录（必须是绝对路径）。
	// 与 db-mysql 共用同一个环境变量名：同一台机器上两个域看到同一个备份目录。
	EnvExportDir = "KORE_DB_EXPORT_DIR"

	// DefaultInstanceName 是配置契约描述的默认实例名。
	DefaultInstanceName = "default"
	// defaultDatabase 是元数据查询连接的维护库。
	defaultDatabase = "postgres"

	defaultPsqlBin   = "psql"
	defaultDockerBin = "docker"
	// defaultConnectTimeout 与「连接超时 5s」口径一致。
	defaultConnectTimeout = 5 * time.Second
	// defaultQueryTimeout 与「查询超时 10s」口径一致。
	defaultQueryTimeout = 10 * time.Second
	maxTimeout          = 2 * time.Minute
	maxInstances        = 64
	// maxDatabases 限制库列表条目数（PostgreSQL 单实例不会超过这个量级）。
	maxDatabases = 4096

	// DefaultExportDirName 是未配置 KORE_DB_EXPORT_DIR 时相对插件状态目录的子目录名。
	DefaultExportDirName = "exports"
)

// ClientSettings 是执行通道与超时设置（与实例表分开，config.set 不重建通道）。
type ClientSettings struct {
	PsqlBin         string
	DockerBin       string
	DockerContainer string
	ConnectTimeout  time.Duration
	QueryTimeout    time.Duration
}

// LoadClientSettings 读取 psql/docker 路径与两个超时（KORE_PG_BIN / KORE_PG_DOCKER_BIN /
// KORE_PG_DOCKER_CONTAINER / KORE_PG_CONNECT_TIMEOUT / KORE_PG_TIMEOUT）。
func LoadClientSettings() (ClientSettings, error) {
	settings := ClientSettings{
		PsqlBin:         strings.TrimSpace(os.Getenv(EnvBin)),
		DockerBin:       strings.TrimSpace(os.Getenv(EnvDockerBin)),
		DockerContainer: strings.TrimSpace(os.Getenv(EnvDockerContainer)),
		ConnectTimeout:  defaultConnectTimeout,
		QueryTimeout:    defaultQueryTimeout,
	}
	if raw := strings.TrimSpace(os.Getenv(EnvConnectTimeout)); raw != "" {
		parsed, err := parseTimeout(raw, EnvConnectTimeout)
		if err != nil {
			return ClientSettings{}, err
		}
		settings.ConnectTimeout = parsed
	}
	if raw := strings.TrimSpace(os.Getenv(EnvQueryTimeout)); raw != "" {
		parsed, err := parseTimeout(raw, EnvQueryTimeout)
		if err != nil {
			return ClientSettings{}, err
		}
		settings.QueryTimeout = parsed
	}
	return settings, nil
}

// parseTimeout 解析 Go 时长并限制在 (0, 2m] 内。
func parseTimeout(raw, key string) (time.Duration, error) {
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 || parsed > maxTimeout {
		return 0, fmt.Errorf("%s=%q 非法：须为 (0, %s] 的 Go 时长（如 5s）", key, raw, maxTimeout)
	}
	return parsed, nil
}

// LoadDumpBin 读取 pg_dump 客户端路径（KORE_PG_DUMP_BIN，缺省 PATH 中的 pg_dump）。
func LoadDumpBin() string {
	if bin := strings.TrimSpace(os.Getenv(EnvDumpBin)); bin != "" {
		return bin
	}
	return defaultDumpBin
}

// LoadRestoreBin 读取 pg_restore 客户端路径（KORE_PG_RESTORE_BIN，缺省 PATH 中的 pg_restore）。
func LoadRestoreBin() string {
	if bin := strings.TrimSpace(os.Getenv(EnvRestoreBin)); bin != "" {
		return bin
	}
	return defaultRestoreBin
}

// ResolveExportDir 解析备份文件的白名单根：
// KORE_DB_EXPORT_DIR 优先；否则 <KORE_STATE_DIR>/exports；两者都没有时返回空串，
// 由方法层给出可操作的 E_INVALID（而不是写相对路径）。
func ResolveExportDir(stateDir string) string {
	if dir := strings.TrimSpace(os.Getenv(EnvExportDir)); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(stateDir); dir != "" {
		return filepath.Join(dir, DefaultExportDirName)
	}
	return ""
}

// LoadEnvInstances 读取 KORE_PG_INSTANCES / KORE_PG_CONFIG / <root>/instances.json
// 这条多实例路径；三处都没有时返回空列表。
func LoadEnvInstances() ([]InstanceConfig, error) {
	raw, source, err := instancesSource()
	if err != nil {
		return nil, err
	}
	instances, err := ParseInstances(raw)
	if err != nil {
		return nil, fmt.Errorf("解析实例配置（%s）: %w", source, err)
	}
	return instances, nil
}

// instancesSource 按优先级取出配置原文；三处都没有时返回空数组。
func instancesSource() ([]byte, string, error) {
	if raw := strings.TrimSpace(os.Getenv(EnvInstances)); raw != "" {
		return []byte(raw), EnvInstances, nil
	}
	if path := strings.TrimSpace(os.Getenv(EnvConfig)); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, EnvConfig, fmt.Errorf("读取 %s 失败: %w", EnvConfig, err)
		}
		return data, EnvConfig + "=" + path, nil
	}
	root := strings.TrimSpace(os.Getenv(EnvRoot))
	if root == "" {
		root = "/var/lib/kore/db-postgres"
	}
	path := filepath.Join(root, "instances.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []byte("[]"), "缺省配置（未配置附加实例）", nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("读取实例配置文件失败: %w", err)
	}
	return data, path, nil
}

// ParseInstances 解析并校验实例配置 JSON 数组，补齐缺省值。
//
// 严格模式：未知字段直接报错，避免把 password 之类的明文误写进配置后被静默忽略。
// 校验后仍然只有 password_env（环境变量名），不保存任何密码值。
func ParseInstances(raw []byte) ([]InstanceConfig, error) {
	list := []InstanceConfig{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return list, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&list); err != nil {
		return nil, fmt.Errorf("实例配置必须是 JSON 数组: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	if len(list) > maxInstances {
		return nil, fmt.Errorf("实例数量 %d 超过上限 %d", len(list), maxInstances)
	}
	seen := make(map[string]struct{}, len(list))
	for i := range list {
		if err := normalizeInstance(&list[i]); err != nil {
			return nil, fmt.Errorf("第 %d 条实例: %w", i+1, err)
		}
		if _, dup := seen[list[i].Name]; dup {
			return nil, fmt.Errorf("实例名 %q 重复", list[i].Name)
		}
		seen[list[i].Name] = struct{}{}
	}
	return list, nil
}

// ensureEOF 确认 JSON 数组后没有多余内容。
func ensureEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("实例配置 JSON 后存在多余内容")
		}
		return fmt.Errorf("实例配置 JSON 尾部非法: %w", err)
	}
	return nil
}

// normalizeInstance 校验单条实例配置并补齐缺省值（port=5432、user=postgres）。
func normalizeInstance(ic *InstanceConfig) error {
	ic.Name = strings.TrimSpace(ic.Name)
	ic.Host = strings.TrimSpace(ic.Host)
	ic.User = strings.TrimSpace(ic.User)
	if err := validateInstanceName(ic.Name); err != nil {
		return err
	}
	if ic.Host == "" {
		ic.Host = "127.0.0.1"
	}
	if err := validateHost(ic.Host); err != nil {
		return fmt.Errorf("实例 %q 的 host 非法: %w", ic.Name, err)
	}
	if ic.Port == 0 {
		ic.Port = 5432
	}
	if ic.Port < 1 || ic.Port > 65535 {
		return fmt.Errorf("实例 %q 的 port=%d 超出 [1, 65535]", ic.Name, ic.Port)
	}
	if ic.User == "" {
		ic.User = "postgres"
	}
	if err := validateUser(ic.User); err != nil {
		return fmt.Errorf("实例 %q 的 user 非法: %w", ic.Name, err)
	}
	if ic.PasswordEnv != "" {
		ic.PasswordEnv = strings.TrimSpace(ic.PasswordEnv)
		if err := ValidatePasswordEnv(ic.PasswordEnv); err != nil {
			return fmt.Errorf("实例 %q 的 password_env 非法: %w", ic.Name, err)
		}
	}
	return nil
}

const (
	// KernelPasswordEnv 是内核按 ADR-0026 注入的凭据环境变量；存在即优先于插件 config 口令。
	KernelPasswordEnv = "PGPASSWORD"
	// CredentialSourceKernelEnv / PluginConfig / None 是对外诊断字段 credential_source 的三态。
	CredentialSourceKernelEnv    = "kernel_env"
	CredentialSourcePluginConfig = "plugin_config"
	CredentialSourceNone         = "none"
)

// readConfigPassword 是读取插件配置口令的唯一入口；单测可替换并计数，
// 用来断言「内核注入 env 存在时零次读取 config 口令」。
var readConfigPassword = func(ic InstanceConfig) string { return ic.Password }

// resolveCredential 返回连接口令与来源（ADR-0026 / §3.5 #8）：
//   - kernel_env：默认实例 + 内核注入的 PGPASSWORD 存在 → 直接用，**不再读 config 口令**；
//   - plugin_config：env 缺失时回落插件 config（password 或 password_env），向后兼容；
//   - none：两者都没有（PostgreSQL 本机常见 trust/peer 认证，空口令是合法配置）。
//
// 内核注入只作用于默认实例：多实例的其它实例各有自己的 config 口令，避免把默认实例的
// 凭据误发给别的数据库。
func resolveCredential(ic InstanceConfig) (string, string) {
	if ic.Name == DefaultInstanceName {
		if password, ok := os.LookupEnv(KernelPasswordEnv); ok && password != "" {
			return password, CredentialSourceKernelEnv
		}
	}
	if password := readConfigPassword(ic); password != "" {
		return password, CredentialSourcePluginConfig
	}
	if ic.PasswordEnv != "" && ic.PasswordEnv != KernelPasswordEnv {
		if password, ok := os.LookupEnv(ic.PasswordEnv); ok && password != "" {
			return password, CredentialSourcePluginConfig
		}
	}
	return "", CredentialSourceNone
}

// instancePassword 沿用旧签名：没有口令时返回空串而不是报错（trust/peer 合法；
// 真正的认证失败由服务端回 E_UPSTREAM，信息更准）。
func instancePassword(ic InstanceConfig) string {
	password, _ := resolveCredential(ic)
	return password
}
