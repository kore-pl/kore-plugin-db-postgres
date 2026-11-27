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
	// CredentialSource 是本次探测使用的凭据来源：kernel_env | plugin_config | none（只读诊断，不含口令）。
	CredentialSource string `json:"credential_source"`
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
	// Admin 表示插件自身声明了 system:admin（info.json.capabilities）。
	// 内核在 invoke 前已按 method_capabilities 授权；这里是插件侧的纵深防御：
	// 未声明 system:admin 时写方法一律 E_UNAUTHORIZED，不依赖内核配置正确。
	Admin bool
	// PsqlBin / DockerBin 是 psql 与 docker 可执行文件（缺省走 PATH）。
	PsqlBin         string
	DockerBin       string
	DockerContainer string
	// DumpBin / RestoreBin 是 pg_dump 与 pg_restore 可执行文件（缺省走 PATH）。
	DumpBin    string
	RestoreBin string
	// ExportDir 是备份文件的白名单根目录（KORE_DB_EXPORT_DIR，缺省 <state>/exports）。
	// 与 db-mysql 共用同一根：切换数据库类型时「备份到哪 / 从哪恢复」不变。
	ExportDir string
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
//
// 只读四方法与写六方法在同一个接口里：插件只有一种执行面（psql 子进程），
// 分开成两个接口只会让调用方多一次类型断言，换不来安全边界——安全边界在
// admin.go 的能力门控与标识符白名单上。
type Executor interface {
	Instances(instances []InstanceConfig) ([]InstanceStatus, error)
	Databases(instance InstanceConfig, includeSystem bool) ([]Database, error)
	// Query / Exec 的 database 是本次调用的**目标业务库**（可选，空串回落配置维护库
	// defaultDatabase）。PostgreSQL 无跨库限定名，必须靠 psql -d 切库；库名在方法层
	// 过 ValidateDatabaseName 白名单后才到这里，执行面仍做一次纵深校验。
	Query(instance InstanceConfig, sql string, database string, limit int) (QueryResult, error)
	// Exec 执行一条写语句（INSERT/UPDATE/DELETE/MERGE 或 DDL），结果结构与 Query 相同：
	// 写语句本身无结果集，但 PG 的 RETURNING 子句可以返回行，RowCount 为返回行数。
	Exec(instance InstanceConfig, sql string, database string, limit int) (QueryResult, error)
	Status(instance InstanceConfig) (StatusResult, error)

	CreateDatabase(instance InstanceConfig, name, owner string) (bool, error)
	DropDatabase(instance InstanceConfig, name string) (bool, error)
	CreateRole(instance InstanceConfig, user, password string, canCreateDB, canCreateRole bool) (bool, error)
	DropRole(instance InstanceConfig, user string) (bool, error)
	Grant(instance InstanceConfig, user, database string, privileges []string, withGrantOption bool) error
	ChangePassword(instance InstanceConfig, user, password string) error

	// Backup / Restore 是备份与恢复的执行面（pg_dump / pg_restore / psql）。
	Backup(instance InstanceConfig, spec BackupSpec) (BackupData, error)
	Restore(instance InstanceConfig, spec RestoreSpec) error
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
// Database 可选：目标业务库名，缺省回落维护库 postgres（老调用不变）。
type queryParams struct {
	Instance string `json:"instance"`
	SQL      string `json:"sql"`
	Database string `json:"database"`
	Limit    int    `json:"limit"`
}

// execParams 是 db-postgres.exec 的显式入参形状。
// Database 可选：目标业务库名，缺省回落维护库 postgres（老调用不变）。
// confirm 为 true 表示调用方已完成一次确认；ddl 级语句还要求 confirm_text 等于
// 语句首两个关键字（如 "DROP TABLE"），由 ValidateWriteSQL 校验。
type execParams struct {
	Instance    string `json:"instance"`
	SQL         string `json:"sql"`
	Database    string `json:"database"`
	Confirm     bool   `json:"confirm"`
	ConfirmText string `json:"confirm_text"`
}

// ExecResult 是 db-postgres.exec 的结构化结果。
// Columns/Rows 来自写语句的 RETURNING 子句（无 RETURNING 时为空数组），
// RowCount = len(Rows)，与 db-mysql.exec 同形状。
type ExecResult struct {
	Instance string            `json:"instance"`
	Kind     string            `json:"kind"`
	Executed bool              `json:"executed"`
	Columns  []string          `json:"columns"`
	Rows     []json.RawMessage `json:"rows"`
	RowCount int               `json:"row_count"`
}

// statusParams 是 db-postgres.status 的显式入参形状。
type statusParams struct {
	Instance string `json:"instance"`
}

// createDatabaseParams 是 db-postgres.createDatabase 的入参形状。
// Owner 可选（缺省由执行者决定），必须已存在。
type createDatabaseParams struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Owner    string `json:"owner"`
}

// dropDatabaseParams 是 db-postgres.dropDatabase 的入参形状；Confirm 必须为 true。
type dropDatabaseParams struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Confirm  bool   `json:"confirm"`
}

// createUserParams 是 db-postgres.createUser 的入参形状。
// Password 为空表示建一个无口令账号（peer/trust/证书认证等场景合法）。
// CanCreateDb / CanCreateRole 是显式的 CREATEDB / CREATEROLE 选项，不接受超级用户。
type createUserParams struct {
	Instance      string `json:"instance"`
	User          string `json:"user"`
	Password      string `json:"password"`
	CanCreateDb   bool   `json:"can_create_db"`
	CanCreateRole bool   `json:"can_create_role"`
}

// dropUserParams 是 db-postgres.dropUser 的入参形状；Confirm 必须为 true。
type dropUserParams struct {
	Instance string `json:"instance"`
	User     string `json:"user"`
	Confirm  bool   `json:"confirm"`
}

// grantParams 是 db-postgres.grant 的入参形状：库级权限，权限名过白名单。
type grantParams struct {
	Instance        string   `json:"instance"`
	User            string   `json:"user"`
	Database        string   `json:"database"`
	Privileges      []string `json:"privileges"`
	WithGrantOption bool     `json:"with_grant_option"`
}

// changePasswordParams 是 db-postgres.changePassword 的入参形状：
// 口令只走 stdin，不进 argv / 日志 / 响应。
type changePasswordParams struct {
	Instance string `json:"instance"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// CreateDatabaseResult 是 db-postgres.createDatabase 的结果；Created=false 表示库已存在。
type CreateDatabaseResult struct {
	Created bool   `json:"created"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
}

// DropDatabaseResult 是 db-postgres.dropDatabase 的结果。
type DropDatabaseResult struct {
	Dropped bool   `json:"dropped"`
	Name    string `json:"name"`
}

// CreateUserResult 是 db-postgres.createUser 的结果；Created=false 表示账号已存在（未改密）。
type CreateUserResult struct {
	Created bool   `json:"created"`
	User    string `json:"user"`
}

// DropUserResult 是 db-postgres.dropUser 的结果。
type DropUserResult struct {
	Dropped bool   `json:"dropped"`
	User    string `json:"user"`
}

// GrantResult 是 db-postgres.grant 的结果；Privileges 是归一化后实际授予的权限。
type GrantResult struct {
	Granted         bool     `json:"granted"`
	User            string   `json:"user"`
	Database        string   `json:"database"`
	Privileges      []string `json:"privileges"`
	WithGrantOption bool     `json:"with_grant_option"`
}

// ChangePasswordResult 是 db-postgres.changePassword 的结果；绝不回显口令。
type ChangePasswordResult struct {
	Changed bool   `json:"changed"`
	User    string `json:"user"`
}
