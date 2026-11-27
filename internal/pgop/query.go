package pgop

import (
	"encoding/json"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const (
	// defaultQueryLimit 是 db-postgres.query 缺省返回行数。
	defaultQueryLimit = 100
	// maxQueryLimit 是 db-postgres.query 允许的最大行数（任务口径 ≤1000 行）。
	maxQueryLimit = 1000
)

// callQuery 处理 db-postgres.query：只读白名单 → 行数上限 → 执行 → 结构化结果。
// 写操作在触达数据库之前就被 E_INVALID 拒绝。
func callQuery(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p queryParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if strings.TrimSpace(p.SQL) == "" {
		return nil, invalid("请填写必填参数 sql（要查询的 SQL 语句）")
	}
	if perr := ValidateReadOnlySQL(p.SQL); perr != nil {
		return nil, perr
	}
	limit := p.Limit
	switch {
	case limit == 0:
		limit = defaultQueryLimit
	case limit < 0 || limit > maxQueryLimit:
		return nil, invalid("limit 超出 0 到 1000 的范围，请改成范围内的值")
	}
	database, perr := resolveDatabase(p.Database)
	if perr != nil {
		return nil, perr
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	result, err := exec.Query(instance, p.SQL, database, limit)
	if err != nil {
		return nil, classify(err)
	}
	if result.Columns == nil {
		result.Columns = []string{}
	}
	if result.Rows == nil {
		result.Rows = []json.RawMessage{}
	}
	result.RowCount = len(result.Rows)
	return encode(result)
}
