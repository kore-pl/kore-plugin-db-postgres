package pgop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 慢查询分析的数据来源：pg_stat_statements 扩展视图（SQL 聚合，只读）。
//
// 为什么不用 PostgreSQL 的 log_min_duration_statement 日志文件：
//   - 读日志文件需要宿主文件系统权限，且日志路径/落盘方式由集群参数决定，
//     跨主机/容器不可移植；插件以低权运行，不直连宿主日志目录。
//   - pg_stat_statements 是按 queryid 归并的 SQL 视图，已含执行次数、总/平均/最大
//     执行时间与行数，正好是排慢查询需要的聚合字段。
//
// 维度说明：pg_stat_statements 只有 dbid，没有 schema 列；因此 schema 参数在
// PostgreSQL 下映射为**数据库名过滤**（PG 的 schema 在 SQL 文本内，无法安全聚合）。
const (
	slowlogSource       = "pg_stat_statements"
	defaultSlowLogLimit = 20
	maxSlowLogLimit     = 100
	// maxSlowLogMS 是 min_ms 上限（1 小时），避免无意义的超大值。
	maxSlowLogMS = 3600000
	// slowLogHint 说明数据来源、前置条件与单位（成功/空态都返回）。
	slowLogHint = "数据来自 pg_stat_statements：需 CREATE EXTENSION pg_stat_statements 且在 shared_preload_libraries 中预加载后重启实例；耗时单位为毫秒；rows 为该语句累计返回/影响行数；schema 参数在 PostgreSQL 中按数据库名过滤（该视图只有 dbid，无 schema 维度）"
)

// slowlogParams 是 db-postgres.slowlog 的显式入参形状。
type slowlogParams struct {
	Instance string `json:"instance"`
	// Schema 在 PG 下表示数据库名（pg_stat_statements 只有 dbid）；空表示不过滤。
	Schema string `json:"schema,omitempty"`
	// MinMS 是总耗时阈值（毫秒）：只返回 total_time ≥ min_ms 的语句；0 表示不设阈值。
	MinMS int `json:"min_ms,omitempty"`
	// Limit 是返回条数（按总耗时倒序），1..100，缺省 20；0 视为缺省。
	Limit int `json:"limit,omitempty"`
}

// SlowQueryItem 是一条按 queryid 聚合的慢查询（耗时单位 ms）。
type SlowQueryItem struct {
	Schema     string  `json:"schema"`
	Digest     string  `json:"digest"`
	Query      string  `json:"query"`
	Executions int64   `json:"executions"`
	TotalTime  float64 `json:"total_time"`
	Mean       float64 `json:"mean"`
	Max        float64 `json:"max"`
	Rows       float64 `json:"rows"`
}

// SlowLogResult 是 db-postgres.slowlog 的结构化结果。
// 扩展未安装/未预加载/无权访问时 Available=false + Count=0 + Hint，不编造数据、不报错。
type SlowLogResult struct {
	Instance  string          `json:"instance"`
	Source    string          `json:"source"`
	Schema    string          `json:"schema,omitempty"`
	MinMS     int             `json:"min_ms"`
	Limit     int             `json:"limit"`
	Available bool            `json:"available"`
	Count     int             `json:"count"`
	Queries   []SlowQueryItem `json:"queries"`
	Warnings  []string        `json:"warnings,omitempty"`
	Hint      string          `json:"hint"`
}

// callSlowlog 处理 db-postgres.slowlog：校验参数 → 生成只读聚合 SQL → 结构化结果。
// 只读：SQL 固定为 SELECT，不接受调用方传 SQL，也不写任何服务端状态。
func callSlowlog(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p slowlogParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	schema := strings.TrimSpace(p.Schema)
	if schema != "" {
		if err := ValidateDatabaseName(schema); err != nil {
			return nil, invalid(err.Error())
		}
	}
	if p.MinMS < 0 || p.MinMS > maxSlowLogMS {
		return nil, invalid(fmt.Sprintf("min_ms 必须在 0–%d 之间", maxSlowLogMS))
	}
	limit := p.Limit
	switch {
	case limit == 0:
		limit = defaultSlowLogLimit
	case limit < 0 || limit > maxSlowLogLimit:
		return nil, invalid(fmt.Sprintf("limit 必须在 0–%d 之间", maxSlowLogLimit))
	}
	result, err := exec.Query(instance, buildSlowQuerySQL(schema, p.MinMS, limit), limit)
	if err != nil {
		if isPgStatStatementsUnavailable(err) {
			return encode(SlowLogResult{
				Instance: instance.Name, Source: slowlogSource, Schema: schema,
				MinMS: p.MinMS, Limit: limit, Available: false, Count: 0,
				Queries: []SlowQueryItem{}, Hint: slowUnavailableHint(err),
			})
		}
		return nil, classify(err)
	}
	queries := make([]SlowQueryItem, 0, len(result.Rows))
	warnings := make([]string, 0)
	for index, row := range result.Rows {
		item, decodeErr := decodeSlowQueryRow(row)
		if decodeErr != nil {
			warnings = append(warnings, fmt.Sprintf("第 %d 行无法解析，已跳过: %v", index+1, decodeErr))
			continue
		}
		queries = append(queries, item)
	}
	return encode(SlowLogResult{
		Instance: instance.Name, Source: slowlogSource, Schema: schema,
		MinMS: p.MinMS, Limit: limit, Available: true,
		Count: len(queries), Queries: queries, Warnings: warnings, Hint: slowLogHint,
	})
}

// buildSlowQuerySQL 生成只读聚合 SQL；schema 已过标识符白名单，min_ms/limit 已校验为整数。
func buildSlowQuerySQL(schema string, minMS, limit int) string {
	var b strings.Builder
	b.WriteString("SELECT COALESCE(d.datname, '') AS schema, s.queryid::text AS digest, ")
	b.WriteString("LEFT(regexp_replace(s.query, '[[:space:]]+', ' ', 'g'), 500) AS query, ")
	b.WriteString("s.calls AS executions, ")
	b.WriteString("ROUND(s.total_exec_time::numeric, 3) AS total_time, ")
	b.WriteString("ROUND((s.total_exec_time / GREATEST(s.calls, 1))::numeric, 3) AS mean, ")
	b.WriteString("ROUND(s.max_exec_time::numeric, 3) AS max, ")
	b.WriteString("ROUND(s.rows::numeric, 1) AS rows ")
	b.WriteString("FROM pg_stat_statements s LEFT JOIN pg_database d ON d.oid = s.dbid ")
	b.WriteString("WHERE TRUE")
	if schema != "" {
		// schema 已过 ValidateDatabaseName，再按字符串字面量编码（单引号翻倍）。
		fmt.Fprintf(&b, " AND d.datname = %s", quoteLiteral(schema))
	}
	fmt.Fprintf(&b, " AND s.total_exec_time >= %d", minMS)
	b.WriteString(" ORDER BY s.total_exec_time DESC")
	fmt.Fprintf(&b, " LIMIT %d", limit)
	return b.String()
}

// slowRow 是执行面行 JSON 的解码形状：可为 NULL 的文本列用指针承接。
type slowRow struct {
	Schema     *string `json:"schema"`
	Digest     *string `json:"digest"`
	Query      *string `json:"query"`
	Executions int64   `json:"executions"`
	TotalTime  float64 `json:"total_time"`
	Mean       float64 `json:"mean"`
	Max        float64 `json:"max"`
	Rows       float64 `json:"rows"`
}

// decodeSlowQueryRow 把一行 JSON 转成对外结构；NULL 文本归一为空串。
func decodeSlowQueryRow(raw json.RawMessage) (SlowQueryItem, error) {
	var row slowRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return SlowQueryItem{}, err
	}
	if row.Schema == nil && row.Digest == nil {
		return SlowQueryItem{}, fmt.Errorf("缺少 schema/digest")
	}
	item := SlowQueryItem{
		Executions: row.Executions,
		TotalTime:  row.TotalTime,
		Mean:       row.Mean,
		Max:        row.Max,
		Rows:       row.Rows,
	}
	if row.Schema != nil {
		item.Schema = *row.Schema
	}
	if row.Digest != nil {
		item.Digest = *row.Digest
	}
	if row.Query != nil {
		item.Query = *row.Query
	}
	return item, nil
}

// isPgStatStatementsUnavailable 判断查询失败是否属于「扩展缺失/未预加载/无权限」。
// 命中时返回 available=false 空态（照 mysql 版空态设计），而不是协议错误。
func isPgStatStatementsUnavailable(err error) bool {
	text := strings.ToLower(errorText(err))
	return strings.Contains(text, "pg_stat_statements") ||
		strings.Contains(text, "shared_preload_libraries")
}

// slowUnavailableHint 在空态 hint 里补上探测失败原因（不含凭据）。
func slowUnavailableHint(err error) string {
	return slowLogHint + "；当前探测失败：" + errorText(err)
}
