package pgop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件覆盖写方法的「方法层防线」：能力门控、confirm、系统库/超级用户保护、
// 标识符注入拒绝、权限白名单、口令不回显。执行面一律用 fakeExec，不连数据库。

// TestWriteMethodsRequireAdmin 未声明 system:admin 时六个写方法全部 E_UNAUTHORIZED，
// 且执行面一次都不被调用（拒绝发生在触达数据库之前）。
func TestWriteMethodsRequireAdmin(t *testing.T) {
	cfg := testConfig()
	cfg.Admin = false
	cases := map[string]string{
		"db-postgres.createDatabase": `{"instance":"default","name":"kore"}`,
		"db-postgres.dropDatabase":   `{"instance":"default","name":"kore","confirm":true}`,
		"db-postgres.createUser":     `{"instance":"default","user":"app_user"}`,
		"db-postgres.dropUser":       `{"instance":"default","user":"app_user","confirm":true}`,
		"db-postgres.grant":          `{"instance":"default","user":"app_user","database":"kore","privileges":["CONNECT"]}`,
		"db-postgres.changePassword": `{"instance":"default","user":"app_user","password":"x"}`,
	}
	for method, params := range cases {
		exec := &fakeExec{}
		_, perr := Call(method, json.RawMessage(params), cfg, exec)
		if perr == nil || perr.Code != pluginapi.E_UNAUTHORIZED {
			t.Fatalf("%s 无 system:admin 时错误=%v，期望 E_UNAUTHORIZED", method, perr)
		}
		if len(exec.gotAdmin) != 0 {
			t.Fatalf("%s 越权时仍调用了执行面: %v", method, exec.gotAdmin)
		}
	}
}

// TestIdentifierInjectionRejected 标识符注入在方法层就拒绝，不进入执行面。
func TestIdentifierInjectionRejected(t *testing.T) {
	payloads := []string{
		`kore"; DROP DATABASE x; --`,
		`'; DROP DATABASE x; --`,
		`kore db`,
		`kore-db`,
		`kore;drop`,
		`用户`,
		strings.Repeat("a", 64),
		`kore\n`,
	}
	// 每条用例把注入载荷填进对应字段，其余字段用合法值。
	type testCase struct {
		method string
		field  string
		extra  string
	}
	cases := []testCase{
		{"db-postgres.createDatabase", "name", `"owner":""`},
		{"db-postgres.dropDatabase", "name", `"confirm":true`},
		{"db-postgres.createUser", "user", `"password":"x"`},
		{"db-postgres.dropUser", "user", `"confirm":true`},
		{"db-postgres.grant", "user", `"database":"kore","privileges":["CONNECT"]`},
		{"db-postgres.grant", "database", `"user":"app_user","privileges":["CONNECT"]`},
	}
	for _, c := range cases {
		for _, payload := range payloads {
			quoted, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("编码载荷失败: %v", err)
			}
			params := `{"instance":"default","` + c.field + `":` + string(quoted) + `,` + c.extra + `}`
			exec := &fakeExec{}
			_, perr := Call(c.method, json.RawMessage(params), testConfig(), exec)
			if perr == nil || perr.Code != pluginapi.E_INVALID {
				t.Fatalf("%s 字段 %s=%q 错误=%v，期望 E_INVALID", c.method, c.field, payload, perr)
			}
			if len(exec.gotAdmin) != 0 {
				t.Fatalf("%s 注入 %q 时仍调用了执行面", c.method, payload)
			}
		}
	}
}

// TestDropDatabaseProtection 系统库先于 confirm 拒绝：任何参数组合都不能删。
func TestDropDatabaseProtection(t *testing.T) {
	for _, name := range []string{"postgres", "Postgres", "template0", "template1"} {
		for _, confirm := range []bool{true, false} {
			params := `{"instance":"default","name":"` + name + `","confirm":` + boolText(confirm) + `}`
			exec := &fakeExec{}
			_, perr := Call("db-postgres.dropDatabase", json.RawMessage(params), testConfig(), exec)
			if perr == nil || perr.Code != pluginapi.E_UNAUTHORIZED {
				t.Fatalf("删系统库 %q(confirm=%v) 错误=%v，期望 E_UNAUTHORIZED", name, confirm, perr)
			}
			if len(exec.gotAdmin) != 0 {
				t.Fatalf("删系统库 %q 仍调用了执行面", name)
			}
		}
	}
	// 普通库缺 confirm → E_INVALID；带 confirm → 放行到执行面。
	exec := &fakeExec{}
	if _, perr := Call("db-postgres.dropDatabase", json.RawMessage(`{"instance":"default","name":"kore"}`), testConfig(), exec); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("缺 confirm 应 E_INVALID，实际 %v", perr)
	}
	if len(exec.gotAdmin) != 0 {
		t.Fatal("缺 confirm 仍调用了执行面")
	}
	if _, perr := Call("db-postgres.dropDatabase", json.RawMessage(`{"instance":"default","name":"kore","confirm":true}`), testConfig(), exec); perr != nil {
		t.Fatalf("合法删库应放行: %v", perr)
	}
}

// TestDropUserProtection 保留账号（pg_ 前缀 / postgres）先于 confirm 拒绝。
func TestDropUserProtection(t *testing.T) {
	for _, user := range []string{"postgres", "pg_monitor", "PG_signal_backend"} {
		exec := &fakeExec{}
		params := `{"instance":"default","user":"` + user + `","confirm":true}`
		_, perr := Call("db-postgres.dropUser", json.RawMessage(params), testConfig(), exec)
		if perr == nil || perr.Code != pluginapi.E_UNAUTHORIZED {
			t.Fatalf("删保留账号 %q 错误=%v，期望 E_UNAUTHORIZED", user, perr)
		}
		if len(exec.gotAdmin) != 0 {
			t.Fatalf("删保留账号 %q 仍调用了执行面", user)
		}
	}
	exec := &fakeExec{}
	if _, perr := Call("db-postgres.dropUser", json.RawMessage(`{"instance":"default","user":"app_user"}`), testConfig(), exec); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("缺 confirm 应 E_INVALID，实际 %v", perr)
	}
	// 改密同样拒绝保留账号（防把自己锁在门外）。
	if _, perr := Call("db-postgres.changePassword",
		json.RawMessage(`{"instance":"default","user":"postgres","password":"x"}`), testConfig(), exec); perr == nil || perr.Code != pluginapi.E_UNAUTHORIZED {
		t.Fatalf("改 postgres 口令应 E_UNAUTHORIZED，实际 %v", perr)
	}
}

// TestGrantWhitelistAndPassthrough 权限白名单归一化后传给执行面，非法权限拒绝。
func TestGrantWhitelistAndPassthrough(t *testing.T) {
	exec := &fakeExec{}
	params := `{"instance":"default","user":"app_user","database":"kore","privileges":["connect","TEMP"],"with_grant_option":true}`
	result, perr := Call("db-postgres.grant", json.RawMessage(params), testConfig(), exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if len(exec.gotAdmin) != 1 || !strings.Contains(exec.gotAdmin[0], "grant:app_user:kore:CONNECT,TEMPORARY") {
		t.Fatalf("执行面收到权限异常: %v", exec.gotAdmin)
	}
	var parsed GrantResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if !parsed.Granted || !parsed.WithGrantOption || strings.Join(parsed.Privileges, ",") != "CONNECT,TEMPORARY" {
		t.Fatalf("结果异常: %s", result)
	}
	for _, bad := range []string{"SUPERUSER", "ALL PRIVILEGES", "GRANT", "CONNECT;"} {
		params := `{"instance":"default","user":"app_user","database":"kore","privileges":["` + bad + `"]}`
		if _, perr := Call("db-postgres.grant", json.RawMessage(params), testConfig(), &fakeExec{}); perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("权限 %q 应 E_INVALID，实际 %v", bad, perr)
		}
	}
}

// TestChangePasswordNoEcho 改密结果是白名单字段，口令绝不出现在响应里。
func TestChangePasswordNoEcho(t *testing.T) {
	const secret = "n3w-s3cret-pw"
	exec := &fakeExec{}
	params := `{"instance":"default","user":"app_user","password":"` + secret + `"}`
	result, perr := Call("db-postgres.changePassword", json.RawMessage(params), testConfig(), exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if strings.Contains(string(result), secret) {
		t.Fatalf("响应回显了口令: %s", result)
	}
	if !strings.Contains(string(result), `"changed":true`) || !strings.Contains(string(result), `"user":"app_user"`) {
		t.Fatalf("结果字段异常: %s", result)
	}
	if len(exec.gotAdmin) != 1 || exec.gotAdmin[0] != "changePassword:app_user:"+secret {
		t.Fatalf("执行面未收到改密参数（或形状异常）: %v", exec.gotAdmin)
	}
}

// TestCreateSemantics created=false 表示「已存在、未改动」，不是错误。
func TestCreateSemantics(t *testing.T) {
	exec := &fakeExec{created: false}
	result, perr := Call("db-postgres.createDatabase", json.RawMessage(`{"instance":"default","name":"kore"}`), testConfig(), exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if !strings.Contains(string(result), `"created":false`) {
		t.Fatalf("已存在库应回 created=false: %s", result)
	}
	if _, perr := Call("db-postgres.createUser", json.RawMessage(`{"instance":"default","user":"app_user","password":"x"}`), testConfig(), &fakeExec{}); perr != nil {
		t.Fatalf("建用户应放行: %v", perr)
	}
}

// TestWriteVerbsRegistered 六个写动词都已注册：缺参数回 E_INVALID，而不是 E_NOTFOUND。
func TestWriteVerbsRegistered(t *testing.T) {
	methods := []string{
		"db-postgres.createDatabase", "db-postgres.dropDatabase",
		"db-postgres.createUser", "db-postgres.dropUser",
		"db-postgres.grant", "db-postgres.changePassword",
	}
	for _, method := range methods {
		_, perr := Call(method, json.RawMessage(`{}`), testConfig(), &fakeExec{})
		if perr == nil {
			t.Fatalf("%s 空参数应报错", method)
		}
		if perr.Code == pluginapi.E_NOTFOUND {
			t.Fatalf("%s 未注册（E_NOTFOUND）", method)
		}
	}
}

// boolText 把 bool 转成 JSON 字面量。
func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
