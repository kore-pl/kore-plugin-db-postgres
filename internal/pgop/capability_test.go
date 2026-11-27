package pgop

import (
	"encoding/json"
	"os"
	"testing"
)

// capabilityTestExpected 是本仓 method_capabilities 的表驱动真源（新增方法必须同步）。
// exec 声明 system:admin：写能力只对管理员开放，内核据此做授权与审计。
var capabilityTestExpected = map[string]string{
	"db-postgres.config.get":     "system:read",
	"db-postgres.instances":      "system:read",
	"db-postgres.databases":      "system:read",
	"db-postgres.query":          "system:read",
	"db-postgres.exec":           "system:admin",
	"db-postgres.status":         "system:read",
	"db-postgres.slowlog":        "system:read",
	"db-postgres.config.set":     "system:admin",
	"db-postgres.createDatabase": "system:admin",
	"db-postgres.dropDatabase":   "system:admin",
	"db-postgres.createUser":     "system:admin",
	"db-postgres.dropUser":       "system:admin",
	"db-postgres.grant":          "system:admin",
	"db-postgres.changePassword": "system:admin",
	"db-postgres.backup":         "system:admin",
	"db-postgres.restore":        "system:admin",
	"db-postgres.backupList":     "system:read",
	"db-postgres.backupDelete":   "system:admin",
	"db-postgres.backupDownload": "system:admin",
}

var capabilityTestPrivileged = map[string]bool{
	"system:read": true, "system:admin": true,
	"net:read": true, "net:write": true,
	"proc:read": true, "proc:write": true,
}

// TestMethodCapabilitiesConsistency 是方法级能力一致性门禁：
// 声明表 == info.json，取值合法，且每个声明的方法都能被 businessVerb 分发。
func TestMethodCapabilitiesConsistency(t *testing.T) {
	var info struct {
		ID                 string            `json:"id"`
		Version            string            `json:"version"`
		Capabilities       []string          `json:"capabilities"`
		MethodCapabilities map[string]string `json:"method_capabilities"`
	}
	data, err := os.ReadFile("../../info.json")
	if err != nil {
		t.Fatalf("读取 info.json 失败: %v", err)
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("解析 info.json 失败: %v", err)
	}

	if info.ID != "db-postgres" {
		t.Fatalf("info.id=%q，期望 db-postgres", info.ID)
	}
	if len(info.Capabilities) == 0 || info.Capabilities[0] != "system:admin" {
		t.Fatalf("插件级 capabilities 不符: %v", info.Capabilities)
	}
	if len(info.MethodCapabilities) != len(capabilityTestExpected) {
		t.Fatalf("method_capabilities 条数=%d，期望 %d", len(info.MethodCapabilities), len(capabilityTestExpected))
	}
	for method, want := range capabilityTestExpected {
		got, ok := info.MethodCapabilities[method]
		if !ok {
			t.Fatalf("method_capabilities 缺 %q", method)
		}
		if got != want {
			t.Fatalf("method_capabilities[%q]=%q，期望 %q", method, got, want)
		}
		if !capabilityTestPrivileged[got] {
			t.Fatalf("method_capabilities[%q]=%q 不是 6 项特权能力之一", method, got)
		}
		if method == "db-postgres.config.get" || method == "db-postgres.config.set" {
			continue // 配置契约由 main 包分发，不经 pgop.businessVerb
		}
		if verb, known := businessVerb(method); !known || verb != method[len("db-postgres."):] {
			t.Fatalf("声明了 %q 但 businessVerb 不认识", method)
		}
	}
}
