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
		return nil, invalid("缺少必填参数 sql")
	}
	if perr := ValidateReadOnlySQL(p.SQL); perr != nil {
		return nil, perr
	}
	limit := p.Limit
	switch {
	case limit == 0:
		limit = defaultQueryLimit
	case limit < 0 || limit > maxQueryLimit:
		return nil, invalid("limit 超出 [0, 1000] 范围")
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	result, err := exec.Query(instance, p.SQL, limit)
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
