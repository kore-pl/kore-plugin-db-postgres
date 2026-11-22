package plugincfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStoreWriteMode0600 配置写入后权限是 0600，缺失的状态目录按 0700 补建。
func TestStoreWriteMode0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins", "db-postgres")
	store := NewStore(dir)
	if store.Exists() {
		t.Fatal("新目录不应有配置文件")
	}
	if err := store.Write(json.RawMessage(`{"host":"127.0.0.1"}`)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("文件权限=%o，期望 600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat 目录失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("目录权限=%o，期望 700", perm)
	}

	// 覆盖写保持 0600，且读到的是新值。
	if err := store.Write(json.RawMessage(`{"host":"10.0.0.1"}`)); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	raw, exists, err := store.Read()
	if err != nil || !exists {
		t.Fatalf("读取失败: %v exists=%v", err, exists)
	}
	if string(raw) != `{"host":"10.0.0.1"}` {
		t.Fatalf("读到旧值: %s", raw)
	}
}

// TestStoreRejectsInvalidJSON 非 JSON 对象拒绝写入 / 读取报错。
func TestStoreRejectsInvalidJSON(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, raw := range []string{``, `[]`, `{`, `not-json`} {
		if err := store.Write(json.RawMessage(raw)); err == nil {
			t.Fatalf("非法内容 %q 未被拒", raw)
		}
	}
	if err := os.WriteFile(store.Path(), []byte(`[1,2]`), 0o600); err != nil {
		t.Fatalf("准备坏文件失败: %v", err)
	}
	if _, _, err := store.Read(); err == nil {
		t.Fatal("读非对象配置未报错")
	}
}

// TestStoreWithoutStateDir 未配置状态目录时给出明确错误，而不是写相对路径。
func TestStoreWithoutStateDir(t *testing.T) {
	store := NewStore("")
	if store.Exists() {
		t.Fatal("无状态目录不应报存在")
	}
	if err := store.Write(json.RawMessage(`{}`)); err == nil {
		t.Fatal("无状态目录竟然写成功")
	}
	if _, _, err := store.Read(); err == nil {
		t.Fatal("无状态目录竟然读成功")
	}
}

// TestMaskValues 敏感字段掩码为 ***，其余字段原样保留。
func TestMaskValues(t *testing.T) {
	schema := Schema{Title: "t", Fields: []Field{
		{Key: "host", Label: "h", Type: TypeString, Help: "x", Default: json.RawMessage(`""`)},
		{Key: "password", Label: "p", Type: TypeString, Help: "x", Default: json.RawMessage(`""`), Sensitive: true},
	}}
	masked, err := MaskValues(schema, json.RawMessage(`{"host":"127.0.0.1","password":"secret"}`))
	if err != nil {
		t.Fatalf("掩码失败: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(masked, &got); err != nil {
		t.Fatalf("解析掩码结果失败: %v", err)
	}
	if got["password"] != SensitivePlaceholder || got["host"] != "127.0.0.1" {
		t.Fatalf("掩码结果异常: %s", masked)
	}
	if !IsSensitivePlaceholder(json.RawMessage(`"***"`)) {
		t.Fatal("*** 未被识别为占位符")
	}
	if IsSensitivePlaceholder(json.RawMessage(`"real"`)) {
		t.Fatal("明文被误判为占位符")
	}
}

// TestValidateSchema 契约枚举校验：缺字段、未知类型、select 缺 options 都要报错。
func TestValidateSchema(t *testing.T) {
	good := Schema{Title: "t", Fields: []Field{
		{Key: "a", Label: "A", Type: TypeInt, Help: "x", Default: json.RawMessage(`1`)},
		{Key: "b", Label: "B", Type: TypeBool, Help: "x", Default: json.RawMessage(`false`)},
		{Key: "c", Label: "C", Type: TypeSelect, Help: "x", Default: json.RawMessage(`"s"`), Options: []string{"s", "t"}},
		{Key: "d", Label: "D", Type: TypeList, Item: TypeString, Help: "x", Default: json.RawMessage(`[]`)},
	}}
	if err := ValidateSchema(good); err != nil {
		t.Fatalf("合法 schema 被拒: %v", err)
	}
	bad := []Schema{
		{Title: "", Fields: good.Fields},
		{Title: "t"},
		{Title: "t", Fields: []Field{{Key: "", Label: "A", Type: TypeString, Help: "x", Default: json.RawMessage(`""`)}}},
		{Title: "t", Fields: []Field{{Key: "a", Label: "A", Type: "object", Help: "x", Default: json.RawMessage(`{}`)}}},
		{Title: "t", Fields: []Field{{Key: "a", Label: "A", Type: TypeSelect, Help: "x", Default: json.RawMessage(`"s"`)}}},
		{Title: "t", Fields: []Field{{Key: "a", Label: "A", Type: TypeList, Item: "object", Help: "x", Default: json.RawMessage(`[]`)}}},
	}
	for i, schema := range bad {
		if err := ValidateSchema(schema); err == nil {
			t.Fatalf("非法 schema #%d 未被拒", i)
		}
	}
}
