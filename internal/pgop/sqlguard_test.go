package pgop

import (
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestValidateReadOnlySQLAllow 只读查询的合法形态必须放行（收紧不能挡住正常查询）。
func TestValidateReadOnlySQLAllow(t *testing.T) {
	allowed := []string{
		"SELECT 1",
		"select * from pg_database",
		"SELECT 1;",
		"  \n SELECT count(*) FROM pg_stat_activity  \n ",
		"SHOW server_version",
		"SHOW ALL",
		"EXPLAIN SELECT 1",
		"EXPLAIN ANALYZE SELECT 1",
		"EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM pg_class LIMIT 1",
		"EXPLAIN VERBOSE SELECT 1",
		"WITH x AS (SELECT 1 AS n) SELECT * FROM x",
		"WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3) SELECT * FROM r",
		"SELECT 'DROP TABLE x' AS s",
		"SELECT 'a;b' AS s, $$ c;d $$ AS q, $tag$ e;f $tag$ AS q2",
		"SELECT E'it\\'s' AS s",
		"SELECT 1 -- 行注释里写 DROP TABLE x 也不算语句",
		"SELECT /* 块注释 DROP TABLE x */ 1",
		"SELECT * FROM t WHERE a = 1 AND b IN (SELECT b FROM u)",
		"SELECT * FROM t FETCH FIRST 10 ROWS ONLY",
		"SELECT datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate ORDER BY datname",
	}
	for _, sql := range allowed {
		if perr := ValidateReadOnlySQL(sql); perr != nil {
			t.Fatalf("合法 SQL 被拒: %q → %v", sql, perr)
		}
	}
}

// TestValidateReadOnlySQLReject 写操作 / 多语句 / 副作用通道必须返回 E_INVALID。
func TestValidateReadOnlySQLReject(t *testing.T) {
	cases := []struct {
		sql  string
		hint string
	}{
		{"DROP TABLE x", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"DELETE FROM x", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"INSERT INTO x VALUES (1)", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"UPDATE x SET a = 1", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"TRUNCATE x", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"VACUUM", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"CREATE TABLE y (id int)", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"SELECT 1; DROP TABLE x", "请一次只提交一条只读语句"},
		{"SELECT 1; SELECT 2", "请一次只提交一条只读语句"},
		{"SELECT 1; SELECT 2;", "请一次只提交一条只读语句"},
		{"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", "WITH 子句里请勿写修改数据的语句 DELETE"},
		{"WITH x AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM x", "WITH 子句里请勿写修改数据的语句 INSERT"},
		{"WITH x AS (UPDATE t SET a = 1 RETURNING *) SELECT * FROM x", "WITH 子句里请勿写修改数据的语句 UPDATE"},
		{"EXPLAIN ANALYZE DELETE FROM t", "EXPLAIN 请只用于分析"},
		{"EXPLAIN (ANALYZE) UPDATE t SET a = 1", "EXPLAIN 请只用于分析"},
		{"EXPLAIN ANALYZE INSERT INTO t VALUES (1)", "EXPLAIN 请只用于分析"},
		{"EXPLAIN EXECUTE plan1", "EXPLAIN 请只用于分析"},
		{"SELECT * INTO newtab FROM t", "SELECT ... INTO"},
		{"SELECT * FROM t FOR UPDATE", "加锁读，请去掉加锁子句"},
		{"SELECT * FROM t FOR SHARE", "加锁读，请去掉加锁子句"},
		{"SELECT * FROM t FOR NO KEY UPDATE", "加锁读，请去掉加锁子句"},
		{"SELECT pg_sleep(10)", "有副作用或会阻塞的函数 pg_sleep"},
		{"SELECT pg_read_file('/etc/passwd')", "有副作用或会阻塞的函数 pg_read_file"},
		{"SELECT lo_import('/etc/passwd')", "有副作用或会阻塞的函数 lo_import"},
		{"SELECT set_config('search_path', '/tmp', false)", "有副作用或会阻塞的函数 set_config"},
		{"SELECT nextval('seq')", "有副作用或会阻塞的函数 nextval"},
		{"COPY t TO '/tmp/x'", "以 SELECT/SHOW/EXPLAIN/WITH 开头"},
		{"\\g", "反斜杠"},
		{"SELECT 1 \\g", "反斜杠"},
		{"SELECT 'unterminated", "引号未闭合"},
		{"SELECT $$ unterminated", "美元引号未闭合"},
		{"SELECT /* unterminated", "块注释未闭合"},
		{"", "请填写要查询的 SQL 语句"},
		{"   ", "请填写要查询的 SQL 语句"},
		{"SELECT 1\x00", "NUL"},
	}
	for _, c := range cases {
		perr := ValidateReadOnlySQL(c.sql)
		if perr == nil {
			t.Fatalf("非法 SQL 未被拒绝: %q", c.sql)
		}
		if perr.Code != pluginapi.E_INVALID {
			t.Fatalf("SQL=%q 错误码=%q，期望 E_INVALID", c.sql, perr.Code)
		}
		if !strings.Contains(perr.Message, c.hint) {
			t.Fatalf("SQL=%q 错误信息=%q，期望含 %q", c.sql, perr.Message, c.hint)
		}
	}
}

// TestValidateReadOnlySQLLengthLimit 超长 SQL 直接拒绝，不进入子进程。
func TestValidateReadOnlySQLLengthLimit(t *testing.T) {
	long := "SELECT '" + strings.Repeat("a", maxSQLBytes) + "'"
	perr := ValidateReadOnlySQL(long)
	if perr == nil || !strings.Contains(perr.Message, "64 KiB") {
		t.Fatalf("超长 SQL 未被拒绝: %v", perr)
	}
}

// TestSanitizeKeepsStructure 去字面量后仍能识别真实分号，字面量里的分号不误判。
func TestSanitizeKeepsStructure(t *testing.T) {
	sanitized, err := sanitizeSQL("SELECT ';' AS a, \"b;c\" AS b, $$;$$ AS c")
	if err != nil {
		t.Fatalf("sanitize 失败: %v", err)
	}
	if strings.Contains(sanitized, ";") {
		t.Fatalf("字面量里的分号未被去掉: %q", sanitized)
	}
}
