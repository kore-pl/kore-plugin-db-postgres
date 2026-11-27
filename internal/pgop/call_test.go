package pgop

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// testConfig 构造一个带默认实例的执行面配置（不连接任何真实服务）。
func testConfig() Config {
	return Config{
		Admin: true,
		Instances: []InstanceConfig{
			{Name: DefaultInstanceName, Host: "127.0.0.1", Port: 5432, User: "postgres"},
			{Name: "primary", Host: "127.0.0.1", Port: 15432, User: "postgres"},
		},
	}
}

// fakeExec 是可编程执行面：记录入参、返回预设结果或错误。
type fakeExec struct {
	statuses  []InstanceStatus
	databases []Database
	query     QueryResult
	status    StatusResult
	err       error

	gotSQL    string
	gotLimit  int
	gotDB     string
	instances []InstanceConfig
	include   bool
	// created 是写方法的返回预设；gotAdmin 记录写方法收到的入参。
	created  bool
	gotAdmin []string
	// 备份/恢复：记录入参并返回预设结果或错误。
	backupSpec  BackupSpec
	restoreSpec RestoreSpec
	backupSize  int64
	backupErr   error
	restoreErr  error
	backupCalls int
}

func (f *fakeExec) Instances(instances []InstanceConfig) ([]InstanceStatus, error) {
	f.instances = instances
	return f.statuses, f.err
}

func (f *fakeExec) Databases(instance InstanceConfig, includeSystem bool) ([]Database, error) {
	f.include = includeSystem
	return f.databases, f.err
}

func (f *fakeExec) Query(instance InstanceConfig, sql, database string, limit int) (QueryResult, error) {
	f.gotSQL, f.gotDB, f.gotLimit = sql, database, limit
	return f.query, f.err
}

// Exec 记录写语句入参（与 Query 共用 gotSQL/gotLimit 与 query 预设），
// 供 callExec 单测断言「拒绝路径不触达执行面」与「合法写语句执行参数正确」。
func (f *fakeExec) Exec(instance InstanceConfig, sql, database string, limit int) (QueryResult, error) {
	f.gotSQL, f.gotDB, f.gotLimit = sql, database, limit
	return f.query, f.err
}

func (f *fakeExec) Status(instance InstanceConfig) (StatusResult, error) { return f.status, f.err }

// --- 写方法：记录入参，返回预设结果（call_test 里用于校验方法层防线）---

func (f *fakeExec) CreateDatabase(instance InstanceConfig, name, owner string) (bool, error) {
	f.gotAdmin = append(f.gotAdmin, "createDatabase:"+name+":"+owner)
	return f.created, f.err
}

func (f *fakeExec) DropDatabase(instance InstanceConfig, name string) (bool, error) {
	f.gotAdmin = append(f.gotAdmin, "dropDatabase:"+name)
	return true, f.err
}

func (f *fakeExec) CreateRole(instance InstanceConfig, user, password string, canCreateDB, canCreateRole bool) (bool, error) {
	f.gotAdmin = append(f.gotAdmin, "createUser:"+user+":"+password)
	return f.created, f.err
}

func (f *fakeExec) DropRole(instance InstanceConfig, user string) (bool, error) {
	f.gotAdmin = append(f.gotAdmin, "dropUser:"+user)
	return true, f.err
}

func (f *fakeExec) Grant(instance InstanceConfig, user, database string, privileges []string, withGrantOption bool) error {
	f.gotAdmin = append(f.gotAdmin, "grant:"+user+":"+database+":"+strings.Join(privileges, ","))
	return f.err
}

func (f *fakeExec) ChangePassword(instance InstanceConfig, user, password string) error {
	f.gotAdmin = append(f.gotAdmin, "changePassword:"+user+":"+password)
	return f.err
}

// Backup / Restore 记录入参；不创建文件（真实文件由方法层或测试自行准备）。
func (f *fakeExec) Backup(instance InstanceConfig, spec BackupSpec) (BackupData, error) {
	f.backupCalls++
	f.backupSpec = spec
	if f.backupErr != nil {
		return BackupData{}, f.backupErr
	}
	size := f.backupSize
	if size == 0 {
		size = 4096
	}
	return BackupData{SizeBytes: size, Format: spec.Format, Compression: "test"}, nil
}

func (f *fakeExec) Restore(instance InstanceConfig, spec RestoreSpec) error {
	f.restoreSpec = spec
	return f.restoreErr
}

// TestCallInstances 实例探测结果结构化：单实例失败不抹掉其它实例。
func TestCallInstances(t *testing.T) {
	exec := &fakeExec{statuses: []InstanceStatus{
		{Name: "default", Host: "127.0.0.1", Port: 5432, Reachable: true, Version: "PostgreSQL 16.16"},
		{Name: "primary", Host: "127.0.0.1", Port: 15432, Reachable: false, Error: "connection refused"},
	}}
	result, perr := Call("db-postgres.instances", json.RawMessage(`{}`), testConfig(), exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var parsed InstancesResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if parsed.Count != 2 || len(parsed.Instances) != 2 {
		t.Fatalf("实例数=%d，期望 2", parsed.Count)
	}
	if !parsed.Instances[0].Reachable || parsed.Instances[1].Reachable {
		t.Fatalf("可达性结果异常: %+v", parsed.Instances)
	}
	if parsed.Instances[1].Error == "" {
		t.Fatal("失败实例未带 error")
	}
	if len(exec.instances) != 2 {
		t.Fatalf("执行面收到的实例数=%d，期望 2", len(exec.instances))
	}
}

// TestCallInstancesFilter 按 instance 过滤只探测一个实例。
func TestCallInstancesFilter(t *testing.T) {
	exec := &fakeExec{}
	if _, perr := Call("db-postgres.instances", json.RawMessage(`{"instance":"primary"}`), testConfig(), exec); perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if len(exec.instances) != 1 || exec.instances[0].Name != "primary" {
		t.Fatalf("过滤结果异常: %+v", exec.instances)
	}
}

// TestCallInstancesHint 空态引导：无实例与「全部不可达」给不同 hint，有可达实例时不给。
func TestCallInstancesHint(t *testing.T) {
	empty := testConfig()
	empty.Instances = nil
	result, perr := Call("db-postgres.instances", json.RawMessage(`{}`), empty, &fakeExec{})
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var parsed InstancesResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if parsed.Count != 0 || !strings.Contains(parsed.Hint, "尚未配置 PostgreSQL 连接") {
		t.Fatalf("空实例 hint 异常: %s", result)
	}

	unreachable := testConfig()
	unreachable.Instances = []InstanceConfig{{Name: DefaultInstanceName, Host: "127.0.0.1", Port: 5432, User: "postgres"}}
	result, perr = Call("db-postgres.instances", json.RawMessage(`{}`), unreachable, &fakeExec{statuses: []InstanceStatus{
		{Name: DefaultInstanceName, Host: "127.0.0.1", Port: 5432, Reachable: false, Error: "connection refused"},
	}})
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if !strings.Contains(string(result), "配置了 1 个实例但均不可达") {
		t.Fatalf("不可达 hint 异常: %s", result)
	}

	result, perr = Call("db-postgres.instances", json.RawMessage(`{}`), testConfig(), &fakeExec{statuses: []InstanceStatus{
		{Name: DefaultInstanceName, Host: "127.0.0.1", Port: 5432, Reachable: true, Version: "PostgreSQL 16.16"},
	}})
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if strings.Contains(string(result), `"hint"`) {
		t.Fatalf("可达实例不应带 hint: %s", result)
	}
}

// TestCallDatabases 库列表：include_system 默认取配置，显式入参覆盖配置。
func TestCallDatabases(t *testing.T) {
	exec := &fakeExec{databases: []Database{{Name: "postgres", SizeBytes: 8234567}, {Name: "kore_e2e", SizeBytes: 7654321}}}
	cfg := testConfig()
	result, perr := Call("db-postgres.databases", json.RawMessage(`{"instance":"default"}`), cfg, exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var parsed DatabasesResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if parsed.Count != 2 || parsed.Databases[0].Name != "postgres" || parsed.Databases[1].SizeBytes != 7654321 {
		t.Fatalf("库列表异常: %s", result)
	}
	if exec.include {
		t.Fatal("默认不应包含模板库")
	}
	if _, perr := Call("db-postgres.databases", json.RawMessage(`{"instance":"default","include_system":true}`), cfg, exec); perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if !exec.include {
		t.Fatal("include_system=true 未透传到执行面")
	}
}

// TestCallQueryWhitelist 只读白名单在方法层生效：写语句在触达数据库前被拒。
func TestCallQueryWhitelist(t *testing.T) {
	cfg := testConfig()
	for _, sql := range []string{"DROP TABLE x", "SELECT 1; DROP TABLE x", "WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x"} {
		exec := &fakeExec{}
		result, perr := Call("db-postgres.query", mustJSON(t, map[string]any{"instance": "default", "sql": sql}), cfg, exec)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("SQL=%q 未被 E_INVALID 拒绝: %v", sql, perr)
		}
		if result != nil {
			t.Fatalf("SQL=%q 被拒时不应带结果", sql)
		}
		if exec.gotSQL != "" {
			t.Fatalf("SQL=%q 在校验前就到了执行面: %q", sql, exec.gotSQL)
		}
	}
}

// TestCallQueryLimit limit 缺省 100、上限 1000，越界 E_INVALID。
func TestCallQueryLimit(t *testing.T) {
	cfg := testConfig()
	exec := &fakeExec{query: QueryResult{Columns: []string{"n"}, Rows: []json.RawMessage{json.RawMessage(`{"n":1}`)}}}
	if _, perr := Call("db-postgres.query", mustJSON(t, map[string]any{"instance": "default", "sql": "SELECT 1"}), cfg, exec); perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if exec.gotLimit != defaultQueryLimit {
		t.Fatalf("缺省 limit=%d，期望 %d", exec.gotLimit, defaultQueryLimit)
	}
	if _, perr := Call("db-postgres.query", mustJSON(t, map[string]any{"instance": "default", "sql": "SELECT 1", "limit": 1000}), cfg, exec); perr != nil {
		t.Fatalf("limit=1000 应被接受: %v", perr)
	}
	for _, limit := range []int{-1, 1001} {
		result, perr := Call("db-postgres.query", mustJSON(t, map[string]any{"instance": "default", "sql": "SELECT 1", "limit": limit}), cfg, exec)
		if perr == nil || perr.Code != pluginapi.E_INVALID || result != nil {
			t.Fatalf("limit=%d 未被拒: %v", limit, perr)
		}
	}
}

// TestCallQueryEmptySQL 空 SQL 与缺少 params 都返回 E_INVALID。
func TestCallQueryEmptySQL(t *testing.T) {
	cfg := testConfig()
	exec := &fakeExec{}
	if _, perr := Call("db-postgres.query", json.RawMessage(`{"instance":"default","sql":"   "}`), cfg, exec); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("空 SQL 未被拒: %v", perr)
	}
	if _, perr := Call("db-postgres.query", nil, cfg, exec); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("缺 params 未被拒: %v", perr)
	}
}

// TestCallQueryDatabase 参数 database 透传到执行面；缺省回落维护库；非法库名
// 在触达执行面之前以 E_INVALID 拒绝（psql -d 注入防线，query 保持 system:read）。
func TestCallQueryDatabase(t *testing.T) {
	cfg := testConfig()
	exec := &fakeExec{}
	// 缺省回落 postgres。
	if _, perr := Call("db-postgres.query", json.RawMessage(`{"instance":"default","sql":"SELECT 1"}`), cfg, exec); perr != nil {
		t.Fatalf("缺省库调用失败: %v", perr)
	}
	if exec.gotDB != defaultDatabase {
		t.Fatalf("缺省 database=%q，期望 %q", exec.gotDB, defaultDatabase)
	}
	// 业务库透传。
	if _, perr := Call("db-postgres.query", json.RawMessage(`{"instance":"default","sql":"SELECT 1","database":"myapp"}`), cfg, exec); perr != nil {
		t.Fatalf("业务库调用失败: %v", perr)
	}
	if exec.gotDB != "myapp" {
		t.Fatalf("database=%q，期望 myapp", exec.gotDB)
	}
	// 注入库名：E_INVALID 且不触达执行面。
	for _, bad := range []string{"myapp;DROP TABLE x", "my app", "-d postgres", `a"b`} {
		before := exec.gotSQL
		result, perr := Call("db-postgres.query",
			mustJSON(t, map[string]any{"instance": "default", "sql": "SELECT 1", "database": bad}), cfg, exec)
		if perr == nil || perr.Code != pluginapi.E_INVALID || result != nil {
			t.Fatalf("database=%q 未被 E_INVALID 拒绝: %v", bad, perr)
		}
		if exec.gotSQL != before {
			t.Fatalf("database=%q 被拒仍触达执行面: %q", bad, exec.gotSQL)
		}
	}
}

// TestCallExecDatabase exec 的 database 参数同样透传且校验；两级确认 / 白名单不变。
func TestCallExecDatabase(t *testing.T) {
	cfg := testConfig()
	exec := &fakeExec{query: QueryResult{}}
	// 业务库 + 合法确认。
	if _, perr := Call("db-postgres.exec", json.RawMessage(`{"instance":"default","sql":"INSERT INTO t VALUES (1)","confirm":true,"database":"myapp"}`), cfg, exec); perr != nil {
		t.Fatalf("exec 业务库调用失败: %v", perr)
	}
	if exec.gotDB != "myapp" {
		t.Fatalf("exec database=%q，期望 myapp", exec.gotDB)
	}
	// 缺省回落。
	if _, perr := Call("db-postgres.exec", json.RawMessage(`{"instance":"default","sql":"INSERT INTO t VALUES (1)","confirm":true}`), cfg, exec); perr != nil {
		t.Fatalf("exec 缺省库调用失败: %v", perr)
	}
	if exec.gotDB != defaultDatabase {
		t.Fatalf("exec 缺省 database=%q，期望 %q", exec.gotDB, defaultDatabase)
	}
	// 非法库名即使确认也拒绝，且不触达执行面。
	before := exec.gotSQL
	if _, perr := Call("db-postgres.exec", json.RawMessage(`{"instance":"default","sql":"INSERT INTO t VALUES (1)","confirm":true,"database":"bad;name"}`), cfg, exec); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("非法库名未被拒: %v", perr)
	}
	if exec.gotSQL != before {
		t.Fatalf("非法库名被拒仍触达执行面: %q", exec.gotSQL)
	}
}

// TestCallStatus status 结果原样返回。
func TestCallStatus(t *testing.T) {
	exec := &fakeExec{status: StatusResult{
		Instance: "default", Version: "PostgreSQL 16.16", Connections: 3, Databases: 5,
		UptimeS: 120, CacheHitRatio: 0.9876, BlocksHit: 100, BlocksRead: 1,
	}}
	result, perr := Call("db-postgres.status", json.RawMessage(`{"instance":"default"}`), testConfig(), exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var parsed StatusResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if parsed.Connections != 3 || parsed.Databases != 5 || parsed.UptimeS != 120 || parsed.CacheHitRatio != 0.9876 {
		t.Fatalf("status 结果异常: %s", result)
	}
}

// TestCallInstanceNotFound 未配置实例返回 E_NOTFOUND，并在信息里列出已配置实例。
func TestCallInstanceNotFound(t *testing.T) {
	result, perr := Call("db-postgres.status", json.RawMessage(`{"instance":"ghost"}`), testConfig(), &fakeExec{})
	if perr == nil || perr.Code != pluginapi.E_NOTFOUND {
		t.Fatalf("错误=%v，期望 E_NOTFOUND", perr)
	}
	if result != nil {
		t.Fatalf("出错时不应带结果: %s", result)
	}
	if !strings.Contains(perr.Message, "default") {
		t.Fatalf("错误信息未列出已配置实例: %q", perr.Message)
	}
}

// TestCallInvalidParams 实例名非法 / 缺 instance 返回 E_INVALID。
func TestCallInvalidParams(t *testing.T) {
	cfg := testConfig()
	for _, params := range []string{`{"instance":"Bad Name"}`, `{"instance":"../etc"}`, `{"instance":""}`} {
		for _, method := range []string{"db-postgres.databases", "db-postgres.status", "db-postgres.query"} {
			body := params
			if method == "db-postgres.query" {
				body = strings.Replace(params, "}", `,"sql":"SELECT 1"}`, 1)
			}
			if _, perr := Call(method, json.RawMessage(body), cfg, &fakeExec{}); perr == nil || perr.Code != pluginapi.E_INVALID {
				t.Fatalf("%s %s 未被拒: %v", method, body, perr)
			}
		}
	}
}

// TestCallConfigError 配置解析错误时所有业务方法返回 E_INVALID（插件仍可启动）。
func TestCallConfigError(t *testing.T) {
	cfg := testConfig()
	cfg.Err = fmt.Errorf("instances.json 非法")
	if _, perr := Call("db-postgres.instances", json.RawMessage(`{}`), cfg, &fakeExec{}); perr == nil || perr.Code != pluginapi.E_INVALID {
		t.Fatalf("配置错误未被映射为 E_INVALID: %v", perr)
	}
}

// TestCallFault 四类故障经 Call 映射到对应协议码。
func TestCallFault(t *testing.T) {
	cases := []struct {
		kind FaultKind
		code string
		hint string
	}{
		{FaultKill, pluginapi.E_UPSTREAM, "信号 9"},
		{FaultTimeout, pluginapi.E_UPSTREAM, "超时"},
		{FaultDisk, pluginapi.E_INTERNAL, "磁盘满"},
		{FaultCgroup, pluginapi.E_UNSUPPORTED, "cgroup"},
	}
	methods := []struct {
		method string
		params string
	}{
		{"db-postgres.instances", `{}`},
		{"db-postgres.databases", `{"instance":"default"}`},
		{"db-postgres.query", `{"instance":"default","sql":"SELECT 1"}`},
		{"db-postgres.status", `{"instance":"default"}`},
	}
	for _, c := range cases {
		for _, m := range methods {
			result, perr := Call(m.method, json.RawMessage(m.params), testConfig(),
				&fakeExec{err: NewFaultError(c.kind, "注入")})
			if perr == nil || perr.Code != c.code {
				t.Fatalf("%s 注入 %s 错误=%v，期望 %s", m.method, c.kind, perr, c.code)
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

// TestCallUnknown 未知方法 / 旧域前缀 / 已移除的 connections 都返回 E_NOTFOUND。
func TestCallUnknown(t *testing.T) {
	for _, method := range []string{"pg.instances", "db-postgres.connections", "db-postgres."} {
		if _, perr := Call(method, json.RawMessage(`{}`), testConfig(), &fakeExec{}); perr == nil || perr.Code != pluginapi.E_NOTFOUND {
			t.Fatalf("方法 %q 错误=%v，期望 E_NOTFOUND", method, perr)
		}
	}
}

// TestBusinessVerb 方法名归一化：只接受 db-postgres.<4 个动词>。
func TestBusinessVerb(t *testing.T) {
	for _, verb := range []string{"instances", "databases", "query", "exec", "status"} {
		got, ok := businessVerb("db-postgres." + verb)
		if !ok || got != verb {
			t.Fatalf("businessVerb(%q)=(%q,%v)", verb, got, ok)
		}
	}
	for _, method := range []string{"pg.query", "query", "db-mysql.query", "db-postgres.query "} {
		if _, ok := businessVerb(method); ok {
			t.Fatalf("%q 不应被接受", method)
		}
	}
}

// mustJSON 把 map 编码为 json.RawMessage（测试辅助）。
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return data
}
