package pgop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 本文件是 P0 SIGSEGV 的回归闸门（r20-e2edb 真机 E2E 抓到）。
//
// 崩溃链路：Grant/ChangePassword 成功路径 return c.execDDL(...) 把 (*OpError)(nil)
// 装箱进 error 接口 → err != nil 为真 → classify 的 errors.As 命中并写入 nil 指针
// → 解引用 opErr.Code 时 SIGSEGV。静态单测用裸 nil 假执行器抓不到，必须显式构造
// 类型化 nil 才能真正锁死这条回归。

// typedNilExec 让两个「返回 error 的写方法」按需返回类型化 nil。
// 嵌入 fakeExec 复用记录逻辑，只覆盖 Grant / ChangePassword。
type typedNilExec struct {
	*fakeExec
	grantTypedNil    bool
	passwordTypedNil bool
}

func (t *typedNilExec) Grant(instance InstanceConfig, user, database string, privileges []string, withGrantOption bool) error {
	t.gotAdmin = append(t.gotAdmin, "grant:"+user+":"+database+":"+strings.Join(privileges, ","))
	if t.grantTypedNil {
		// 崩溃现场：error 接口持有 (*OpError)(nil)，err != nil 但底层指针为 nil。
		return error((*OpError)(nil))
	}
	return nil
}

func (t *typedNilExec) ChangePassword(instance InstanceConfig, user, password string) error {
	t.gotAdmin = append(t.gotAdmin, "changePassword:"+user+":"+password)
	if t.passwordTypedNil {
		return error((*OpError)(nil))
	}
	return nil
}

// TestGrantTypedNilNoPanic 类型化 nil 不得崩溃，且 grant 成功结果照常返回。
func TestGrantTypedNilNoPanic(t *testing.T) {
	exec := &typedNilExec{fakeExec: &fakeExec{}, grantTypedNil: true}
	params := `{"instance":"default","user":"app_user","database":"kore","privileges":["connect"],"with_grant_option":true}`
	result, perr := Call("db-postgres.grant", json.RawMessage(params), testConfig(), exec)
	if perr != nil {
		t.Fatalf("类型化 nil 不应被当作错误: %v", perr)
	}
	var parsed GrantResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if !parsed.Granted || parsed.User != "app_user" || parsed.Database != "kore" ||
		strings.Join(parsed.Privileges, ",") != "CONNECT" || !parsed.WithGrantOption {
		t.Fatalf("grant 成功结果异常: %s", result)
	}
}

// TestChangePasswordTypedNilNoPanic 改密成功路径返回类型化 nil 不得崩溃，回包仍不含口令。
func TestChangePasswordTypedNilNoPanic(t *testing.T) {
	const secret = "n3w-s3cret-pw"
	exec := &typedNilExec{fakeExec: &fakeExec{}, passwordTypedNil: true}
	params := `{"instance":"default","user":"app_user","password":"` + secret + `"}`
	result, perr := Call("db-postgres.changePassword", json.RawMessage(params), testConfig(), exec)
	if perr != nil {
		t.Fatalf("类型化 nil 不应被当作错误: %v", perr)
	}
	if strings.Contains(string(result), secret) {
		t.Fatalf("响应回显了口令: %s", result)
	}
	if !strings.Contains(string(result), `"changed":true`) || !strings.Contains(string(result), `"user":"app_user"`) {
		t.Fatalf("改密成功结果异常: %s", result)
	}
}

// TestClassifyTypedNil 直接把类型化 nil 送进 classify：裸 nil 与装箱 nil 都不得 panic。
func TestClassifyTypedNil(t *testing.T) {
	if got := classify(nil); got != nil {
		t.Fatalf("classify(nil)=%v，期望 nil", got)
	}
	if got := classify(error((*OpError)(nil))); got != nil {
		t.Fatalf("classify((*OpError)(nil))=%v，期望 nil", got)
	}
	if got := classify(error((*FaultError)(nil))); got != nil {
		t.Fatalf("classify((*FaultError)(nil))=%v，期望 nil", got)
	}
	// 正常的非 nil 错误仍要被正确分类，兜底不能吞错误。
	got := classify(opErrorf(pluginapi.E_NOTFOUND, "不存在"))
	if got == nil || got.Code != pluginapi.E_NOTFOUND || got.Message != "不存在" {
		t.Fatalf("classify(OpError)=%+v，期望 E_NOTFOUND/不存在", got)
	}
}

// TestErrorTextTypedNil errorText 对类型化 nil 也不得 panic。
func TestErrorTextTypedNil(t *testing.T) {
	if got := errorText(error((*OpError)(nil))); got != "" {
		t.Fatalf("errorText((*OpError)(nil))=%q，期望空串", got)
	}
	if got := errorText(opErrorf(pluginapi.E_UPSTREAM, "上游失败")); got != "上游失败" {
		t.Fatalf("errorText(OpError)=%q，期望 上游失败", got)
	}
}

// TestWriteMethodsRealErrorStillMapped 是副作用闸门：六个写方法的执行面判断改成
// !isNilError 后，真实非 nil *OpError 必须仍被 classify 正常映射，不能被当成成功吞掉。
// 对应 r20-e2edb 的 B 类失败路径（exec 真实失败），与 A 类参数校验失败一起构成
// 「修类型化 nil 没破坏正常错误路径」的证据。
func TestWriteMethodsRealErrorStillMapped(t *testing.T) {
	const msg = "执行面真实失败"
	realErr := opErrorf(pluginapi.E_UPSTREAM, msg)
	cases := map[string]string{
		"db-postgres.createDatabase": `{"instance":"default","name":"kore"}`,
		"db-postgres.dropDatabase":   `{"instance":"default","name":"kore","confirm":true}`,
		"db-postgres.createUser":     `{"instance":"default","user":"app_user"}`,
		"db-postgres.dropUser":       `{"instance":"default","user":"app_user","confirm":true}`,
		"db-postgres.grant":          `{"instance":"default","user":"app_user","database":"kore","privileges":["CONNECT"]}`,
		"db-postgres.changePassword": `{"instance":"default","user":"app_user","password":"x"}`,
	}
	for method, params := range cases {
		result, perr := Call(method, json.RawMessage(params), testConfig(), &fakeExec{err: realErr})
		if perr == nil || perr.Code != pluginapi.E_UPSTREAM || perr.Message != msg {
			t.Fatalf("%s 真实错误未被映射: %+v", method, perr)
		}
		if result != nil {
			t.Fatalf("%s 出错时不应带结果: %s", method, result)
		}
	}
}

// TestWriteMethodsParamFailureBeforeExec 覆盖 A 类：参数校验失败在 exec 之前返回，
// 不进 classify、不触达执行面——加固后的 isNilError 不改变这条既有防线。
func TestWriteMethodsParamFailureBeforeExec(t *testing.T) {
	exec := &fakeExec{}
	cfg := testConfig()
	grant := `{"instance":"default","user":"app_user","database":"kore","privileges":["SUPERUSER"]}`
	if _, perr := Call("db-postgres.grant", json.RawMessage(grant), cfg, exec); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("非法权限应 E_INVALID: %v", perr)
	}
	passwd := `{"instance":"default","user":"postgres","password":"x"}`
	if _, perr := Call("db-postgres.changePassword", json.RawMessage(passwd), cfg, exec); perr == nil || perr.Code != pluginapi.E_UNAUTHORIZED {
		t.Fatalf("保留账号改密应 E_UNAUTHORIZED: %v", perr)
	}
	if len(exec.gotAdmin) != 0 {
		t.Fatalf("参数校验失败仍触达执行面: %v", exec.gotAdmin)
	}
}
