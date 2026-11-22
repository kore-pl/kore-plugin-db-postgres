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

// instancePassword 优先使用配置契约直接保存的密码（仅默认实例），否则从实例声明的
// 环境变量读取。
//
// 两者都没有时返回空串而不是报错：PostgreSQL 本机常见 trust / peer 认证，没有密码是
// 合法配置；真正的认证失败由服务端回 E_UPSTREAM，信息比「密码环境变量未设置」更准。
func instancePassword(ic InstanceConfig) string {
	if ic.Password != "" {
		return ic.Password
	}
	if ic.PasswordEnv == "" {
		return ""
	}
	return os.Getenv(ic.PasswordEnv)
}
