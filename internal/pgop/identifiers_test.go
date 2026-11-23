package pgop

import (
	"strings"
	"testing"
)

// TestQuoteIdentifier 标识符引用二次转义：内部双引号双写，外层仍是 "..."。
func TestQuoteIdentifier(t *testing.T) {
	cases := map[string]string{
		"kore":        `"kore"`,
		"Kore_Test01": `"Kore_Test01"`,
		`a"b`:         `"a""b"`,
		`a""b`:        `"a""""b"`,
	}
	for in, want := range cases {
		if got := quoteIdentifier(in); got != want {
			t.Fatalf("quoteIdentifier(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// TestQuoteLiteral 单引号字面量：单引号双写，反斜杠保持原样（standard_conforming_strings=on）。
func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"secret":  `'secret'`,
		"it's":    `'it''s'`,
		`a\'b`:    `'a\''b'`, // 反斜杠不转义，只动单引号
		"":        `''`,
		"中文密码ABC": `'中文密码ABC'`,
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Fatalf("quoteLiteral(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// TestProtectedDatabases 系统库保护忽略大小写，普通库不误伤。
func TestProtectedDatabases(t *testing.T) {
	for _, name := range []string{"postgres", "Postgres", "POSTGRES", "template0", "template1", " Template1 "} {
		if !IsProtectedDatabase(name) {
			t.Fatalf("%q 应判为系统库", name)
		}
	}
	for _, name := range []string{"kore", "app_db", "template2", "postgresql"} {
		if IsProtectedDatabase(name) {
			t.Fatalf("%q 不应判为系统库", name)
		}
	}
}

// TestProtectedRoleNames pg_ 前缀与 postgres 是硬保护，大小写变体不能绕过。
func TestProtectedRoleNames(t *testing.T) {
	for _, name := range []string{"postgres", "POSTGRES", "pg_monitor", "PG_signal_backend", "pg_"} {
		if !IsProtectedRoleName(name) {
			t.Fatalf("%q 应判为保留角色", name)
		}
	}
	for _, name := range []string{"kore_app", "app_user", "postgre", "pgsql"} {
		if IsProtectedRoleName(name) {
			t.Fatalf("%q 不应判为保留角色", name)
		}
	}
}

// TestValidateRoleName 用户名白名单：注入字符与超长一律拒绝。
func TestValidateRoleName(t *testing.T) {
	for _, name := range []string{"kore_app", "AppUser2", "a", strings.Repeat("x", 63)} {
		if err := ValidateRoleName(name); err != nil {
			t.Fatalf("%q 应通过校验: %v", name, err)
		}
	}
	for _, name := range []string{"", "a b", "a-b", `a"; DROP ROLE x; --`, "用户", strings.Repeat("x", 64)} {
		if err := ValidateRoleName(name); err == nil {
			t.Fatalf("%q 应被拒绝", name)
		}
	}
}

// TestNormalizePrivileges 权限白名单归一化：大小写/TEMP 别名/去重，非法值拒绝。
func TestNormalizePrivileges(t *testing.T) {
	got, perr := normalizePrivileges([]string{"connect", "TEMP", "CONNECT", "all"})
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if strings.Join(got, ",") != "CONNECT,TEMPORARY,ALL" {
		t.Fatalf("归一化结果=%v，期望 CONNECT,TEMPORARY,ALL", got)
	}
	for _, bad := range [][]string{
		nil, {}, {"SUPERUSER"}, {"ALL PRIVILEGES"}, {"GRANT"}, {"CONNECT;"},
	} {
		if _, perr := normalizePrivileges(bad); perr == nil {
			t.Fatalf("%v 应被拒绝", bad)
		} else if perr.Code != "E_INVALID" {
			t.Fatalf("%v 错误码=%q，期望 E_INVALID", bad, perr.Code)
		}
	}
}

// TestAdminSQLBuilders SQL 构造：标识符带双引号、口令在字面量里、权限来自白名单。
func TestAdminSQLBuilders(t *testing.T) {
	if got := createDatabaseSQL("kore", ""); got != `CREATE DATABASE "kore";`+"\n" {
		t.Fatalf("createDatabaseSQL=%q", got)
	}
	if got := createDatabaseSQL("kore", "app_user"); !strings.Contains(got, `OWNER "app_user"`) {
		t.Fatalf("owner 未写入: %q", got)
	}
	if got := dropDatabaseSQL("kore"); got != `DROP DATABASE "kore";`+"\n" {
		t.Fatalf("dropDatabaseSQL=%q", got)
	}
	create := createRoleSQL("app_user", "it's-secret", true, false)
	for _, want := range []string{`CREATE ROLE "app_user"`, "LOGIN", "CREATEDB", `PASSWORD 'it''s-secret'`} {
		if !strings.Contains(create, want) {
			t.Fatalf("createRoleSQL 缺 %q: %q", want, create)
		}
	}
	if strings.Contains(create, "SUPERUSER") {
		t.Fatalf("建角色不应带 SUPERUSER: %q", create)
	}
	if got := createRoleSQL("app_user", "", false, false); strings.Contains(got, "PASSWORD") {
		t.Fatalf("空口令不应写 PASSWORD 子句: %q", got)
	}
	grant := grantSQL("app_user", "kore", []string{"CONNECT", "TEMPORARY"}, false)
	if grant != `GRANT CONNECT, TEMPORARY ON DATABASE "kore" TO "app_user";`+"\n" {
		t.Fatalf("grantSQL=%q", grant)
	}
	if !strings.Contains(grantSQL("app_user", "kore", []string{"CONNECT"}, true), "WITH GRANT OPTION") {
		t.Fatal("with_grant_option 未写入")
	}
	alter := alterPasswordSQL("app_user", "new'pw")
	if alter != `ALTER ROLE "app_user" WITH PASSWORD 'new''pw';`+"\n" {
		t.Fatalf("alterPasswordSQL=%q", alter)
	}
}
