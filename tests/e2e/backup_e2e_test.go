// Package e2e 的备份/恢复真机用例：真实 pg_dump / pg_restore / psql，不 mock 任何一层。
//
// 默认跳过；需要先启动容器（本机没有 psql 时插件会自动回退 docker exec）：
//
//	docker run -d --name kore-pg-test -p 15432:5432 -e POSTGRES_PASSWORD=*** postgres:16-alpine
//	KORE_PG_E2E=1 go test -timeout 300s -v ./tests/e2e -run TestRealBackupRestore
package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// backupDB 是备份用例专用的库（与只读用例的 postgres 库隔离，互不影响）。
const backupDB = "kore_backup_e2e"

// TestRealBackupRestore 覆盖交付口径的每一项：
//  1. custom 归档备份 → 落盘文件存在且非空；
//  2. 破坏数据后恢复 → 数据回来（真 pg_restore）；
//  3. 恢复前自动快照 → 快照文件存在；
//  4. 备份列表 / 分块下载（base64 与磁盘内容一致）/ 删除；
//  5. sql + gzip 备份 → 恢复（真 psql 通道）；
//  6. 参数防线：恢复必须 confirm、路径不得越界、备份文件必须存在。
func TestRealBackupRestore(t *testing.T) {
	if os.Getenv("KORE_PG_E2E") != "1" {
		t.Skip("设置 KORE_PG_E2E=1 并启动 kore-pg-test 容器后运行")
	}
	container := strings.TrimSpace(os.Getenv("KORE_PG_DOCKER_CONTAINER"))
	if container == "" {
		container = e2eContainer
	}
	requireContainer(t, container)
	seedBackupDatabase(t, container)

	binary := buildPlugin(t)
	stateDir := t.TempDir()
	conn := startPlugin(t, binary, container, stateDir)
	defer func() { _ = conn.Close() }()

	// 连接配置指向容器的发布端口（口令只出现在配置契约里，不回显）。
	setParams := `{"values":{"host":"127.0.0.1","port":15432,"user":"postgres","password":"` + e2ePassword + `","databases_include_system":true}}`
	result, perr := call(t, conn, "db-postgres.config.set", setParams)
	mustOK(t, "config.set 指向容器", result, perr)

	exportDir := filepath.Join(stateDir, "exports")

	// ── 1. custom 归档备份 ──
	result, perr = call(t, conn, "db-postgres.backup", `{"database":"`+backupDB+`","format":"custom"}`)
	mustOK(t, "STEP1 db-postgres.backup（custom）", result, perr)
	var backup struct {
		FilePath    string `json:"file_path"`
		FileName    string `json:"file_name"`
		Format      string `json:"format"`
		Compression string `json:"compression"`
		SizeBytes   int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(result, &backup); err != nil {
		t.Fatalf("解析备份结果失败: %v", err)
	}
	if backup.Format != "custom" || !strings.HasSuffix(backup.FileName, ".dump") || backup.SizeBytes <= 0 {
		t.Fatalf("备份结果不符合预期: %+v", backup)
	}
	if !strings.HasPrefix(backup.FilePath, exportDir+string(filepath.Separator)) {
		t.Fatalf("备份文件未落在导出目录 %s: %s", exportDir, backup.FilePath)
	}
	if info, err := os.Stat(backup.FilePath); err != nil || info.Size() != backup.SizeBytes {
		t.Fatalf("磁盘上的备份文件与响应不一致: %v %+v", err, backup)
	}
	assertNoSecret(t, result, e2ePassword)

	// ── 2. 破坏数据 → 恢复 → 数据回来 ──
	dockerExec(t, container, "psql", "-U", "postgres", "-d", backupDB, "-c", "DROP TABLE kore_backup_probe")
	if got := probeRows(t, container, backupDB); got != "" {
		t.Fatalf("删表后仍能查到数据（前置破坏没生效）: %q", got)
	}
	restoreParams := `{"database":"` + backupDB + `","file_path":"` + backup.FilePath + `","confirm":true}`
	result, perr = call(t, conn, "db-postgres.restore", restoreParams)
	mustOK(t, "STEP2 db-postgres.restore（custom + 自动快照）", result, perr)
	var restored struct {
		Restored  bool   `json:"restored"`
		Database  string `json:"database"`
		Format    string `json:"format"`
		CreatedDB bool   `json:"created_database"`
		Snapshot  *struct {
			FilePath  string `json:"file_path"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(result, &restored); err != nil {
		t.Fatalf("解析恢复结果失败: %v", err)
	}
	if !restored.Restored || restored.Format != "custom" || restored.CreatedDB {
		t.Fatalf("恢复结果不符合预期: %+v", restored)
	}
	if restored.Snapshot == nil || !strings.Contains(restored.Snapshot.FilePath, "-prerestore-") {
		t.Fatalf("缺省应生成恢复前快照: %+v", restored.Snapshot)
	}
	if info, err := os.Stat(restored.Snapshot.FilePath); err != nil || info.Size() != restored.Snapshot.SizeBytes {
		t.Fatalf("快照文件与响应不一致: %v %+v", err, restored.Snapshot)
	}
	if rows := probeRows(t, container, backupDB); rows != "alpha,beta" {
		t.Fatalf("恢复后数据不对: %q，期望 alpha,beta", rows)
	}

	// ── 3. 列表：备份 + 快照两条，且都带格式/大小 ──
	result, perr = call(t, conn, "db-postgres.backupList", `{"database":"`+backupDB+`"}`)
	mustOK(t, "STEP3 db-postgres.backupList", result, perr)
	var listed struct {
		Dir     string `json:"dir"`
		Count   int    `json:"count"`
		Backups []struct {
			FilePath  string `json:"file_path"`
			FileName  string `json:"file_name"`
			SizeBytes int64  `json:"size_bytes"`
			Format    string `json:"format"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(result, &listed); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if listed.Count != 2 {
		t.Fatalf("列表应有 2 条（备份 + 快照），实际 %d: %s", listed.Count, result)
	}
	for _, file := range listed.Backups {
		if file.SizeBytes <= 0 || file.Format == "" {
			t.Fatalf("列表条目字段不全: %+v", file)
		}
	}

	// ── 4. 分块下载：base64 还原后与磁盘文件逐字节一致 ──
	onDisk, err := os.ReadFile(backup.FilePath)
	if err != nil {
		t.Fatalf("读取备份文件失败: %v", err)
	}
	var collected []byte
	offset := int64(0)
	for {
		params, _ := json.Marshal(map[string]any{
			"file_path": backup.FilePath, "offset": offset, "limit": 65536,
		})
		result, perr = call(t, conn, "db-postgres.backupDownload", string(params))
		if perr != nil {
			t.Fatalf("backupDownload 失败: %s: %s", perr.Code, perr.Message)
		}
		var chunk struct {
			NextOffset int64  `json:"next_offset"`
			EOF        bool   `json:"eof"`
			DataBase64 string `json:"data_base64"`
		}
		if err := json.Unmarshal(result, &chunk); err != nil {
			t.Fatalf("解析下载结果失败: %v", err)
		}
		raw, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
		if err != nil {
			t.Fatalf("base64 解码失败: %v", err)
		}
		collected = append(collected, raw...)
		offset = chunk.NextOffset
		if chunk.EOF {
			break
		}
	}
	if string(collected) != string(onDisk) {
		t.Fatalf("下载内容与磁盘文件不一致（下载 %d 字节 / 磁盘 %d 字节）", len(collected), len(onDisk))
	}
	t.Logf("STEP4 backupDownload 分块还原 %d 字节与磁盘一致", len(collected))

	// ── 5. sql + gzip 备份 → 恢复（真 psql 通道）──
	result, perr = call(t, conn, "db-postgres.backup", `{"database":"`+backupDB+`","format":"sql","compress":6}`)
	mustOK(t, "STEP5a db-postgres.backup（sql+gzip）", result, perr)
	var sqlBackup struct {
		FilePath  string `json:"file_path"`
		FileName  string `json:"file_name"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(result, &sqlBackup); err != nil {
		t.Fatalf("解析 sql 备份结果失败: %v", err)
	}
	if !strings.HasSuffix(sqlBackup.FileName, ".sql.gz") || sqlBackup.SizeBytes <= 0 {
		t.Fatalf("sql 备份文件名/大小不符合预期: %+v", sqlBackup)
	}
	dockerExec(t, container, "psql", "-U", "postgres", "-d", backupDB, "-c", "DROP TABLE kore_backup_probe")
	result, perr = call(t, conn, "db-postgres.restore",
		`{"database":"`+backupDB+`","file_path":"`+sqlBackup.FilePath+`","confirm":true,"snapshot":false}`)
	mustOK(t, "STEP5b db-postgres.restore（sql+gzip，无快照）", result, perr)
	if rows := probeRows(t, container, backupDB); rows != "alpha,beta" {
		t.Fatalf("sql 恢复后数据不对: %q", rows)
	}

	// ── 6. 参数防线（全部在触达数据库之前拒绝）──
	if _, perr := call(t, conn, "db-postgres.restore", `{"database":"`+backupDB+`","file_path":"`+backup.FilePath+`"}`); perr == nil || perr.Code != "E_INVALID" {
		t.Fatalf("缺 confirm 应 E_INVALID，实际 %+v", perr)
	}
	if _, perr := call(t, conn, "db-postgres.backup", `{"database":"`+backupDB+`","file_path":"../../etc/kore-escape.dump"}`); perr == nil || perr.Code != "E_INVALID" {
		t.Fatalf("路径穿越应 E_INVALID，实际 %+v", perr)
	}
	if _, perr := call(t, conn, "db-postgres.restore", `{"database":"`+backupDB+`","file_path":"`+filepath.Join(exportDir, "nope.dump")+`","confirm":true}`); perr == nil || perr.Code != "E_INVALID" {
		t.Fatalf("不存在的备份文件应 E_INVALID，实际 %+v", perr)
	}
	if _, perr := call(t, conn, "db-postgres.backupDelete", `{"file_path":"`+backup.FilePath+`"}`); perr == nil || perr.Code != "E_INVALID" {
		t.Fatalf("删除缺 confirm 应 E_INVALID，实际 %+v", perr)
	}

	// ── 7. 删除快照文件 ──
	result, perr = call(t, conn, "db-postgres.backupDelete",
		`{"file_path":"`+restored.Snapshot.FilePath+`","confirm":true}`)
	mustOK(t, "STEP7 db-postgres.backupDelete", result, perr)
	if _, err := os.Stat(restored.Snapshot.FilePath); !os.IsNotExist(err) {
		t.Fatalf("快照文件应已删除: %v", err)
	}
}

// seedBackupDatabase 造一个独立库 + 探测表，并用 INSERT 顺序固定期望值。
func seedBackupDatabase(t *testing.T, container string) {
	t.Helper()
	dockerExec(t, container, "psql", "-U", "postgres", "-d", "postgres", "-c",
		"DROP DATABASE IF EXISTS "+backupDB)
	dockerExec(t, container, "psql", "-U", "postgres", "-d", "postgres", "-c",
		"CREATE DATABASE "+backupDB)
	dockerExec(t, container, "psql", "-U", "postgres", "-d", backupDB, "-c",
		"CREATE TABLE kore_backup_probe (id int PRIMARY KEY, name text NOT NULL)")
	dockerExec(t, container, "psql", "-U", "postgres", "-d", backupDB, "-c",
		"INSERT INTO kore_backup_probe (id, name) VALUES (1, 'alpha'), (2, 'beta')")
}

// probeRows 用容器内 psql 独立读回数据（不走插件通道，避免「插件自证」）。
// 表不存在时返回空串——那就是恢复前的破坏状态。
func probeRows(t *testing.T, container, database string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "psql", "-U", "postgres",
		"-d", database, "-tAq", "-c", "SELECT name FROM kore_backup_probe ORDER BY id").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), ",")
}
