package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pengtang1024/kore-plugin-db-postgres/internal/pgop"
	"github.com/pengtang1024/kore-plugin-db-postgres/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// dispatchCall 走一遍 dispatch 并返回响应（不触碰数据库的方法才会被这里调用）。
func dispatchCall(t *testing.T, cfg *config, method, params string) *pluginapi.Response {
	t.Helper()
	req := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id", ReqID: "r",
		Method: method, Params: json.RawMessage(params),
	}
	resp, shutdown := dispatch(req, cfg, testLogger())
	if shutdown {
		t.Fatalf("%s 不应触发 shutdown", method)
	}
	return resp
}

// TestConfigGetDefaults 未保存配置时 config.get 返回默认值 + source=default，
// 密码为空串（不是 "***"，因为根本没有密码）。
func TestConfigGetDefaults(t *testing.T) {
	cfg := testConfig(t)
	resp := dispatchCall(t, cfg, "db-postgres.config.get", `{}`)
	if resp.Error != nil {
		t.Fatalf("config.get 意外错误: %v", resp.Error)
	}
	var result struct {
		Schema struct {
			Title  string `json:"title"`
			Fields []struct {
				Key       string          `json:"key"`
				Type      string          `json:"type"`
				Default   json.RawMessage `json:"default"`
				Sensitive bool            `json:"sensitive"`
			} `json:"fields"`
		} `json:"schema"`
		Values struct {
			Host                   string `json:"host"`
			Port                   int    `json:"port"`
			User                   string `json:"user"`
			Password               string `json:"password"`
			PasswordEnv            string `json:"password_env"`
			DatabasesIncludeSystem bool   `json:"databases_include_system"`
		} `json:"values"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("解析 config.get 结果失败: %v", err)
	}
	if result.Source != "default" {
		t.Fatalf("source=%q，期望 default", result.Source)
	}
	if result.Values.Host != "127.0.0.1" || result.Values.Port != 5432 || result.Values.User != "postgres" {
		t.Fatalf("默认值异常: %+v", result.Values)
	}
	if result.Values.Password != "" {
		t.Fatalf("未设置密码时 password=%q，期望空串", result.Values.Password)
	}
	if result.Values.PasswordEnv != "KORE_PG_PASSWORD" {
		t.Fatalf("password_env=%q", result.Values.PasswordEnv)
	}
	if result.Values.DatabasesIncludeSystem {
		t.Fatal("databases_include_system 默认应为 false")
	}
	keys := map[string]string{}
	for _, field := range result.Schema.Fields {
		keys[field.Key] = field.Type
	}
	for _, want := range []string{"host", "port", "user", "password", "password_env", "databases_include_system"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("schema 缺少字段 %s: %v", want, keys)
		}
	}
	if keys["password"] != "string" {
		t.Fatalf("password 字段类型=%q", keys["password"])
	}
}

// TestConfigSetThenGet 保存后：source=file、密码掩码、端口热更新到实例表。
func TestConfigSetThenGet(t *testing.T) {
	cfg := testConfig(t)
	resp := dispatchCall(t, cfg, "db-postgres.config.set",
		`{"values":{"host":"127.0.0.1","port":15432,"user":"postgres","password":"testpass123","databases_include_system":true}}`)
	if resp.Error != nil {
		t.Fatalf("config.set 意外错误: %v", resp.Error)
	}
	var setResult struct {
		Saved           bool     `json:"saved"`
		Applied         bool     `json:"applied"`
		RestartRequired bool     `json:"restart_required"`
		Warnings        []string `json:"warnings"`
	}
	if err := json.Unmarshal(resp.Result, &setResult); err != nil {
		t.Fatalf("解析 config.set 结果失败: %v", err)
	}
	if !setResult.Saved || !setResult.Applied || setResult.RestartRequired {
		t.Fatalf("保存结果异常: %s", resp.Result)
	}
	if setResult.Warnings == nil {
		t.Fatal("warnings 应为空数组而不是 null")
	}

	// 热生效：实例表已指向 15432，不需要重启。
	instance := cfg.pg.Instances[0]
	if instance.Port != 15432 || instance.Password != "testpass123" {
		t.Fatalf("实例表未热更新: %+v", instance)
	}
	if !cfg.pg.IncludeSystem {
		t.Fatal("include_system 未热更新")
	}

	// 再读：source=file，密码只回 "***"，其它字段是真实值。
	resp = dispatchCall(t, cfg, "db-postgres.config.get", `{}`)
	if resp.Error != nil {
		t.Fatalf("config.get 意外错误: %v", resp.Error)
	}
	if strings.Contains(string(resp.Result), "testpass123") {
		t.Fatalf("config.get 泄漏了密码: %s", resp.Result)
	}
	var got struct {
		Values struct {
			Port     int    `json:"port"`
			Password string `json:"password"`
		} `json:"values"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("解析 config.get 结果失败: %v", err)
	}
	if got.Source != "file" {
		t.Fatalf("source=%q，期望 file", got.Source)
	}
	if got.Values.Password != "***" {
		t.Fatalf("密码未掩码: %q", got.Values.Password)
	}
	if got.Values.Port != 15432 {
		t.Fatalf("port=%d，期望 15432", got.Values.Port)
	}
}

// TestConfigSetPlaceholderKeepsPassword 提交 "***" 表示不修改密码。
func TestConfigSetPlaceholderKeepsPassword(t *testing.T) {
	cfg := testConfig(t)
	if resp := dispatchCall(t, cfg, "db-postgres.config.set",
		`{"values":{"password":"keep-me-please"}}`); resp.Error != nil {
		t.Fatalf("首次保存失败: %v", resp.Error)
	}
	if resp := dispatchCall(t, cfg, "db-postgres.config.set",
		`{"values":{"password":"***","port":15433}}`); resp.Error != nil {
		t.Fatalf("二次保存失败: %v", resp.Error)
	}
	if cfg.values.Password != "keep-me-please" {
		t.Fatalf("密码被 *** 覆盖: %q", cfg.values.Password)
	}
	if cfg.pg.Instances[0].Port != 15433 {
		t.Fatalf("端口未更新: %+v", cfg.pg.Instances[0])
	}
}

// TestConfigSetInvalidNotPersisted 非法值返回 E_INVALID，且磁盘上不留半份配置。
func TestConfigSetInvalidNotPersisted(t *testing.T) {
	cfg := testConfig(t)
	cases := []string{
		`{"values":{"port":70000}}`,
		`{"values":{"port":0}}`,
		`{"values":{"host":"-oProxyCommand=sh"}}`,
		`{"values":{"user":"bad user"}}`,
		`{"values":{"password_env":"1BAD"}}`,
		`{"values":{"unknown_field":"x"}}`,
		`{"values":{"port":"5432"}}`,
		`{}`,
	}
	for _, params := range cases {
		resp := dispatchCall(t, cfg, "db-postgres.config.set", params)
		if resp.Error == nil || resp.Error.Code != pluginapi.E_INVALID {
			t.Fatalf("params=%s 错误=%v，期望 E_INVALID", params, resp.Error)
		}
		if resp.Result != nil {
			t.Fatalf("params=%s 出错时不应带结果: %s", params, resp.Result)
		}
	}
	if _, err := os.Stat(cfg.store.Path()); err == nil {
		t.Fatalf("非法值竟然落盘了: %s", cfg.store.Path())
	}
	// 旧配置仍然可用。
	if cfg.values.Port != 5432 {
		t.Fatalf("非法值污染了内存配置: %+v", cfg.values)
	}
}

// TestResolveValuesPriority 优先级：config.json > 环境变量 > 默认值。
func TestResolveValuesPriority(t *testing.T) {
	// 1) 纯默认值。
	store := newTestStore(t)
	values, source, err := resolveValues(store)
	if err != nil {
		t.Fatalf("resolveValues 失败: %v", err)
	}
	if source != "default" || values.Host != "127.0.0.1" || values.Port != 5432 {
		t.Fatalf("默认值异常: %s %+v", source, values)
	}

	// 2) 环境变量覆盖默认值。
	t.Setenv(EnvHost, "10.0.0.9")
	t.Setenv(EnvPort, "6543")
	t.Setenv(EnvUser, "kore")
	values, source, err = resolveValues(store)
	if err != nil {
		t.Fatalf("resolveValues 失败: %v", err)
	}
	if source != "env" || values.Host != "10.0.0.9" || values.Port != 6543 || values.User != "kore" {
		t.Fatalf("环境变量未生效: %s %+v", source, values)
	}

	// 3) 配置文件覆盖环境变量。
	if err := store.Write(json.RawMessage(`{"host":"127.0.0.1","port":15432,"user":"postgres","password_env":"KORE_PG_PASSWORD"}`)); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	values, source, err = resolveValues(store)
	if err != nil {
		t.Fatalf("resolveValues 失败: %v", err)
	}
	if source != "file" || values.Host != "127.0.0.1" || values.Port != 15432 {
		t.Fatalf("配置文件未覆盖环境变量: %s %+v", source, values)
	}
}

// TestResolveValuesInvalidEnv 非法环境变量直接报错（不静默回退默认值）。
func TestResolveValuesInvalidEnv(t *testing.T) {
	store := newTestStore(t)
	t.Setenv(EnvPort, "not-a-port")
	if _, _, err := resolveValues(store); err == nil {
		t.Fatal("非法 KORE_PG_PORT 未报错")
	}
	t.Setenv(EnvPort, "")
	t.Setenv(EnvIncludeSystem, "maybe")
	if _, _, err := resolveValues(store); err == nil {
		t.Fatal("非法 KORE_PG_DATABASES_INCLUDE_SYSTEM 未报错")
	}
}

// TestBuildInstancesEnvInstances 默认实例 + 多实例合并，default 由配置覆盖。
func TestBuildInstancesEnvInstances(t *testing.T) {
	values := fileValues{Host: "127.0.0.1", Port: 15432, User: "postgres", Password: "pw"}
	envInstances := []pgop.InstanceConfig{
		{Name: "default", Host: "10.0.0.1", Port: 5432, User: "someone"},
		{Name: "primary", Host: "10.0.0.2", Port: 5432, User: "someone"},
	}
	instances := buildInstances(values, true, envInstances)
	if len(instances) != 2 {
		t.Fatalf("实例数=%d，期望 2: %+v", len(instances), instances)
	}
	if instances[0].Name != "default" || instances[0].Port != 15432 {
		t.Fatalf("默认实例未被配置文件覆盖: %+v", instances[0])
	}
	if instances[1].Name != "primary" {
		t.Fatalf("附加实例丢失: %+v", instances)
	}
	// includeDefault=false（未配置任何来源）时不虚构实例。
	if got := buildInstances(values, false, nil); len(got) != 0 {
		t.Fatalf("未配置时不该有实例: %+v", got)
	}
}

// newTestStore 构造指向临时目录的配置存储。
func newTestStore(t *testing.T) *plugincfg.Store {
	t.Helper()
	return plugincfg.NewStore(t.TempDir())
}

// TestInfoMethodCapabilities info.json 的方法级能力必须与「只读 read / 管理 admin」一致。
//
// 尤其盯住 config.get / config.set：漏声明会让配置读取回退到插件级 system:admin，
// 授权与写审计被放大（这是反复踩过的坑，所以用精确表驱动钉死）。
func TestInfoMethodCapabilities(t *testing.T) {
	raw, err := os.ReadFile("info.json")
	if err != nil {
		t.Fatalf("读取 info.json 失败: %v", err)
	}
	var manifest struct {
		Version            string            `json:"version"`
		Capabilities       []string          `json:"capabilities"`
		MethodCapabilities map[string]string `json:"method_capabilities"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("解析 info.json 失败: %v", err)
	}
	want := map[string]string{
		"db-postgres.config.get": "system:read",
		"db-postgres.instances":  "system:read",
		"db-postgres.databases":  "system:read",
		"db-postgres.query":      "system:read",
		"db-postgres.exec":       "system:admin",
		"db-postgres.status":     "system:read",
		"db-postgres.slowlog":    "system:read",

		"db-postgres.backupList":     "system:read",
		"db-postgres.backup":         "system:admin",
		"db-postgres.restore":        "system:admin",
		"db-postgres.backupDelete":   "system:admin",
		"db-postgres.backupDownload": "system:admin",

		"db-postgres.config.set":     "system:admin",
		"db-postgres.createDatabase": "system:admin",
		"db-postgres.dropDatabase":   "system:admin",
		"db-postgres.createUser":     "system:admin",
		"db-postgres.dropUser":       "system:admin",
		"db-postgres.grant":          "system:admin",
		"db-postgres.changePassword": "system:admin",
	}
	if len(manifest.MethodCapabilities) != len(want) {
		t.Fatalf("method_capabilities 条数=%d，期望 %d: %v", len(manifest.MethodCapabilities), len(want), manifest.MethodCapabilities)
	}
	for method, capability := range want {
		if got := manifest.MethodCapabilities[method]; got != capability {
			t.Fatalf("method_capabilities[%q]=%q，期望 %q", method, got, capability)
		}
	}
	for method := range manifest.MethodCapabilities {
		if _, ok := want[method]; !ok {
			t.Fatalf("出现未登记的方法级能力: %s", method)
		}
	}
	if len(manifest.Capabilities) == 0 || manifest.Capabilities[0] != "system:admin" {
		t.Fatalf("插件级 capabilities 应保留 system:admin（写方法门控依赖它）: %v", manifest.Capabilities)
	}
}
