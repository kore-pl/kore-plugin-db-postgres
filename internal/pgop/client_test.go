package pgop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// TestPSQLArgsNoPassword 密码绝不进 argv：psql 参数里不能出现口令，口令只走环境变量。
func TestPSQLArgsNoPassword(t *testing.T) {
	const secret = "t0p-s3cret-pw"
	args := psqlArgs(psqlTarget{host: "127.0.0.1", port: 15432, database: "postgres"}, "postgres", "SELECT 1")
	for _, arg := range args {
		if strings.Contains(arg, secret) || strings.Contains(arg, "PGPASSWORD") {
			t.Fatalf("psql argv 含密码相关项: %q", arg)
		}
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--no-psqlrc", "--csv", "-v ON_ERROR_STOP=1", "null=" + nullMarker, "-h 127.0.0.1", "-p 15432", "-U postgres", "-d postgres"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("psql argv 缺少 %q: %s", want, joined)
		}
	}

	env := psqlEnv(secret, 5*time.Second)
	if !containsString(env, "PGPASSWORD="+secret) {
		t.Fatalf("PGPASSWORD 未注入环境变量: %v", env)
	}
	if !containsString(env, "PGCONNECT_TIMEOUT=3") {
		t.Fatalf("缺少 PGCONNECT_TIMEOUT=3: %v", env)
	}
	// 无密码时不应注入空 PGPASSWORD（否则会覆盖本机 .pgpass / trust 行为）。
	if containsString(psqlEnv("", 5*time.Second), "PGPASSWORD=") {
		t.Fatalf("空密码仍注入了 PGPASSWORD: %v", psqlEnv("", 5*time.Second))
	}
}

// TestDockerExecArgsNoPassword docker exec 的 argv 同样不含密码（密码从 stdin 进容器）。
func TestDockerExecArgsNoPassword(t *testing.T) {
	const secret = "t0p-s3cret-pw"
	args := dockerExecArgs("kore-pg-test", psqlArgs(psqlTarget{host: "127.0.0.1", port: 5432, database: "postgres"}, "postgres", "SELECT version()"))
	for i, arg := range args {
		if strings.Contains(arg, secret) {
			t.Fatalf("docker argv[%d] 含密码: %q", i, arg)
		}
	}
	if args[0] != "exec" || args[1] != "-i" || args[2] != "kore-pg-test" {
		t.Fatalf("docker 前缀异常: %v", args[:3])
	}
	// 脚本必须只从 stdin 读口令，且用 "$@" 承接 psql 参数（不拼 SQL）。
	// args = exec -i <容器> sh -c <脚本> sh <psql 参数…>
	if len(args) < 6 || args[3] != "sh" || args[4] != "-c" || args[5] != dockerPsqlScript {
		t.Fatalf("docker 参数形状异常: %v", args[:6])
	}
	script := args[5]
	if !strings.Contains(script, `read -r pw`) || !strings.Contains(script, `exec psql "$@"`) {
		t.Fatalf("容器内脚本不符合约定: %q", script)
	}
	if strings.Contains(script, "SELECT") {
		t.Fatalf("SQL 被拼进了容器内脚本: %q", script)
	}
}

// TestParseCSV psql --csv 解析：NULL 标记、数字、字符串、内嵌逗号/制表符/换行。
func TestParseCSV(t *testing.T) {
	stdout := "n,nn,text,multi\n" +
		"1," + nullMarker + ",hello,\"a,b\"\n" +
		"2.5," + nullMarker + ",\"tab\there\",\"line1\nline2\"\n"
	columns, rows, truncated, err := parseCSV([]byte(stdout), 10, maxCellBytes)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if truncated {
		t.Fatal("未超限却被标记 truncated")
	}
	if len(columns) != 4 || columns[0] != "n" || columns[3] != "multi" {
		t.Fatalf("列名解析异常: %v", columns)
	}
	if len(rows) != 2 {
		t.Fatalf("行数=%d，期望 2", len(rows))
	}
	if rows[1][2] != "tab\there" || rows[1][3] != "line1\nline2" {
		t.Fatalf("内嵌特殊字符解析异常: %q / %q", rows[1][2], rows[1][3])
	}

	row, err := buildRow(columns, rows[0])
	if err != nil {
		t.Fatalf("编码行失败: %v", err)
	}
	if string(row) != `{"n":1,"nn":null,"text":"hello","multi":"a,b"}` {
		t.Fatalf("行 JSON=%s", row)
	}
	row, err = buildRow(columns, rows[1])
	if err != nil {
		t.Fatalf("编码行失败: %v", err)
	}
	if !strings.Contains(string(row), `"n":2.5`) {
		t.Fatalf("小数未按数字编码: %s", row)
	}
}

// TestLooksNumeric 只有合法 JSON 数字才按数字输出，避免 "007" 产出非法 JSON。
func TestLooksNumeric(t *testing.T) {
	// JSON 允许值尾随空白，因此 "1 " 仍是合法数字文档（解析后是 1）。
	yes := []string{"1", "-1", "2.5", "1e3", "-1.5e-3", "0", "1 "}
	no := []string{"", "007", "1,000", "abc", "t", "f", "NULL", "1.2.3", "--1"}
	for _, cell := range yes {
		if !looksNumeric(cell) {
			t.Fatalf("%q 应判为数字", cell)
		}
	}
	for _, cell := range no {
		if looksNumeric(cell) {
			t.Fatalf("%q 不应判为数字", cell)
		}
	}
}

// TestParseCSVTruncation 行数上限与单格字节上限都要落到 truncated 标记上。
func TestParseCSVTruncation(t *testing.T) {
	_, rows, truncated, err := parseCSV([]byte("a\n1\n2\n3\n"), 2, maxCellBytes)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("行数=%d truncated=%v，期望 2/true", len(rows), truncated)
	}

	long := strings.Repeat("x", maxCellBytes+100)
	_, rows, truncated, err = parseCSV([]byte("a\n"+long+"\n"), 1, maxCellBytes)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !truncated {
		t.Fatal("超长单元格未标记 truncated")
	}
	if len(rows[0][0]) != maxCellBytes {
		t.Fatalf("单格截断长度=%d，期望 %d", len(rows[0][0]), maxCellBytes)
	}
}

// TestParseCSVUTF8Truncation 截断不能切出半个多字节字符。
func TestParseCSVUTF8Truncation(t *testing.T) {
	// 每个汉字 3 字节：limit=4 → 只能保留 1 个汉字（3 字节）。
	cut := truncateUTF8("中文数据", 4)
	if cut != "中" {
		t.Fatalf("截断结果=%q，期望 中", cut)
	}
	if !json.Valid([]byte(`"` + cut + `"`)) {
		t.Fatalf("截断后不是合法 UTF-8 JSON 字符串: %q", cut)
	}
}

// TestNormalizeColumns 重复列名去重，保证 JSON 键唯一。
func TestNormalizeColumns(t *testing.T) {
	columns := normalizeColumns([]string{"a", "a", "a", ""})
	if columns[0] != "a" || columns[1] != "a_2" || columns[2] != "a_3" || columns[3] != "column_4" {
		t.Fatalf("列名归一异常: %v", columns)
	}
}

// TestDockerTargetMapping docker 回退的本机端口映射：宿主机发布端口 → 容器内 5432。
func TestDockerTargetMapping(t *testing.T) {
	backend := &backend{kind: kindDocker, container: "kore-pg-test"}
	target, perr := backend.dockerTarget(InstanceConfig{Name: "default", Host: "127.0.0.1", Port: 5432}, defaultDatabase)
	if perr != nil {
		t.Fatalf("5432 直连不该报错: %v", perr)
	}
	if target.host != "127.0.0.1" || target.port != 5432 || target.database != "postgres" {
		t.Fatalf("目标映射异常: %+v", target)
	}
	// 业务库名随 docker 目标一起映射，不再固定 postgres。
	target, perr = backend.dockerTarget(InstanceConfig{Name: "default", Host: "127.0.0.1", Port: 5432}, "myapp")
	if perr != nil || target.database != "myapp" {
		t.Fatalf("业务库映射异常: %+v / %v", target, perr)
	}
	// 远程库按原样透传，不做本机映射。
	target, perr = backend.dockerTarget(InstanceConfig{Name: "remote", Host: "db.example.com", Port: 6432}, defaultDatabase)
	if perr != nil {
		t.Fatalf("远程库不该报错: %v", perr)
	}
	if target.host != "db.example.com" || target.port != 6432 {
		t.Fatalf("远程目标被改写: %+v", target)
	}
}

// TestTargetForDatabase 目标库构造的两条通道：psql 直连用实例地址 + 给定库；
// docker 回退也带业务库；空串回落维护库；非法库名 E_INVALID 且不带目标。
func TestTargetForDatabase(t *testing.T) {
	instance := InstanceConfig{Name: "default", Host: "127.0.0.1", Port: 15432, User: "postgres"}
	psqlBackend := &backend{kind: kindPsql}
	target, perr := (&Client{}).targetFor(instance, psqlBackend, "myapp")
	if perr != nil || target.database != "myapp" || target.host != "127.0.0.1" || target.port != 15432 {
		t.Fatalf("psql 业务库目标异常: %+v / %v", target, perr)
	}
	// 缺省回落。
	target, perr = (&Client{}).targetFor(instance, psqlBackend, "")
	if perr != nil || target.database != defaultDatabase {
		t.Fatalf("缺省库回落异常: %+v / %v", target, perr)
	}
	// 注入：psql 元命令 / 空白 / 分号 / 选项前缀全部在目标构造处拒绝。
	for _, bad := range []string{"myapp;DROP TABLE x", "my app", "-d postgres", `my"app`, "a\\connect x", "x' OR 1=1"} {
		_, perr := (&Client{}).targetFor(instance, psqlBackend, bad)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("非法库名 %q 未被 E_INVALID 拒绝: %v", bad, perr)
		}
	}
	// psql -d argv 真正带上了业务库（不是只改了结构体）。
	appTarget, perr := (&Client{}).targetFor(instance, psqlBackend, "myapp")
	if perr != nil {
		t.Fatalf("业务库目标构造失败: %v", perr)
	}
	args := psqlArgs(appTarget, instance.User, "SELECT 1")
	if !containsString(args, "myapp") || !strings.Contains(strings.Join(args, " "), "-d myapp") {
		t.Fatalf("psql argv 未带 -d myapp: %v", args)
	}
}

// TestResolveDatabaseWhitespace 空白只做 trim，不静默改写有效库名；非法名 E_INVALID。
func TestResolveDatabaseWhitespace(t *testing.T) {
	for _, valid := range []string{"myapp", " myapp ", "Demo_01"} {
		got, perr := resolveDatabase(valid)
		if perr != nil || got != strings.TrimSpace(valid) {
			t.Fatalf("库名 %q 解析异常: %q / %v", valid, got, perr)
		}
	}
	for _, bad := range []string{"my app", "app;DROP", "-x", "a-b", "a.b", `a"b`, "a'b", strings.Repeat("a", 64)} {
		_, perr := resolveDatabase(bad)
		if perr == nil || perr.Code != pluginapi.E_INVALID {
			t.Fatalf("非法库名 %q 未被拒: %v", bad, perr)
		}
	}
}

// TestFormatPublished 端口摘要稳定排序（错误信息可复现）。
func TestFormatPublished(t *testing.T) {
	got := formatPublished(map[int]int{5432: 15432, 5433: 25432})
	if got != "5432→15432, 5433→25432" {
		t.Fatalf("摘要=%q", got)
	}
	if formatPublished(nil) != "无" {
		t.Fatal("空映射摘要异常")
	}
}

// TestSanitizeStderr 错误输出脱敏：口令变成 ***，NOTICE 噪声被丢弃，长度受限。
func TestSanitizeStderr(t *testing.T) {
	const secret = "p@ssw0rd"
	stderr := "NOTICE:  noise\npsql: error: connection failed: password authentication failed for user \"postgres\" with p@ssw0rd\n"
	got := sanitizeStderr(stderr, secret)
	if strings.Contains(got, secret) {
		t.Fatalf("错误输出未脱敏: %q", got)
	}
	if strings.Contains(got, "NOTICE") {
		t.Fatalf("NOTICE 噪声未过滤: %q", got)
	}
	if !strings.Contains(got, "password authentication failed") {
		t.Fatalf("真实错误被吞: %q", got)
	}
	long := sanitizeStderr(strings.Repeat("x", 5000), "")
	if len([]rune(long)) > 501 {
		t.Fatalf("错误输出未截断: %d 字符", len([]rune(long)))
	}
}

// containsString 判断切片是否包含给定元素。
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestBuildRowsBudget 总字节预算耗尽时停止追加并标记截断。
func TestBuildRowsBudget(t *testing.T) {
	columns := []string{"a"}
	cells := [][]string{{"1"}, {"2"}, {"3"}}
	rows, cut, err := buildRows(columns, cells, 14)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !cut || len(rows) >= len(cells) {
		t.Fatalf("预算截断异常: rows=%d cut=%v", len(rows), cut)
	}
	for _, row := range rows {
		if !json.Valid(row) {
			t.Fatalf("行不是合法 JSON: %s", row)
		}
	}
}
