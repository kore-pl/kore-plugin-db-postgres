// Package pgop 实现 db-postgres 插件的方法面：实例配置解析、只读 SQL 白名单、
// 通过本机 psql 客户端（os/exec）执行查询并结构化结果。
//
// 依赖边界（G-17）：只依赖 kore/pkg/pluginapi 与 Go 标准库，不引入任何 PostgreSQL
// driver、不内置 psql 二进制；客户端由宿主机 PATH 提供，可用 KORE_PG_BIN 指到绝对路径。
// 本机没有 psql 时回退到「docker exec <postgres 容器> psql」（见 backend.go）。
package pgop

import (
	"encoding/json"
	"fmt"
	"time"
)

// InstanceConfig 是一条实例配置。密码可以来自配置契约（Password，仅默认实例）
// 或环境变量名（PasswordEnv）；两者都不会进入 argv、ps 或任何响应。
type InstanceConfig struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	PasswordEnv string `json:"password_env"`
	// Password 是配置契约直接保存的密码（仅默认实例使用）；json:"-" 保证它
	// 不会出现在任何响应里，也不接受 KORE_PG_INSTANCES 的 password 字段。
	Password string `json:"-"`
}

// InstanceStatus 是 db-postgres.instances 的单实例结果。Error 为空串表示探测成功。
type InstanceStatus struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version"`
	Error     string `json:"error"`
}

// Database 是 db-postgres.databases 的单库结果（含 pg_database_size 实际字节数）。
// SizeBytes 为 -1 表示该库的大小取不到（例如非超级用户无 CONNECT 权限），
// 不是「大小为 0」——0 字节的库与取不到必须能区分。
type Database struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// FaultKind 标识一次故障注入的类别（四类，与 tests/fault 对齐）。
type FaultKind string

const (
	FaultNone    FaultKind = ""
	FaultKill    FaultKind = "kill"
	FaultTimeout FaultKind = "timeout"
	FaultDisk    FaultKind = "disk"
	FaultCgroup  FaultKind = "cgroup"
)

// FaultError 是执行面注入的故障错误，Call 将其映射为协议错误码。
type FaultError struct {
	Kind    FaultKind
	Message string
}

// Error 实现 error 接口，输出 "<类别>: <说明>"。
func (e *FaultError) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Message) }

// NewFaultError 构造给定类别的故障错误。
func NewFaultError(kind FaultKind, message string) *FaultError {
	return &FaultError{Kind: kind, Message: message}
}

// Config 是插件运行期 PostgreSQL 配置。
type Config struct {
	Instances []InstanceConfig
	// PsqlBin / DockerBin 是 psql 与 docker 可执行文件（缺省走 PATH）。
	PsqlBin         string
	DockerBin       string
	DockerContainer string
	// ConnectTimeout 是元数据方法（instances/databases/status）的整体预算。
	ConnectTimeout time.Duration
	// QueryTimeout 是 db-postgres.query 的整体预算。
	QueryTimeout time.Duration
	// IncludeSystem 是 db-postgres.databases 未显式传 include_system 时的默认值。
	IncludeSystem bool
	// Err 是配置解析错误；非 nil 时所有业务方法返回 E_INVALID，但插件仍可启动。
	Err error
}

// Executor 是数据库操作执行面：真实实现是 psql / docker exec，测试可注入故障。
type Executor interface {
	Instances(instances []InstanceConfig) ([]InstanceStatus, error)
	Databases(instance InstanceConfig, includeSystem bool) ([]Database, error)
	Query(instance InstanceConfig, sql string, limit int) (QueryResult, error)
	Status(instance InstanceConfig) (StatusResult, error)
}

// InstancesResult 是 db-postgres.instances 的结构化结果。
//
// Hint 只在空态（count=0）或全部不可达时出现，是给前端的「下一步做什么」，
// 不是错误：调用本身仍然成功，实例列表照常为空。
type InstancesResult struct {
	Instances []InstanceStatus `json:"instances"`
	Count     int              `json:"count"`
	Hint      string           `json:"hint,omitempty"`
}

// DatabasesResult 是 db-postgres.databases 的结构化结果。
type DatabasesResult struct {
	Instance  string     `json:"instance"`
	Databases []Database `json:"databases"`
	Count     int        `json:"count"`
}

// QueryResult 是 db-postgres.query 的结构化结果。
//
// Rows 的每个元素是已编码的 JSON 对象（列名 → 值），值按 psql 文本推断为
// null / 数字 / 字符串；行内容动态，因此在解析收口处直接产出 json.RawMessage，
// 不向对外 schema 泄漏 map[string]any（G-24）。
type QueryResult struct {
	Columns   []string          `json:"columns"`
	Rows      []json.RawMessage `json:"rows"`
	RowCount  int               `json:"row_count"`
	Truncated bool              `json:"truncated"`
}

// StatusResult 是 db-postgres.status 的结构化结果；指标全部来自 pg_stat_database
// 与 pg_stat_activity，CacheHitRatio = blks_hit / (blks_hit + blks_read)。
type StatusResult struct {
	Instance      string  `json:"instance"`
	Version       string  `json:"version"`
	Connections   int64   `json:"connections"`
	Databases     int64   `json:"databases"`
	UptimeS       int64   `json:"uptime_s"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	BlocksHit     int64   `json:"blocks_hit"`
	BlocksRead    int64   `json:"blocks_read"`
}

// instancesParams 是 db-postgres.instances 的显式入参形状（instance 可选）。
type instancesParams struct {
	Instance string `json:"instance"`
}

// databasesParams 是 db-postgres.databases 的显式入参形状。
type databasesParams struct {
	Instance      string `json:"instance"`
	IncludeSystem *bool  `json:"include_system"`
}

// queryParams 是 db-postgres.query 的显式入参形状。
type queryParams struct {
	Instance string `json:"instance"`
	SQL      string `json:"sql"`
	Limit    int    `json:"limit"`
}

// statusParams 是 db-postgres.status 的显式入参形状。
type statusParams struct {
	Instance string `json:"instance"`
}
