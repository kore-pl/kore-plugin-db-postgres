package pgop

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateDatabaseName 库名白名单严格按 ^[a-zA-Z0-9_]{1,63}$。
func TestValidateDatabaseName(t *testing.T) {
	ok := []string{"postgres", "kore_e2e", "A1_b9", strings.Repeat("a", 63)}
	for _, name := range ok {
		if err := ValidateDatabaseName(name); err != nil {
			t.Fatalf("合法库名被拒: %q → %v", name, err)
		}
	}
	bad := []string{
		"", strings.Repeat("a", 64), "a-b", "a.b", "a b", `a"b`, "a'b",
		"a;drop", "a$b", "ümlaut", "-lead", "a\\b", "a\nb", "a\x00b",
	}
	for _, name := range bad {
		if err := ValidateDatabaseName(name); err == nil {
			t.Fatalf("非法库名未被拒: %q", name)
		}
	}
}

// TestValidateInstanceConfig 实例名 / 主机 / 用户名 / 密码环境变量。
func TestValidateInstanceConfig(t *testing.T) {
	okNames := []string{"default", "primary", "pg-01", "a.b", "x1"}
	for _, name := range okNames {
		if err := ValidateInstanceName(name); err != nil {
			t.Fatalf("合法实例名被拒: %q → %v", name, err)
		}
	}
	badNames := []string{"", "Default", "../etc", "a/b", "a b", "-x", strings.Repeat("a", 65)}
	for _, name := range badNames {
		if err := ValidateInstanceName(name); err == nil {
			t.Fatalf("非法实例名未被拒: %q", name)
		}
	}

	if err := ValidateHost("127.0.0.1"); err != nil {
		t.Fatalf("合法 host 被拒: %v", err)
	}
	if err := ValidateHost("db.internal-1"); err != nil {
		t.Fatalf("合法 host 被拒: %v", err)
	}
	// "-h" 形式的 host 会被 psql 当成选项，必须拒绝。
	for _, host := range []string{"", "-oProxyCommand=sh", "a b", "a;b", "a|b"} {
		if err := ValidateHost(host); err == nil {
			t.Fatalf("非法 host 未被拒: %q", host)
		}
	}

	if err := ValidateUser("postgres"); err != nil {
		t.Fatalf("合法 user 被拒: %v", err)
	}
	for _, user := range []string{"", "-U", "a b", "a;b"} {
		if err := ValidateUser(user); err == nil {
			t.Fatalf("非法 user 未被拒: %q", user)
		}
	}

	if err := ValidatePasswordEnv("KORE_PG_PASSWORD"); err != nil {
		t.Fatalf("合法 password_env 被拒: %v", err)
	}
	for _, name := range []string{"", "1ABC", "A-B", "A B"} {
		if err := ValidatePasswordEnv(name); err == nil {
			t.Fatalf("非法 password_env 未被拒: %q", name)
		}
	}

	if err := ValidatePassword(strings.Repeat("p", maxPasswordBytes)); err != nil {
		t.Fatalf("上限内密码被拒: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("p", maxPasswordBytes+1)); err == nil {
		t.Fatal("超长密码未被拒")
	}
	if err := ValidatePassword("a\x00b"); err == nil {
		t.Fatal("含 NUL 密码未被拒")
	}
}

// TestParseInstances 多实例配置：缺省值补齐、未知字段拒绝、重名拒绝。
func TestParseInstances(t *testing.T) {
	list, err := ParseInstances([]byte(`[{"name":"primary","host":"127.0.0.1","port":15432,"user":"postgres","password_env":"PG_PW"}]`))
	if err != nil {
		t.Fatalf("合法配置解析失败: %v", err)
	}
	if len(list) != 1 || list[0].Port != 15432 || list[0].Name != "primary" {
		t.Fatalf("解析结果异常: %+v", list)
	}

	list, err = ParseInstances([]byte(`[{"name":"a"}]`))
	if err != nil {
		t.Fatalf("缺省值补齐失败: %v", err)
	}
	if list[0].Host != "127.0.0.1" || list[0].Port != 5432 || list[0].User != "postgres" {
		t.Fatalf("缺省值不对: %+v", list[0])
	}

	// 未知字段必须拒绝：避免 password 明文被静默忽略后误以为生效。
	if _, err := ParseInstances([]byte(`[{"name":"a","password":"plain"}]`)); err == nil {
		t.Fatal("含 password 明文的多实例配置未被拒")
	}
	if _, err := ParseInstances([]byte(`[{"name":"a"},{"name":"a"}]`)); err == nil {
		t.Fatal("重名实例未被拒")
	}
	if _, err := ParseInstances([]byte(`[] extra`)); err == nil {
		t.Fatal("尾部多余内容未被拒")
	}
}

// TestInstancePassword 密码优先级：配置文件密码 > password_env > 空。
func TestInstancePassword(t *testing.T) {
	t.Setenv("PGOP_TEST_PW", "from-env")
	if got := instancePassword(InstanceConfig{Password: "from-file", PasswordEnv: "PGOP_TEST_PW"}); got != "from-file" {
		t.Fatalf("密码=%q，期望 from-file", got)
	}
	if got := instancePassword(InstanceConfig{PasswordEnv: "PGOP_TEST_PW"}); got != "from-env" {
		t.Fatalf("密码=%q，期望 from-env", got)
	}
	// 没有密码是合法配置（本机 trust/peer），不报错、不返回哨兵值。
	if got := instancePassword(InstanceConfig{}); got != "" {
		t.Fatalf("密码=%q，期望空串", got)
	}
}

// TestInstanceConfigJSONNoPassword 实例配置序列化后永不包含密码字段。
func TestInstanceConfigJSONNoPassword(t *testing.T) {
	data, err := json.Marshal(InstanceConfig{Name: "default", Host: "127.0.0.1", Port: 5432, User: "postgres", Password: "s3cret"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(data), "s3cret") || strings.Contains(string(data), `"password":`) {
		t.Fatalf("实例配置泄漏了密码: %s", data)
	}
}
