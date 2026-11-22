package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestDispatchMethods 两个 pg 方法经 dispatch 返回结构化结果。
func TestDispatchMethods(t *testing.T) {
	cases := []struct {
		method string
		params string
		want   string
	}{
		{"db-postgres.instances", `{}`, `"count":0`},
		{"db-postgres.instances", `{"filter":"main"}`, `"applied":false`},
		{"db-postgres.connections", `{"instance":"main"}`, `"instance":"main"`},
		{"db-postgres.connections", `{"instance":"main","database":"app"}`, `"database":"app"`},
	}
	for _, c := range cases {
		req := &pluginapi.Request{
			Type: pluginapi.TypeRequest, ID: "id", ReqID: "r",
			Method: c.method, Params: json.RawMessage(c.params),
		}
		resp, shutdown := dispatch(req, testConfig(), testLogger())
		if shutdown {
			t.Fatalf("%s 不应触发 shutdown", c.method)
		}
		if resp.Error != nil {
			t.Fatalf("%s 意外错误: %v", c.method, resp.Error)
		}
		if !strings.Contains(string(resp.Result), c.want) {
			t.Fatalf("%s 结果=%s，未含 %q", c.method, resp.Result, c.want)
		}
	}
}

// TestDispatchReject 非法参数与未知方法经 dispatch 返回协议错误。
func TestDispatchReject(t *testing.T) {
	cases := []struct {
		method string
		params string
		code   string
	}{
		{"db-postgres.instances", `{"filter":"../etc"}`, pluginapi.E_INVALID},
		{"db-postgres.connections", `{}`, pluginapi.E_INVALID},
		{"db-postgres.connections", `{"instance":"Bad Name"}`, pluginapi.E_INVALID},
		{"db-postgres.exec", `{}`, pluginapi.E_NOTFOUND},
	}
	for _, c := range cases {
		req := &pluginapi.Request{
			Type: pluginapi.TypeRequest, ID: "id", ReqID: "r",
			Method: c.method, Params: json.RawMessage(c.params),
		}
		resp, _ := dispatch(req, testConfig(), testLogger())
		if resp.Error == nil || resp.Error.Code != c.code {
			t.Fatalf("%s 错误=%v，期望 %q", c.method, resp.Error, c.code)
		}
		if resp.Result != nil {
			t.Fatalf("%s 出错时不应带结果", c.method)
		}
	}
}

// TestPingResult kore.ping 返回 ready 状态。
func TestPingResult(t *testing.T) {
	req := &pluginapi.Request{Type: pluginapi.TypeRequest, ID: "id", ReqID: "r", Method: "kore.ping"}
	resp, _ := dispatch(req, testConfig(), testLogger())
	if !strings.Contains(string(resp.Result), `"status":"ready"`) {
		t.Fatalf("ping 结果=%s", resp.Result)
	}
}

// TestDispatchShutdown shutdown 返回 ok 并置位退出标记。
func TestDispatchShutdown(t *testing.T) {
	req := &pluginapi.Request{Type: pluginapi.TypeRequest, ID: "id", ReqID: "r", Method: "kore.shutdown"}
	resp, shutdown := dispatch(req, testConfig(), testLogger())
	if !shutdown {
		t.Fatal("kore.shutdown 应触发退出标记")
	}
	if !strings.Contains(string(resp.Result), `"ok":true`) {
		t.Fatalf("shutdown 结果=%s", resp.Result)
	}
}
