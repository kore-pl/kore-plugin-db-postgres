package pgop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callExec 处理 db-postgres.exec：两级确认 → 写入白名单 → 执行 → 结构化结果。
//
// 为什么单独开方法而不是放开 db-postgres.query：方法级 capability 决定授权面。
// query 声明 system:read，把写语句塞进去等于给「只读操作员」发 DROP 权限；
// exec 声明 system:admin，写能力只对管理员开放，内核 invoke 审计（audit.invoke_all
// 默认只记 admin 类方法）可覆盖其全量调用，拒绝路径由 capability 审计 fail-closed 记录。
//
// 与 db-mysql.exec 的行为对齐（见 kore-plugin-db-mysql/internal/mysqlop/exec.go），
// 差异只在执行面：PG 的写语句走 runStdin（SQL 从 stdin 进 psql，不进 argv / ps），
// 结果按 CSV 解析，RETURNING 的行进入 Columns/Rows，RowCount = len(Rows)。
func callExec(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p execParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	kind, perr := ValidateWriteSQL(p.SQL, p.Confirm, p.ConfirmText)
	if perr != nil {
		return nil, perr
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	result, err := exec.Exec(instance, p.SQL, maxExecRows)
	if err != nil {
		return nil, classify(err)
	}
	out := ExecResult{
		Instance: p.Instance,
		Kind:     kind,
		Executed: true,
		Columns:  result.Columns,
		Rows:     result.Rows,
	}
	if out.Columns == nil {
		out.Columns = []string{}
	}
	if out.Rows == nil {
		out.Rows = []json.RawMessage{}
	}
	out.RowCount = len(out.Rows)
	return encode(out)
}
