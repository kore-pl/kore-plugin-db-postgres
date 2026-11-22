// Package pgop 实现 db-postgres 插件的方法面：参数校验、作用域约束与结构化结果。
// 本包只做逻辑校验与执行面调用，不连接数据库、不内置 psql/pg_dump（骨架口径）。
package pgop

import "fmt"

// maxNameLen 是骨架口径下实例名 / 数据库名的最大长度。
const maxNameLen = 128

// FaultKind 标识一次故障注入的类别。
type FaultKind string

const (
	FaultNone    FaultKind = ""
	FaultKill    FaultKind = "kill"
	FaultTimeout FaultKind = "timeout"
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

// Instance 是一条 PostgreSQL 实例的逻辑占位记录。
type Instance struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

// Connection 是一条连接配置的逻辑占位记录（不含任何口令字段）。
type Connection struct {
	Instance string `json:"instance"`
	Database string `json:"database"`
	User     string `json:"user"`
	Status   string `json:"status"`
}

// Executor 是数据库操作的执行面；骨架实现为 noop，测试可注入故障执行器。
type Executor interface {
	Instances(scope, filter string) ([]Instance, error)
	Connections(scope, instance, database string) ([]Connection, error)
}
