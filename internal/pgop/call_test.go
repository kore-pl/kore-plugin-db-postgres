package pgop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const testScope = "/var/lib/kore/db-postgres"

// faultExec 按注入类别让两个方法都失败，模拟运行时故障。
type faultExec struct{ kind FaultKind }

func (f faultExec) Instances(scope, filter string) ([]Instance, error) {
	return nil, NewFaultError(f.kind, "instances 执行面故障")
}

func (f faultExec) Connections(scope, instance, database string) ([]Connection, error) {
	return nil, NewFaultError(f.kind, "connections 执行面故障")
}

// TestCallNoFault 两个方法在无故障时返回非空结构化结果。
func TestCallNoFault(t *testing.T) {
	cases := []struct {
		method string
		params string
		want   string
	}{
		{"pg.instances", `{}`, `"count":0`},
		{"instances", `{"filter":"main"}`, `"applied":false`},
		{"pg.connections", `{"instance":"main"}`, `"instance":"main"`},
		{"connections", `{"instance":"main","database":"app"}`, `"database":"app"`},
	}
	for _, c := range cases {
		result, perr := Call(c.method, json.RawMessage(c.params), testScope, nil)
		if perr != nil {
			t.Fatalf("%s 无故障意外错误: %v", c.method, perr)
		}
		if !strings.Contains(string(result), c.want) {
			t.Fatalf("%s 结果=%s，未含 %q", c.method, result, c.want)
		}
	}
}

// TestCallInvalid 参数校验：非法逻辑名 / 缺必填 / 作用域穿越均返回 E_INVALID。
func TestCallInvalid(t *testing.T) {
	cases := []struct {
		method string
		params string
		scope  string
	}{
		{"pg.instances", `{"filter":"../etc"}`, testScope},
		{"pg.connections", `{}`, testScope},
		{"pg.connections", `{"instance":"Bad Name"}`, testScope},
		{"pg.instances", `{}`, "/var/lib/../escape"},
	}
	for _, c := range cases {
		result, perr := Call(c.method, json.RawMessage(c.params), c.scope, nil)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("%s params=%s scope=%s 错误=%v，期望 E_INVALID", c.method, c.params, c.scope, perr)
		}
		if result != nil {
			t.Fatalf("%s 出错时不应带结果: %s", c.method, result)
		}
	}
}

// TestCallFault 两类故障经 Call 映射为 E_UPSTREAM，且错误信息含类别关键词。
func TestCallFault(t *testing.T) {
	cases := []struct {
		kind FaultKind
		hint string
	}{
		{FaultKill, "信号 9"},
		{FaultTimeout, "超时"},
	}
	methods := []struct {
		method string
		params string
	}{
		{"pg.instances", `{}`},
		{"pg.connections", `{"instance":"main"}`},
	}
	for _, c := range cases {
		for _, m := range methods {
			result, perr := Call(m.method, json.RawMessage(m.params), testScope, faultExec{kind: c.kind})
			if perr == nil || perr.Code != pluginapi.E_UPSTREAM {
				t.Fatalf("%s 注入 %s 错误=%v，期望 E_UPSTREAM", m.method, c.kind, perr)
			}
			if result != nil {
				t.Fatalf("%s 出错时不应带结果: %s", m.method, result)
			}
			if !strings.Contains(perr.Message, c.hint) {
				t.Fatalf("%s 错误信息 %q 未含 %q", m.method, perr.Message, c.hint)
			}
		}
	}
}

// TestCallUnknown 未知方法返回 E_NOTFOUND。
func TestCallUnknown(t *testing.T) {
	_, perr := Call("pg.exec", json.RawMessage(`{}`), testScope, nil)
	if perr == nil || perr.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("错误=%v，期望 E_NOTFOUND", perr)
	}
}
