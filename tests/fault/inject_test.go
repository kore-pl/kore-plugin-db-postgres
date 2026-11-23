package fault

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-db-postgres/internal/pgop"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// faultExecutor 在十一个真实方法上注入同一类故障，验证「故障 → 协议码」映射。
type faultExecutor struct{ kind pgop.FaultKind }

func (f faultExecutor) Instances([]pgop.InstanceConfig) ([]pgop.InstanceStatus, error) {
	return nil, pgop.NewFaultError(f.kind, "instances 执行面故障")
}

func (f faultExecutor) Databases(pgop.InstanceConfig, bool) ([]pgop.Database, error) {
	return nil, pgop.NewFaultError(f.kind, "databases 执行面故障")
}

func (f faultExecutor) Query(pgop.InstanceConfig, string, int) (pgop.QueryResult, error) {
	return pgop.QueryResult{}, pgop.NewFaultError(f.kind, "query 执行面故障")
}

func (f faultExecutor) Status(pgop.InstanceConfig) (pgop.StatusResult, error) {
	return pgop.StatusResult{}, pgop.NewFaultError(f.kind, "status 执行面故障")
}

func (f faultExecutor) CreateDatabase(pgop.InstanceConfig, string, string) (bool, error) {
	return false, pgop.NewFaultError(f.kind, "createDatabase 执行面故障")
}

func (f faultExecutor) DropDatabase(pgop.InstanceConfig, string) (bool, error) {
	return false, pgop.NewFaultError(f.kind, "dropDatabase 执行面故障")
}

func (f faultExecutor) CreateRole(pgop.InstanceConfig, string, string, bool, bool) (bool, error) {
	return false, pgop.NewFaultError(f.kind, "createUser 执行面故障")
}

func (f faultExecutor) DropRole(pgop.InstanceConfig, string) (bool, error) {
	return false, pgop.NewFaultError(f.kind, "dropUser 执行面故障")
}

func (f faultExecutor) Grant(pgop.InstanceConfig, string, string, []string, bool) error {
	return pgop.NewFaultError(f.kind, "grant 执行面故障")
}

func (f faultExecutor) ChangePassword(pgop.InstanceConfig, string, string) error {
	return pgop.NewFaultError(f.kind, "changePassword 执行面故障")
}

// assertFault 对十一个真实方法断言同一类故障的协议码与关键词。
func assertFault(t *testing.T, kind pgop.FaultKind, wantCode, wantHint string) {
	t.Helper()
	cfg := pgop.Config{
		Admin: true,
		Instances: []pgop.InstanceConfig{
			{Name: pgop.DefaultInstanceName, Host: "127.0.0.1", Port: 5432, User: "postgres"},
		},
	}
	cases := []struct {
		method string
		params string
	}{
		{"db-postgres.instances", `{}`},
		{"db-postgres.databases", `{"instance":"default"}`},
		{"db-postgres.query", `{"instance":"default","sql":"SELECT 1"}`},
		{"db-postgres.status", `{"instance":"default"}`},
		{"db-postgres.slowlog", `{"instance":"default"}`},
		{"db-postgres.createDatabase", `{"instance":"default","name":"kore_fault"}`},
		{"db-postgres.dropDatabase", `{"instance":"default","name":"kore_fault","confirm":true}`},
		{"db-postgres.createUser", `{"instance":"default","user":"kore_fault_user"}`},
		{"db-postgres.dropUser", `{"instance":"default","user":"kore_fault_user","confirm":true}`},
		{"db-postgres.grant", `{"instance":"default","user":"kore_fault_user","database":"kore_fault","privileges":["CONNECT"]}`},
		{"db-postgres.changePassword", `{"instance":"default","user":"kore_fault_user","password":"***"}`},
	}
	for _, c := range cases {
		result, perr := pgop.Call(c.method, json.RawMessage(c.params), cfg, faultExecutor{kind: kind})
		if perr == nil {
			t.Fatalf("%s 注入 %s 后未报错，结果=%s", c.method, kind, result)
		}
		if perr.Code != wantCode {
			t.Fatalf("%s 注入 %s 错误码=%q，期望 %q（message=%q）", c.method, kind, perr.Code, wantCode, perr.Message)
		}
		if !strings.Contains(perr.Message, wantHint) {
			t.Fatalf("%s 错误信息 %q 未含 %q", c.method, perr.Message, wantHint)
		}
		if result != nil {
			t.Fatalf("%s 出错时不应带结果: %s", c.method, result)
		}
	}
}

// TestDiskFullFault 注入磁盘满：十一个方法都返回 E_INTERNAL。
func TestDiskFullFault(t *testing.T) {
	assertFault(t, pgop.FaultDisk, pluginapi.E_INTERNAL, "磁盘满")
}

// TestCgroupLimitFault 注入 cgroup 超限：十一个方法都返回 E_UNSUPPORTED。
func TestCgroupLimitFault(t *testing.T) {
	assertFault(t, pgop.FaultCgroup, pluginapi.E_UNSUPPORTED, "cgroup")
}

// TestKillFault 注入客户端被信号 9 终止：十一个方法都返回 E_UPSTREAM。
func TestKillFault(t *testing.T) {
	assertFault(t, pgop.FaultKill, pluginapi.E_UPSTREAM, "信号 9")
}

// TestTimeoutFault 注入调用超时：十一个方法都返回 E_UPSTREAM。
func TestTimeoutFault(t *testing.T) {
	assertFault(t, pgop.FaultTimeout, pluginapi.E_UPSTREAM, "超时")
}
