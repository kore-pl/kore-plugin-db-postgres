package pgop

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateWriteSQLAllow 合法写语句必须放行，并返回正确分级。
func TestValidateWriteSQLAllow(t *testing.T) {
	cases := []struct {
		name        string
		sql         string
		confirm     bool
		confirmText string
		wantKind    string
	}{
		{"update 带 confirm", "UPDATE users SET name='x' WHERE id=1", true, "", ExecKindData},
		{"insert 带 confirm", "INSERT INTO logs (msg) VALUES ('a;b')", true, "", ExecKindData},
		{"insert returning 带 confirm", "INSERT INTO logs (msg) VALUES ('x') RETURNING id", true, "", ExecKindData},
		{"delete 带 confirm", "DELETE FROM logs WHERE id < 10;", true, "", ExecKindData},
		{"merge 带 confirm", "MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v", true, "", ExecKindData},
		{"drop table 手打短语", "DROP TABLE logs", true, "drop table", ExecKindDDL},
		{"drop table 短语容忍空白", "DROP   TABLE logs", true, "  DROP TABLE ", ExecKindDDL},
		{"alter table 手打短语", "ALTER TABLE users ADD COLUMN age INT", true, "ALTER TABLE", ExecKindDDL},
		{"truncate 手打短语", "TRUNCATE TABLE logs", true, "TRUNCATE TABLE", ExecKindDDL},
		{"create database 手打短语", "CREATE DATABASE kore_x", true, "CREATE DATABASE", ExecKindDDL},
		{"grant 手打短语", "GRANT SELECT ON TABLE logs TO app_user", true, "GRANT SELECT", ExecKindDDL},
		{"reindex 手打短语", "REINDEX TABLE logs", true, "REINDEX TABLE", ExecKindDDL},
		{"vacuum 括号前缀只认首关键字", "VACUUM (ANALYZE) logs", true, "VACUUM", ExecKindDDL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, perr := ValidateWriteSQL(c.sql, c.confirm, c.confirmText)
			if perr != nil {
				t.Fatalf("期望放行，实际拒绝: %s", perr.Message)
			}
			if kind != c.wantKind {
				t.Fatalf("分级 = %q，期望 %q", kind, c.wantKind)
			}
		})
	}
}

// TestValidateWriteSQLReject 越权 / 未确认 / 多语句 / 侧信道必须 E_INVALID，且错误信息可执行。
func TestValidateWriteSQLReject(t *testing.T) {
	long := "UPDATE t SET a='" + strings.Repeat("x", maxSQLBytes) + "'"
	cases := []struct {
		name        string
		sql         string
		confirm     bool
		confirmText string
		wantHint    string
	}{
		{"空语句", "   ", true, "", "不能为空"},
		{"读语句走 query", "SELECT 1", true, "", "db-postgres.query"},
		{"with 走 query", "WITH x AS (SELECT 1) SELECT * FROM x", true, "", "db-postgres.query"},
		{"show 走 query", "SHOW ALL", true, "", "db-postgres.query"},
		{"未确认", "DELETE FROM t", false, "", "confirm=true"},
		{"ddl 缺确认短语", "DROP TABLE t", true, "", "confirm_text"},
		{"ddl 确认短语不匹配", "DROP TABLE t", true, "DROP", "不匹配"},
		{"多语句", "DELETE FROM t; DELETE FROM u", true, "", "一条写语句"},
		{"NUL 字节", "DELETE FROM t\x00", true, "", "NUL"},
		{"超 64KiB", long, true, "", "64 KiB"},
		{"反斜杠命令", `DELETE FROM t \g`, true, "", "格式有误"},
		{"块注释内多语句", "DELETE FROM t /* x; y */", true, "", ""}, // 清洗后单条，应放行；见 allow 用例
		{"非白名单前缀", "COPY t FROM '/etc/passwd'", true, "", "写入白名单拒绝"},
		{"copy program 侧信道", "COPY t FROM PROGRAM 'id'", true, "", "写入白名单拒绝"},
		{"副作用函数 pg_sleep", "UPDATE t SET a=pg_sleep(10)", true, "", "副作用"},
		{"副作用函数 dblink", "UPDATE t SET a=dblink_exec('host=local', 'DROP TABLE x')", true, "", "副作用"},
		{"空关键字", "'abc'", true, "", "不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, perr := ValidateWriteSQL(c.sql, c.confirm, c.confirmText)
			if c.name == "块注释内多语句" {
				// 块注释内的分号不算多语句：清洗后是合法单条 DELETE，应放行。
				if perr != nil || kind != ExecKindData {
					t.Fatalf("期望放行，实际 kind=%q perr=%v", kind, perr)
				}
				return
			}
			if perr == nil {
				t.Fatalf("期望拒绝，实际放行: %q", c.sql)
			}
			if perr.Code != "E_INVALID" {
				t.Fatalf("错误码 = %q，期望 E_INVALID", perr.Code)
			}
			if !strings.Contains(perr.Message, c.wantHint) {
				t.Fatalf("错误信息 %q 未包含 %q", perr.Message, c.wantHint)
			}
		})
	}
}

// TestCallExec 校验：校验失败不触达执行面；合法写语句带回分级与结果形状。
func TestCallExec(t *testing.T) {
	fake := &fakeExec{query: QueryResult{Columns: []string{"id"}, Rows: []json.RawMessage{json.RawMessage(`{"id":1}`)}}}
	cfg := testConfig()

	// 未确认：校验失败，不触达执行面。
	_, perr := callExec(json.RawMessage(`{"instance":"default","sql":"UPDATE t SET a=1"}`), cfg, fake)
	if perr == nil {
		t.Fatal("未确认的写语句应被拒绝")
	}
	if fake.gotSQL != "" {
		t.Fatalf("拒绝路径不应触达执行面，实际 sql=%q", fake.gotSQL)
	}

	// 合法写语句：带回分级与 RETURNING 行，row_count=len(rows)。
	raw, perr := callExec(json.RawMessage(`{"instance":"default","sql":"UPDATE t SET a=1 RETURNING id","confirm":true}`), cfg, fake)
	if perr != nil {
		t.Fatalf("合法写语句应放行: %s", perr.Message)
	}
	var out ExecResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("结果解码失败: %v", err)
	}
	if out.Kind != ExecKindData || !out.Executed || out.Instance != "default" {
		t.Fatalf("结果形状不符: %+v", out)
	}
	if out.RowCount != 1 || len(out.Rows) != 1 {
		t.Fatalf("row_count 不符: %+v", out)
	}
	if fake.gotSQL != "UPDATE t SET a=1 RETURNING id" {
		t.Fatalf("执行面收到的 SQL 不符: %q", fake.gotSQL)
	}

	// ddl 语句：confirm=true 但缺 confirm_text 仍拒绝。
	_, perr = callExec(json.RawMessage(`{"instance":"default","sql":"DROP TABLE t","confirm":true}`), cfg, fake)
	if perr == nil || !strings.Contains(perr.Message, "confirm_text") {
		t.Fatalf("ddl 缺确认短语应拒绝并给可执行提示，实际: %v", perr)
	}

	// 无 system:admin：写方法一律 E_UNAUTHORIZED（纵深防御）。
	noAdmin := cfg
	noAdmin.Admin = false
	_, perr = callExec(json.RawMessage(`{"instance":"default","sql":"UPDATE t SET a=1","confirm":true}`), noAdmin, fake)
	if perr == nil || perr.Code != "E_UNAUTHORIZED" {
		t.Fatalf("无 system:admin 应 E_UNAUTHORIZED，实际: %v", perr)
	}
}
