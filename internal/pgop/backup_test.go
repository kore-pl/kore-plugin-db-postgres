package pgop

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// backupTestConfig 构造带备份目录的配置（目录落 t.TempDir，不碰真实状态目录）。
func backupTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig()
	cfg.ExportDir = t.TempDir()
	return cfg
}

// TestCallBackupDefaults 缺省参数：custom 归档 + compress 6 + 自动生成 <库>-<时间戳>.dump。
func TestCallBackupDefaults(t *testing.T) {
	cfg := backupTestConfig(t)
	exec := &fakeExec{}
	raw, perr := Call("db-postgres.backup", mustJSON(t, map[string]any{"database": "kore_app"}), cfg, exec)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var result BackupResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if result.Format != FormatCustom || result.Compression != "test" {
		t.Fatalf("格式/压缩不符合缺省口径: %+v", result)
	}
	if !strings.HasSuffix(result.FileName, ".dump") || !strings.HasPrefix(result.FileName, "kore_app-") {
		t.Fatalf("缺省文件名不合规: %s", result.FileName)
	}
	if !filepath.IsAbs(result.FilePath) || filepath.Dir(result.FilePath) != cfg.ExportDir {
		t.Fatalf("备份路径未落在备份目录内: %s (dir=%s)", result.FilePath, cfg.ExportDir)
	}
	if exec.backupSpec.Database != "kore_app" || exec.backupSpec.Compress != defaultCompression {
		t.Fatalf("执行面入参不对: %+v", exec.backupSpec)
	}
	if exec.backupSpec.SchemaOnly {
		t.Fatal("缺省不应是仅结构备份")
	}
}

// TestCallBackupParameterDefense 参数防线：每一种非法入参都必须 E_INVALID 且不触达执行面。
func TestCallBackupParameterDefense(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		hint   string
	}{
		{"缺少库名", map[string]any{}, "database"},
		{"库名非法", map[string]any{"database": "bad name"}, "库名"},
		{"格式非法", map[string]any{"database": "kore_app", "format": "tar"}, "format"},
		{"压缩越界", map[string]any{"database": "kore_app", "compress": 11}, "compress"},
		{"路径穿越", map[string]any{"database": "kore_app", "file_path": "../escape.dump"}, ".."},
		{"后缀不识别", map[string]any{"database": "kore_app", "file_path": "notes.txt"}, "结尾"},
		{"表名带通配符", map[string]any{"database": "kore_app", "tables": []string{"public.*"}}, "表名"},
		{"表名带分号", map[string]any{"database": "kore_app", "tables": []string{"t; DROP DATABASE x"}}, "表名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := backupTestConfig(t)
			exec := &fakeExec{}
			_, perr := Call("db-postgres.backup", mustJSON(t, c.params), cfg, exec)
			if perr == nil || perr.Code != pluginapi.E_INVALID {
				t.Fatalf("期望 E_INVALID，实际 %+v", perr)
			}
			if !strings.Contains(perr.Message, c.hint) {
				t.Fatalf("错误文案 %q 未含 %q（文案要能指导用户改参数）", perr.Message, c.hint)
			}
			if exec.backupCalls != 0 {
				t.Fatalf("参数被拒时不应触达执行面: %d 次", exec.backupCalls)
			}
		})
	}
}

// TestCallBackupRequiresAdmin 未声明 system:admin 时写方法一律 E_UNAUTHORIZED。
func TestCallBackupRequiresAdmin(t *testing.T) {
	cfg := backupTestConfig(t)
	cfg.Admin = false
	if _, perr := Call("db-postgres.backup", mustJSON(t, map[string]any{"database": "kore_app"}), cfg, &fakeExec{}); perr == nil || perr.Code != pluginapi.E_UNAUTHORIZED {
		t.Fatalf("期望 E_UNAUTHORIZED，实际 %+v", perr)
	}
}

// TestCallBackupNoExportDir 未配置备份目录时给出可操作提示，而不是写相对路径。
func TestCallBackupNoExportDir(t *testing.T) {
	cfg := testConfig() // ExportDir 为空
	cfg.ExportDir = ""
	if _, perr := Call("db-postgres.backup", mustJSON(t, map[string]any{"database": "kore_app"}), cfg, &fakeExec{}); perr == nil || !strings.Contains(perr.Message, "KORE_DB_EXPORT_DIR") {
		t.Fatalf("期望含 KORE_DB_EXPORT_DIR 的提示，实际 %+v", perr)
	}
}

// TestCallBackupSQLFormat 纯文本格式：压缩级别决定后缀，compress=0 时不加 .gz。
func TestCallBackupSQLFormat(t *testing.T) {
	for _, c := range []struct {
		name     string
		compress any
		suffix   string
	}{
		{"缺省压缩", nil, ".sql.gz"},
		{"显式 9", 9, ".sql.gz"},
		{"不压缩", 0, ".sql"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := backupTestConfig(t)
			exec := &fakeExec{}
			params := map[string]any{"database": "kore_app", "format": FormatSQL}
			if c.compress != nil {
				params["compress"] = c.compress
			}
			raw, perr := Call("db-postgres.backup", mustJSON(t, params), cfg, exec)
			if perr != nil {
				t.Fatalf("意外错误: %v", perr)
			}
			var result BackupResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatalf("解析结果失败: %v", err)
			}
			if !strings.HasSuffix(result.FileName, c.suffix) {
				t.Fatalf("文件名 %q 未以 %q 结尾", result.FileName, c.suffix)
			}
			if result.Format != FormatSQL {
				t.Fatalf("格式=%q，期望 sql", result.Format)
			}
		})
	}
}

// TestCallBackupRemovesPartialFile 执行面失败时不留半份文件。
func TestCallBackupRemovesPartialFile(t *testing.T) {
	cfg := backupTestConfig(t)
	exec := &fakeExec{backupErr: &OpError{Code: pluginapi.E_UPSTREAM, Message: "pg_dump 失败"}}
	_, perr := Call("db-postgres.backup", mustJSON(t, map[string]any{"database": "kore_app"}), cfg, exec)
	if perr == nil || perr.Code != pluginapi.E_UPSTREAM {
		t.Fatalf("期望 E_UPSTREAM，实际 %+v", perr)
	}
	entries, err := os.ReadDir(cfg.ExportDir)
	if err != nil {
		t.Fatalf("读取备份目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败后残留文件: %v", entries)
	}
}

// TestCallBackupRejectsOversize 超过 1 GiB 上限的产物会被删除并拒绝。
func TestCallBackupRejectsOversize(t *testing.T) {
	cfg := backupTestConfig(t)
	exec := &fakeExec{backupSize: maxBackupBytes + 1}
	if _, perr := Call("db-postgres.backup", mustJSON(t, map[string]any{"database": "kore_app"}), cfg, exec); perr == nil || !strings.Contains(perr.Message, "1 GiB") {
		t.Fatalf("期望超限提示，实际 %+v", perr)
	}
}

// writeBackupFile 在备份目录里落一个内容可辨识的备份文件。
func writeBackupFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("写入备份文件失败: %v", err)
	}
	return path
}

// TestCallRestoreRequiresConfirm confirm 缺失时必须在触达执行面之前拒绝。
func TestCallRestoreRequiresConfirm(t *testing.T) {
	cfg := backupTestConfig(t)
	path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", []byte("PGDMPfake-archive"))
	exec := &fakeExec{databases: []Database{{Name: "kore_app"}}}
	_, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
		"database": "kore_app", "file_path": path,
	}), cfg, exec)
	if perr == nil || perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "confirm=true") {
		t.Fatalf("期望 confirm 提示，实际 %+v", perr)
	}
	if exec.restoreSpec.FilePath != "" || exec.backupCalls != 0 {
		t.Fatal("未确认时不应触达执行面")
	}
}

// TestCallRestoreSnapshot 恢复前自动快照：缺省开启，显式 false 关闭。
func TestCallRestoreSnapshot(t *testing.T) {
	t.Run("缺省快照", func(t *testing.T) {
		cfg := backupTestConfig(t)
		path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", []byte("PGDMPfake-archive"))
		exec := &fakeExec{databases: []Database{{Name: "kore_app"}}}
		raw, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
			"database": "kore_app", "file_path": path, "confirm": true,
		}), cfg, exec)
		if perr != nil {
			t.Fatalf("意外错误: %v", perr)
		}
		var result RestoreResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("解析结果失败: %v", err)
		}
		if !result.Restored || result.Format != FormatCustom {
			t.Fatalf("恢复结果不对: %+v", result)
		}
		if result.Snapshot == nil || !strings.Contains(result.Snapshot.FilePath, "-prerestore-") {
			t.Fatalf("缺省应生成恢复前快照: %+v", result.Snapshot)
		}
		if exec.backupCalls != 1 {
			t.Fatalf("快照应调用一次 Backup，实际 %d", exec.backupCalls)
		}
		if exec.backupSpec.Database != "kore_app" || exec.backupSpec.Format != FormatCustom {
			t.Fatalf("快照入参不对: %+v", exec.backupSpec)
		}
		if exec.restoreSpec.Format != FormatCustom || exec.restoreSpec.Gzip {
			t.Fatalf("恢复入参不对: %+v", exec.restoreSpec)
		}
	})

	t.Run("关闭快照", func(t *testing.T) {
		cfg := backupTestConfig(t)
		path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", []byte("PGDMPfake-archive"))
		exec := &fakeExec{databases: []Database{{Name: "kore_app"}}}
		raw, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
			"database": "kore_app", "file_path": path, "confirm": true, "snapshot": false,
		}), cfg, exec)
		if perr != nil {
			t.Fatalf("意外错误: %v", perr)
		}
		var result RestoreResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("解析结果失败: %v", err)
		}
		if result.Snapshot != nil || exec.backupCalls != 0 {
			t.Fatalf("关闭快照后不应备份: snapshot=%+v calls=%d", result.Snapshot, exec.backupCalls)
		}
	})
}

// TestCallRestoreMissingDatabase 目标库不存在：拒绝并给可操作提示；勾选自动建库才继续。
func TestCallRestoreMissingDatabase(t *testing.T) {
	cfg := backupTestConfig(t)
	path := writeBackupFile(t, cfg.ExportDir, "brandnew-20260101-000000.dump", []byte("PGDMPfake-archive"))
	exec := &fakeExec{databases: []Database{{Name: "postgres"}}}
	_, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
		"database": "brandnew", "file_path": path, "confirm": true,
	}), cfg, exec)
	if perr == nil || perr.Code != pluginapi.E_INVALID || !strings.Contains(perr.Message, "不存在") {
		t.Fatalf("期望库不存在提示，实际 %+v", perr)
	}

	exec2 := &fakeExec{databases: []Database{{Name: "postgres"}}, created: true}
	raw, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
		"database": "brandnew", "file_path": path, "confirm": true, "create_database": true,
	}), cfg, exec2)
	if perr != nil {
		t.Fatalf("自动建库后应成功: %v", perr)
	}
	var result RestoreResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if !result.CreatedDB || result.Snapshot != nil || exec2.backupCalls != 0 {
		t.Fatalf("新建库没有可快照对象，不应快照: %+v calls=%d", result, exec2.backupCalls)
	}
}

// TestCallRestoreSniffFormat 不信任扩展名：gzip 包裹的纯文本按 sql + gzip 恢复。
func TestCallRestoreSniffFormat(t *testing.T) {
	cfg := backupTestConfig(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("CREATE TABLE t (id int);\n")); err != nil {
		t.Fatalf("gzip 写入失败: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip 关闭失败: %v", err)
	}
	path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.sql.gz", buf.Bytes())
	exec := &fakeExec{databases: []Database{{Name: "kore_app"}}}
	if _, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
		"database": "kore_app", "file_path": path, "confirm": true,
	}), cfg, exec); perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if exec.restoreSpec.Format != FormatSQL || !exec.restoreSpec.Gzip {
		t.Fatalf("格式判定不对: %+v", exec.restoreSpec)
	}
}

// TestCallBackupList 列表只认备份后缀、按修改时间倒序、limit 生效。
func TestCallBackupList(t *testing.T) {
	cfg := backupTestConfig(t)
	writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", []byte("PGDMPaaa"))
	writeBackupFile(t, cfg.ExportDir, "kore_app-20260102-000000.sql.gz", []byte("not-really-gzip"))
	writeBackupFile(t, cfg.ExportDir, "mysql-20260101-000000.sql", []byte("-- MySQL dump 10.13  Distrib 8.0\n-- Host: localhost\n"))
	writeBackupFile(t, cfg.ExportDir, "notes.txt", []byte("hello"))

	raw, perr := Call("db-postgres.backupList", mustJSON(t, map[string]any{}), cfg, nil)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var result BackupListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("应只列出 2 个备份文件，实际 %d: %+v", result.Count, result.Backups)
	}
	for _, file := range result.Backups {
		if file.Format == "" || file.SizeBytes == 0 || file.ModifiedAt == "" {
			t.Fatalf("列表条目字段不全: %+v", file)
		}
	}

	// 按库名过滤 + limit。
	raw, perr = Call("db-postgres.backupList", mustJSON(t, map[string]any{"database": "kore_app", "limit": 1}), cfg, nil)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if len(result.Backups) != 1 || !strings.HasPrefix(result.Backups[0].FileName, "kore_app-") {
		t.Fatalf("过滤/限量失效: %+v", result.Backups)
	}
}

// TestCallBackupListEmptyDir 目录不存在时返回空列表 + 下一步提示，不报错。
func TestCallBackupListEmptyDir(t *testing.T) {
	cfg := testConfig()
	cfg.ExportDir = filepath.Join(t.TempDir(), "not-created")
	raw, perr := Call("db-postgres.backupList", mustJSON(t, map[string]any{}), cfg, nil)
	if perr != nil {
		t.Fatalf("目录不存在不应报错: %v", perr)
	}
	var result BackupListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if result.Count != 0 || result.Hint == "" {
		t.Fatalf("空态应给 hint: %+v", result)
	}
}

// TestCallBackupDelete 删除必须 confirm、只允许备份后缀、越界路径拒绝。
func TestCallBackupDelete(t *testing.T) {
	cfg := backupTestConfig(t)
	path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", []byte("PGDMPaaa"))
	other := writeBackupFile(t, cfg.ExportDir, "notes.txt", []byte("keep"))

	if _, perr := Call("db-postgres.backupDelete", mustJSON(t, map[string]any{"file_path": path}), cfg, nil); perr == nil || !strings.Contains(perr.Message, "confirm=true") {
		t.Fatalf("缺 confirm 应拒绝: %+v", perr)
	}
	if _, perr := Call("db-postgres.backupDelete", mustJSON(t, map[string]any{"file_path": other, "confirm": true}), cfg, nil); perr == nil {
		t.Fatal("非备份后缀应拒绝删除")
	}
	if _, perr := Call("db-postgres.backupDelete", mustJSON(t, map[string]any{"file_path": "../../etc/passwd", "confirm": true}), cfg, nil); perr == nil {
		t.Fatal("越界路径应拒绝")
	}
	raw, perr := Call("db-postgres.backupDelete", mustJSON(t, map[string]any{"file_path": path, "confirm": true}), cfg, nil)
	if perr != nil {
		t.Fatalf("意外错误: %v", perr)
	}
	var result BackupDeleteResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if !result.Deleted || result.SizeBytes != int64(len("PGDMPaaa")) {
		t.Fatalf("删除结果不对: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("文件应已删除")
	}
}

// TestCallBackupDownload 分块下载：base64 内容可还原、EOF 判定正确、越界 offset 拒绝。
func TestCallBackupDownload(t *testing.T) {
	cfg := backupTestConfig(t)
	content := make([]byte, 10)
	for i := range content {
		content[i] = byte('a' + i)
	}
	path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", content)

	var collected []byte
	offset := int64(0)
	for {
		raw, perr := Call("db-postgres.backupDownload", mustJSON(t, map[string]any{
			"file_path": path, "offset": offset, "limit": 4,
		}), cfg, nil)
		if perr != nil {
			t.Fatalf("意外错误: %v", perr)
		}
		var result BackupDownloadResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("解析结果失败: %v", err)
		}
		chunk, err := base64.StdEncoding.DecodeString(result.DataBase64)
		if err != nil {
			t.Fatalf("base64 解码失败: %v", err)
		}
		if result.ChunkBytes != len(chunk) || result.NextOffset != offset+int64(len(chunk)) {
			t.Fatalf("分块元信息不对: %+v", result)
		}
		collected = append(collected, chunk...)
		offset = result.NextOffset
		if result.EOF {
			break
		}
	}
	if !bytes.Equal(collected, content) {
		t.Fatalf("下载内容与文件不一致: %q vs %q", collected, content)
	}

	if _, perr := Call("db-postgres.backupDownload", mustJSON(t, map[string]any{
		"file_path": path, "offset": 100,
	}), cfg, nil); perr == nil || !strings.Contains(perr.Message, "offset") {
		t.Fatalf("offset 越界应拒绝: %+v", perr)
	}
	if _, perr := Call("db-postgres.backupDownload", mustJSON(t, map[string]any{
		"file_path": path, "limit": MaxDownloadChunk + 1,
	}), cfg, nil); perr == nil {
		t.Fatal("limit 超上限应拒绝")
	}
}

// TestDumpArgsArgvShape 固定 argv：选项位不含用户输入，库名只作最后的位置参数。
func TestDumpArgsArgvShape(t *testing.T) {
	spec := BackupSpec{Database: "kore_app", Tables: []string{"public.orders"}, Format: FormatCustom, Compress: 6}
	args := dumpArgs(spec, psqlTarget{host: "127.0.0.1", port: 5432, database: defaultDatabase}, "postgres")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--format=custom", "--compress=6", "--table=public.orders", "-h 127.0.0.1", "-p 5432", "-U postgres"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv 缺少 %q: %v", want, args)
		}
	}
	if args[len(args)-1] != "kore_app" {
		t.Fatalf("库名必须是最后一个位置参数（所有版本都支持）: %v", args)
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, ";|&$()") {
			t.Fatalf("argv 出现 shell 元字符（不应经过 shell）: %q", arg)
		}
	}

	plain := dumpArgs(BackupSpec{Database: "kore_app", Format: FormatSQL, Compress: 9}, psqlTarget{}, "postgres")
	if !strings.Contains(strings.Join(plain, " "), "--format=plain --clean --if-exists") {
		t.Fatalf("纯文本备份应带 --clean --if-exists: %v", plain)
	}
}

// TestValidateTablePattern 表名白名单：允许 schema.table，拒绝通配符与 SQL 片段。
func TestValidateTablePattern(t *testing.T) {
	for _, ok := range []string{"orders", "public.orders", "Kore_App", "s1.t2"} {
		if err := validateTablePattern(ok); err != nil {
			t.Fatalf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "*", "public.*", "a.b.c", "orders; DROP TABLE x", "a b", strings.Repeat("x", 64)} {
		if err := validateTablePattern(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

// TestSnapshotSurvivesFailedRestore 恢复失败时不删快照（快照是回滚点）。
func TestSnapshotSurvivesFailedRestore(t *testing.T) {
	cfg := backupTestConfig(t)
	path := writeBackupFile(t, cfg.ExportDir, "kore_app-20260101-000000.dump", []byte("PGDMPfake-archive"))
	exec := &fakeExec{
		databases:  []Database{{Name: "kore_app"}},
		restoreErr: &OpError{Code: pluginapi.E_UPSTREAM, Message: "pg_restore 失败"},
	}
	_, perr := Call("db-postgres.restore", mustJSON(t, map[string]any{
		"database": "kore_app", "file_path": path, "confirm": true,
	}), cfg, exec)
	if perr == nil || perr.Code != pluginapi.E_UPSTREAM {
		t.Fatalf("期望 E_UPSTREAM，实际 %+v", perr)
	}
	if exec.backupCalls != 1 {
		t.Fatalf("失败前应已做快照，实际 Backup 调用 %d 次", exec.backupCalls)
	}
}
