package pgop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// slowlogRows 是两行典型的 pg_stat_statements 聚合结果。
func slowlogRows() []json.RawMessage {
	rows := []string{
		`{"schema":"shop","digest":"101","query":"SELECT * FROM orders WHERE id = $1","executions":42,"total_time":3210.5,"mean":76.44,"max":900.1,"rows":1200}`,
		`{"schema":"shop","digest":"202","query":"UPDATE inventory SET qty = $1 WHERE sku = $2","executions":7,"total_time":880,"mean":125.7,"max":400,"rows":35}`,
	}
	out := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, json.RawMessage(row))
	}
	return out
}

// TestSlowlogContract 校验入参 → 聚合 SQL → 结构化结果的完整契约。
func TestSlowlogContract(t *testing.T) {
	exec := &fakeExec{query: QueryResult{Rows: slowlogRows()}}
	raw, perr := Call("db-postgres.slowlog",
		json.RawMessage(`{"instance":"primary","schema":"shop","min_ms":50,"limit":5}`), testConfig(), exec)
	if perr != nil {
		t.Fatalf("slowlog 失败: %+v", perr)
	}
	var got SlowLogResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析结果: %v (%s)", err, raw)
	}
	if got.Instance != "primary" || got.Source != "pg_stat_statements" || got.Schema != "shop" ||
		got.MinMS != 50 || got.Limit != 5 || !got.Available || got.Count != 2 || len(got.Queries) != 2 {
		t.Fatalf("结果形状不对: %+v", got)
	}
	first := got.Queries[0]
	if first.Digest != "101" || first.Executions != 42 || first.TotalTime != 3210.5 ||
		first.Mean != 76.44 || first.Max != 900.1 || first.Rows != 1200 ||
		!strings.Contains(first.Query, "orders") {
		t.Fatalf("第一条慢查询解析不对: %+v", first)
	}
	sql := exec.gotSQL
	for _, want := range []string{
		"FROM pg_stat_statements s",
		"s.calls AS executions",
		"ORDER BY s.total_exec_time DESC",
		"d.datname = 'shop'",
		"s.total_exec_time >= 50",
		"LIMIT 5",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL 缺少 %q: %s", want, sql)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(sql), "SELECT ") {
		t.Fatalf("slowlog 必须是只读 SELECT: %s", sql)
	}
	if exec.gotLimit != 5 {
		t.Fatalf("limit 未透传到执行面: %d", exec.gotLimit)
	}
}

// TestSlowlogDefaults 校验缺省 limit=20、min_ms=0、不限定数据库。
func TestSlowlogDefaults(t *testing.T) {
	exec := &fakeExec{}
	if _, perr := Call("db-postgres.slowlog", json.RawMessage(`{"instance":"primary"}`), testConfig(), exec); perr != nil {
		t.Fatalf("slowlog 失败: %+v", perr)
	}
	if exec.gotLimit != defaultSlowLogLimit {
		t.Fatalf("缺省 limit=%d，期望 %d", exec.gotLimit, defaultSlowLogLimit)
	}
	if strings.Contains(exec.gotSQL, "d.datname = '") {
		t.Fatalf("未传 schema 时不应带库过滤: %s", exec.gotSQL)
	}
	if !strings.Contains(exec.gotSQL, ">= 0") || !strings.Contains(exec.gotSQL, "LIMIT 20") {
		t.Fatalf("缺省 SQL 不对: %s", exec.gotSQL)
	}
}

// TestSlowlogValidation 校验参数与实例边界：校验失败不触达执行面。
func TestSlowlogValidation(t *testing.T) {
	cases := []struct {
		name   string
		params string
		code   string
	}{
		{"缺 instance", `{}`, pluginapi.E_INVALID},
		{"未知实例", `{"instance":"absent"}`, pluginapi.E_NOTFOUND},
		{"非法 schema", `{"instance":"primary","schema":"shop;drop"}`, pluginapi.E_INVALID},
		{"limit 越界", `{"instance":"primary","limit":101}`, pluginapi.E_INVALID},
		{"负 limit", `{"instance":"primary","limit":-1}`, pluginapi.E_INVALID},
		{"min_ms 越界", `{"instance":"primary","min_ms":3600001}`, pluginapi.E_INVALID},
		{"负 min_ms", `{"instance":"primary","min_ms":-1}`, pluginapi.E_INVALID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exec := &fakeExec{}
			_, perr := Call("db-postgres.slowlog", json.RawMessage(c.params), testConfig(), exec)
			if perr == nil || perr.Code != c.code {
				t.Fatalf("期望 %s，得到 %+v", c.code, perr)
			}
			if exec.gotSQL != "" {
				t.Fatalf("校验失败仍触达执行面: %s", exec.gotSQL)
			}
		})
	}
}

// TestSlowlogUnavailable 扩展未安装/未预加载/无权限 → available=false + hint 空态，不报错、不编数据。
func TestSlowlogUnavailable(t *testing.T) {
	for _, message := range []string{
		`relation "pg_stat_statements" does not exist`,
		"pg_stat_statements must be loaded via shared_preload_libraries",
		"permission denied for relation pg_stat_statements",
	} {
		exec := &fakeExec{err: opErrorf(pluginapi.E_INTERNAL, "%s", message)}
		raw, perr := Call("db-postgres.slowlog", json.RawMessage(`{"instance":"primary"}`), testConfig(), exec)
		if perr != nil {
			t.Fatalf("%q 应返回空态而非错误: %+v", message, perr)
		}
		var got SlowLogResult
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("解析: %v", err)
		}
		if got.Available || got.Count != 0 || len(got.Queries) != 0 {
			t.Fatalf("%q 应为不可用空态: %+v", message, got)
		}
		if !strings.Contains(got.Hint, "CREATE EXTENSION pg_stat_statements") || !strings.Contains(got.Hint, message) {
			t.Fatalf("%q 的 hint 不完整: %q", message, got.Hint)
		}
	}
}

// TestSlowlogSkipsMalformedRow 校验坏行只记 warning 并跳过，不失败整个调用。
func TestSlowlogSkipsMalformedRow(t *testing.T) {
	exec := &fakeExec{query: QueryResult{Rows: []json.RawMessage{
		json.RawMessage(`{"schema":"shop","digest":"ok","executions":1,"total_time":10}`),
		json.RawMessage(`not-json`),
		json.RawMessage(`{"executions":1}`), // 缺 schema/digest
	}}}
	raw, perr := Call("db-postgres.slowlog", json.RawMessage(`{"instance":"primary"}`), testConfig(), exec)
	if perr != nil {
		t.Fatalf("slowlog 失败: %+v", perr)
	}
	var got SlowLogResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if got.Count != 1 || len(got.Queries) != 1 || got.Queries[0].Digest != "ok" {
		t.Fatalf("应只保留 1 条有效行: %+v", got)
	}
	if len(got.Warnings) != 2 {
		t.Fatalf("应有 2 条 warning: %+v", got.Warnings)
	}
}

// TestSlowlogNullColumns 校验 NULL 文本列归一为空串（JSON null 不能解进 string）。
func TestSlowlogNullColumns(t *testing.T) {
	exec := &fakeExec{query: QueryResult{Rows: []json.RawMessage{
		json.RawMessage(`{"schema":null,"digest":"d","query":null,"executions":2,"total_time":1}`),
	}}}
	raw, perr := Call("db-postgres.slowlog", json.RawMessage(`{"instance":"primary"}`), testConfig(), exec)
	if perr != nil {
		t.Fatalf("slowlog 失败: %+v", perr)
	}
	var got SlowLogResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if got.Count != 1 || got.Queries[0].Query != "" || got.Queries[0].Schema != "" {
		t.Fatalf("NULL 列应归一为空串: %+v", got.Queries)
	}
}

// TestSlowlogQueryError 校验执行面故障按四类映射（超时 → E_UPSTREAM），不误判为空态。
func TestSlowlogQueryError(t *testing.T) {
	exec := &fakeExec{err: NewFaultError(FaultTimeout, "slowlog 执行面故障")}
	_, perr := Call("db-postgres.slowlog", json.RawMessage(`{"instance":"primary"}`), testConfig(), exec)
	if perr == nil || perr.Code != pluginapi.E_UPSTREAM || !strings.Contains(perr.Message, "超时") {
		t.Fatalf("期望 E_UPSTREAM + 超时，得到 %+v", perr)
	}
}
