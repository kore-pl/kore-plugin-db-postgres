// Package e2e 对真实 PostgreSQL 做端到端验证：配置契约 → 实例探测 → 库列表 →
// 只读查询 → 只读拒绝 → 状态指标 → 密码不外泄。
//
// 默认跳过；需要先启动容器（本机没有 psql 时插件会自动回退 docker exec）：
//
//	docker run -d --name kore-pg-test -p 15432:5432 -e POSTGRES_PASSWORD=*** postgres:16-alpine
//	KORE_PG_E2E=1 KORE_PG_DOCKER_CONTAINER=kore-pg-test go test -timeout 300s -v ./tests/e2e
package e2e_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

const (
	// e2eContainer 是 E2E 使用的 postgres 容器名（与任务口径一致）。
	e2eContainer = "kore-pg-test"
	// e2ePassword 是容器初始化口令；它不允许出现在任何响应里。
	e2ePassword = "testpass123"
	// e2ePort 是容器发布到宿主机的端口。
	e2ePort = 15432
	// e2eTable 是 E2E 造的验证表。
	e2eTable = "kore_e2e"
	// callTimeout 是单次方法调用的等待上限（含 docker exec 启动开销）。
	callTimeout = 30 * time.Second
)

// TestRealPostgres 走完整链路；每一步都把真实响应打到测试日志里作为证据。
func TestRealPostgres(t *testing.T) {
	if os.Getenv("KORE_PG_E2E") != "1" {
		t.Skip("设置 KORE_PG_E2E=1 并启动 kore-pg-test 容器后运行")
	}
	container := strings.TrimSpace(os.Getenv("KORE_PG_DOCKER_CONTAINER"))
	if container == "" {
		container = e2eContainer
	}
	requireContainer(t, container)

	binary := buildPlugin(t)
	stateDir := t.TempDir()
	conn := startPlugin(t, binary, container, stateDir)
	defer func() { _ = conn.Close() }()

	// 步骤 1：初始配置来自默认值，还没有密码。
	result, perr := call(t, conn, "db-postgres.config.get", `{}`)
	mustOK(t, "STEP1 db-postgres.config.get（默认值）", result, perr)
	step1 := decode[configGetResult](t, result)
	if step1.Source != "default" {
		t.Fatalf("初始 source=%q，期望 default", step1.Source)
	}
	if step1.Values.Port != 5432 || step1.Values.Host != "127.0.0.1" || step1.Values.User != "postgres" {
		t.Fatalf("默认值异常: %+v", step1.Values)
	}
	if step1.Values.Password != "" {
		t.Fatalf("未设置密码时 password=%q，期望空串", step1.Values.Password)
	}

	// 步骤 2：config.set 指向 15432（容器发布端口）。
	setParams := fmt.Sprintf(`{"values":{"host":"127.0.0.1","port":%d,"user":"postgres","password":"%s","databases_include_system":false}}`, e2ePort, e2ePassword)
	result, perr = call(t, conn, "db-postgres.config.set", setParams)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP2 db-postgres.config.set", result, perr)
	step2 := decode[configSetResult](t, result)
	if !step2.Saved || !step2.Applied || step2.RestartRequired {
		t.Fatalf("保存结果异常: %s", result)
	}

	// 步骤 2b：配置文件 0600 且 config.get 只回 ***。
	mode := configFileMode(t, stateDir)
	if mode != 0o600 {
		t.Fatalf("配置文件权限=%o，期望 600", mode)
	}
	result, perr = call(t, conn, "db-postgres.config.get", `{}`)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP2b db-postgres.config.get（保存后）", result, perr)
	step2b := decode[configGetResult](t, result)
	if step2b.Source != "file" || step2b.Values.Password != "***" || step2b.Values.Port != e2ePort {
		t.Fatalf("保存后配置异常: %s", result)
	}

	// 步骤 3：instances 真实连通，版本是 16.x。
	result, perr = call(t, conn, "db-postgres.instances", `{}`)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP3 db-postgres.instances", result, perr)
	step3 := decode[instancesResult](t, result)
	if step3.Count != 1 || len(step3.Instances) != 1 {
		t.Fatalf("实例数=%d，期望 1", step3.Count)
	}
	probe := step3.Instances[0]
	if !probe.Reachable {
		t.Fatalf("实例不可达: %+v", probe)
	}
	if probe.Error != "" {
		t.Fatalf("可达实例不应带 error: %+v", probe)
	}
	if !strings.Contains(probe.Version, "PostgreSQL 16.") {
		t.Fatalf("版本=%q，期望 PostgreSQL 16.x", probe.Version)
	}
	if probe.Port != e2ePort || probe.Host != "127.0.0.1" {
		t.Fatalf("实例地址回显异常: %+v", probe)
	}

	// 步骤 4：databases 列出 postgres，且默认不含模板库。
	result, perr = call(t, conn, "db-postgres.databases", `{"instance":"default"}`)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP4 db-postgres.databases", result, perr)
	step4 := decode[databasesResult](t, result)
	if !hasDatabase(step4.Databases, "postgres") {
		t.Fatalf("库列表未包含 postgres: %+v", step4.Databases)
	}
	if hasDatabase(step4.Databases, "template0") || hasDatabase(step4.Databases, "template1") {
		t.Fatalf("默认结果不应含模板库: %+v", step4.Databases)
	}
	// postgres 库有真实大小（> 0 字节），证明 pg_database_size 生效。
	for _, database := range step4.Databases {
		if database.Name == "postgres" && database.SizeBytes <= 0 {
			t.Fatalf("postgres 库大小=%d，期望 >0", database.SizeBytes)
		}
	}
	// include_system=true 时模板库出现。
	result, perr = call(t, conn, "db-postgres.databases", `{"instance":"default","include_system":true}`)
	mustOK(t, "STEP4b db-postgres.databases（含模板库）", result, perr)
	step4b := decode[databasesResult](t, result)
	if !hasDatabase(step4b.Databases, "template0") {
		t.Fatalf("include_system=true 未包含 template0: %+v", step4b.Databases)
	}

	// 步骤 5：query "SELECT 1 AS n" → n=1（数字而不是字符串）。
	result, perr = call(t, conn, "db-postgres.query", `{"instance":"default","sql":"SELECT 1 AS n"}`)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP5 db-postgres.query SELECT 1 AS n", result, perr)
	step5 := decode[queryResult](t, result)
	if step5.RowCount != 1 || len(step5.Rows) != 1 {
		t.Fatalf("行数=%d，期望 1", step5.RowCount)
	}
	if string(step5.Rows[0]) != `{"n":1}` {
		t.Fatalf("首行=%s，期望 {\"n\":1}", step5.Rows[0])
	}

	// 步骤 6：容器外造数据（docker exec psql），插件 query 读到。
	seedTable(t, container)
	result, perr = call(t, conn, "db-postgres.query",
		`{"instance":"default","sql":"SELECT id, name FROM kore_e2e ORDER BY id"}`)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP6 db-postgres.query 读容器外表数据", result, perr)
	step6 := decode[queryResult](t, result)
	if step6.RowCount != 2 {
		t.Fatalf("造了 2 行，实际读到 %d 行: %s", step6.RowCount, result)
	}
	if !strings.Contains(string(step6.Rows[0]), `"id":1`) || !strings.Contains(string(step6.Rows[0]), `"name":"alpha"`) {
		t.Fatalf("首行内容异常: %s", step6.Rows[0])
	}

	// 步骤 6b：NULL 与空串必须可区分。
	result, perr = call(t, conn, "db-postgres.query",
		`{"instance":"default","sql":"SELECT NULL::text AS n, ''::text AS e"}`)
	mustOK(t, "STEP6b db-postgres.query NULL vs 空串", result, perr)
	step6b := decode[queryResult](t, result)
	if string(step6b.Rows[0]) != `{"n":null,"e":""}` {
		t.Fatalf("NULL/空串区分失败: %s", step6b.Rows[0])
	}

	// 步骤 7：写操作与多语句在触达数据库前被 E_INVALID 拒绝。
	for _, sql := range []string{
		"DROP TABLE kore_e2e",
		"SELECT 1; DROP TABLE kore_e2e",
		"WITH x AS (DELETE FROM kore_e2e RETURNING *) SELECT * FROM x",
		"EXPLAIN ANALYZE DELETE FROM kore_e2e",
		"SELECT pg_sleep(5)",
	} {
		params := fmt.Sprintf(`{"instance":"default","sql":%q}`, sql)
		bad, badErr := call(t, conn, "db-postgres.query", params)
		if badErr == nil {
			t.Fatalf("SQL=%q 未被拒绝，结果=%s", sql, bad)
		}
		if badErr.Code != pluginapi.E_INVALID {
			t.Fatalf("SQL=%q 错误码=%q，期望 E_INVALID（%s）", sql, badErr.Code, badErr.Message)
		}
		if bad != nil {
			t.Fatalf("SQL=%q 被拒时不应带结果: %s", sql, bad)
		}
		t.Logf("STEP7 拒绝 %q → %s: %s", sql, badErr.Code, badErr.Message)
	}
	// 表必须还在：证明拒绝发生在执行之前，而不是执行失败。
	result, perr = call(t, conn, "db-postgres.query", `{"instance":"default","sql":"SELECT count(*) AS c FROM kore_e2e"}`)
	mustOK(t, "STEP7b 表仍在", result, perr)
	if got := decode[queryResult](t, result); string(got.Rows[0]) != `{"c":2}` {
		t.Fatalf("写操作拒绝后表数据异常: %s", got.Rows[0])
	}

	// 步骤 8：行数上限 1000 与单格 8 KiB 上限。
	result, perr = call(t, conn, "db-postgres.query",
		`{"instance":"default","sql":"SELECT g AS n FROM generate_series(1, 1100) g","limit":1000}`)
	mustOK(t, "STEP8 db-postgres.query 1000 行上限", result, perr)
	step8 := decode[queryResult](t, result)
	if step8.RowCount != 1000 || !step8.Truncated {
		t.Fatalf("行数=%d truncated=%v，期望 1000/true", step8.RowCount, step8.Truncated)
	}
	result, perr = call(t, conn, "db-postgres.query",
		`{"instance":"default","sql":"SELECT repeat('x', 9000) AS big"}`)
	mustOK(t, "STEP8b db-postgres.query 单格 8KiB 上限", result, perr)
	step8b := decode[queryResult](t, result)
	if !step8b.Truncated {
		t.Fatalf("超长单元格未标记 truncated: %s", result)
	}
	var bigRow struct {
		Big string `json:"big"`
	}
	if err := json.Unmarshal(step8b.Rows[0], &bigRow); err != nil {
		t.Fatalf("解析超长行失败: %v", err)
	}
	if len(bigRow.Big) != 8192 {
		t.Fatalf("单元格长度=%d，期望 8192", len(bigRow.Big))
	}

	// 步骤 9：status 真实指标。
	result, perr = call(t, conn, "db-postgres.status", `{"instance":"default"}`)
	assertNoSecret(t, result, e2ePassword)
	mustOK(t, "STEP9 db-postgres.status", result, perr)
	step9 := decode[statusResult](t, result)
	if !strings.Contains(step9.Version, "PostgreSQL 16.") {
		t.Fatalf("status 版本=%q", step9.Version)
	}
	if step9.Connections < 1 {
		t.Fatalf("连接数=%d，期望 ≥1（当前这次查询本身就是一个连接）", step9.Connections)
	}
	if step9.Databases < 1 {
		t.Fatalf("数据库数=%d，期望 ≥1", step9.Databases)
	}
	if step9.UptimeS <= 0 {
		t.Fatalf("运行时长=%ds，期望 >0", step9.UptimeS)
	}
	if step9.CacheHitRatio < 0 || step9.CacheHitRatio > 1 {
		t.Fatalf("缓存命中率=%v，期望 [0,1]", step9.CacheHitRatio)
	}
	if step9.BlocksHit+step9.BlocksRead <= 0 {
		t.Fatalf("pg_stat_database 块计数为空: %+v", step9)
	}

	// 步骤 10：任何响应里都不能出现口令。
	for _, raw := range [][]byte{result} {
		assertNoSecret(t, raw, e2ePassword)
	}

	// 收尾：删掉验证表，保证用例可重复运行。
	dockerExec(t, container, "psql", "-U", "postgres", "-d", "postgres", "-c", "DROP TABLE IF EXISTS "+e2eTable)
}

// JSON 形状（只声明断言需要的字段）。

type configGetResult struct {
	Schema struct {
		Title  string `json:"title"`
		Fields []struct {
			Key       string `json:"key"`
			Label     string `json:"label"`
			Type      string `json:"type"`
			Help      string `json:"help"`
			Sensitive bool   `json:"sensitive"`
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

type configSetResult struct {
	Saved           bool     `json:"saved"`
	Applied         bool     `json:"applied"`
	RestartRequired bool     `json:"restart_required"`
	Warnings        []string `json:"warnings"`
}

type instancesResult struct {
	Instances []struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Port      int    `json:"port"`
		Reachable bool   `json:"reachable"`
		Version   string `json:"version"`
		Error     string `json:"error"`
	} `json:"instances"`
	Count int `json:"count"`
}

type databasesResult struct {
	Instance  string `json:"instance"`
	Databases []struct {
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"databases"`
	Count int `json:"count"`
}

type queryResult struct {
	Columns   []string          `json:"columns"`
	Rows      []json.RawMessage `json:"rows"`
	RowCount  int               `json:"row_count"`
	Truncated bool              `json:"truncated"`
}

type statusResult struct {
	Instance      string  `json:"instance"`
	Version       string  `json:"version"`
	Connections   int64   `json:"connections"`
	Databases     int64   `json:"databases"`
	UptimeS       int64   `json:"uptime_s"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	BlocksHit     int64   `json:"blocks_hit"`
	BlocksRead    int64   `json:"blocks_read"`
}

// hasDatabase 判断库列表里是否有指定名字。
func hasDatabase(list []struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}, name string) bool {
	for _, database := range list {
		if database.Name == name {
			return true
		}
	}
	return false
}

// configFileMode 返回配置文件权限位（不存在时失败）。
func configFileMode(t *testing.T, stateDir string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(stateDir, "config.json"))
	if err != nil {
		t.Fatalf("stat 插件配置失败: %v", err)
	}
	return info.Mode().Perm()
}

// decode 把响应载荷解码成断言用的形状。
func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("解析结果失败: %v（原文 %s）", err, raw)
	}
	return value
}

// mustOK 断言调用成功，并把真实响应打进日志（E2E 证据）。
func mustOK(t *testing.T, label string, result json.RawMessage, perr *pluginapi.Error) {
	t.Helper()
	if perr != nil {
		t.Fatalf("%s 失败: %s: %s", label, perr.Code, perr.Message)
	}
	t.Logf("%s → %s", label, result)
}

// assertNoSecret 断言响应里没有口令（密码永不出现在任何对外载荷）。
func assertNoSecret(t *testing.T, raw json.RawMessage, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("响应泄漏了口令: %s", raw)
	}
}

// requireContainer 确认容器在跑（否则给出可执行的下一步）。
func requireContainer(t *testing.T, container string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("本机没有 docker，跳过真实 PostgreSQL E2E")
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", container).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "true") {
		t.Skipf("容器 %s 未运行：docker run -d --name %s -p %d:5432 -e POSTGRES_PASSWORD=*** postgres:16-alpine（%s%s）",
			container, container, e2ePort, strings.TrimSpace(string(out)), err)
	}
}

// seedTable 用容器内 psql 造一张验证表（插件只负责读）。
func seedTable(t *testing.T, container string) {
	t.Helper()
	dockerExec(t, container, "psql", "-U", "postgres", "-d", "postgres", "-c",
		"DROP TABLE IF EXISTS "+e2eTable)
	dockerExec(t, container, "psql", "-U", "postgres", "-d", "postgres", "-c",
		"CREATE TABLE "+e2eTable+" (id int PRIMARY KEY, name text NOT NULL)")
	dockerExec(t, container, "psql", "-U", "postgres", "-d", "postgres", "-c",
		"INSERT INTO "+e2eTable+" (id, name) VALUES (1, 'alpha'), (2, 'beta')")
}

// dockerExec 在容器内执行命令，失败即中止用例（E2E 不吞错误）。
func dockerExec(t *testing.T, container string, args ...string) {
	t.Helper()
	full := append([]string{"exec", container}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v 失败: %v\n%s", full, err, out)
	}
}

// buildPlugin 构建（或复用）真实插件二进制；黑盒测试不 mock 任何东西。
func buildPlugin(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	// 始终从源码构建：E2E 必须验证当前工作树的代码，不能复用 bin/ 里可能过期的二进制。
	root := ""
	for i := 0; i < 5; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			root = dir
			break
		}
		dir = filepath.Dir(dir)
	}
	if root == "" {
		t.Fatalf("未找到仓根（含 go.mod）")
	}
	built := filepath.Join(t.TempDir(), "db-postgres")
	cmd := exec.Command("go", "build", "-o", built, ".")
	cmd.Dir = root
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("构建 db-postgres 失败: %v\n%s", buildErr, out)
	}
	return built
}

// startPlugin 在临时 UDS 上拉起真实插件进程，并完成 kore.hello 握手。
func startPlugin(t *testing.T, binary, container, stateDir string) net.Conn {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "kore.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听临时 UDS 失败: %v", err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(isolatedEnv(), "KORE_PLUGIN_SOCK="+sock, "KORE_STATE_DIR="+stateDir,
		"KORE_PG_DOCKER_CONTAINER="+container, "KORE_LOG_LEVEL=error")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		t.Fatalf("启动插件失败: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = ln.Close()
	})
	conn, acceptErr := ln.Accept()
	if acceptErr != nil {
		t.Fatalf("接受插件连接失败: %v", acceptErr)
	}
	handshakeWith(t, conn)
	return conn
}

// isolatedEnv 去掉外部 KORE_PG_* 变量，保证用例只受自己设置的环境影响。
func isolatedEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "KORE_PG_") || strings.HasPrefix(item, "KORE_STATE_DIR=") {
			continue
		}
		env = append(env, item)
	}
	return env
}

// handshakeWith 扮演内核侧：读 kore.hello 请求并回 ok 响应。
func handshakeWith(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(callTimeout))
	payload, err := readFramePayload(conn)
	if err != nil {
		t.Fatalf("读 kore.hello 请求失败: %v", err)
	}
	if !strings.Contains(string(payload), `"method":"kore.hello"`) {
		t.Fatalf("首帧不是 kore.hello: %s", payload)
	}
	var request struct {
		ID    string `json:"id"`
		ReqID string `json:"req_id"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("解析 hello 请求失败: %v", err)
	}
	writeFrame(t, conn, map[string]any{
		"type": "response", "id": request.ID, "req_id": request.ReqID,
		"result": map[string]any{"ok": true, "protocol": "kore.plugin.v1"},
	})
	_ = conn.SetDeadline(time.Time{})
}

// call 发一个方法调用并读回响应。
func call(t *testing.T, conn net.Conn, method, params string) (json.RawMessage, *pluginapi.Error) {
	t.Helper()
	id := "id-" + method + "-" + strconv.Itoa(time.Now().Nanosecond())
	request := map[string]any{
		"type": "request", "id": id, "req_id": "r-" + id,
		"method": method, "params": json.RawMessage(params),
	}
	_ = conn.SetDeadline(time.Now().Add(callTimeout))
	writeFrame(t, conn, request)
	payload, err := readFramePayload(conn)
	if err != nil {
		t.Fatalf("%s 读响应失败: %v", method, err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("%s 解码响应失败: %v（%s）", method, err, payload)
	}
	response, ok := env.(*pluginapi.Response)
	if !ok {
		t.Fatalf("%s 响应帧类型=%T", method, env)
	}
	if response.ID != id {
		t.Fatalf("%s 响应 id=%q，期望 %q", method, response.ID, id)
	}
	_ = conn.SetDeadline(time.Time{})
	return response.Result, response.Error
}

// writeFrame 写一帧 kore.plugin.v1 消息（4 字节大端长度 + JSON 载荷）。
func writeFrame(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化帧失败: %v", err)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := conn.Write(append(header, payload...)); err != nil {
		t.Fatalf("写帧失败: %v", err)
	}
}

// readFramePayload 读一帧完整载荷。
func readFramePayload(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := readFull(conn, header); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(header))
	if n <= 0 || n > 1<<20 {
		return nil, fmt.Errorf("帧长 %d 超出合理范围", n)
	}
	payload := make([]byte, n)
	if _, err := readFull(conn, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// readFull 读满 len(buf) 字节。
func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
