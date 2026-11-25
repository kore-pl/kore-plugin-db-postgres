package pgop

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 备份 / 恢复方法面（db-postgres.backup / restore / backupList / backupDelete / backupDownload）。
//
// 与 db-mysql 的 export / import 口径对齐：同一份备份根目录（KORE_DB_EXPORT_DIR，
// 缺省 <state>/exports）、同样的路径白名单、同样的 0600 落盘与「失败不留半份文件」。
// 在此之上 PostgreSQL 侧多出：自定义归档格式（pg_dump -Fc，天然压缩）+ 纯 SQL 的 gzip、
// 备份列表 / 删除 / 分块下载、恢复前自动快照与显式 confirm 门。
//
// 防线顺序固定（任一缺失都不允许触达数据库或文件系统）：
//  1. 写方法 requireAdmin 纵深防御（内核已按 method_capabilities 授权过一次）；
//  2. 库名 / 表名过白名单，只作为固定 argv 的位置参数或 --table 值，绝不拼进 shell；
//  3. file_path 必须落在备份根目录内（backuppath.go），符号链接逃逸二次拦截；
//  4. 恢复类操作强制 confirm=true，并在恢复前对目标库做一次自动快照（可关）。

// 备份能力边界（与 mysql 插件的 256 MiB 相比放宽：PostgreSQL 库体量普遍更大）。
const (
	// FormatCustom 是 pg_dump 自定义归档格式（-Fc）：单文件、内置压缩、可用 pg_restore 选择性恢复。
	FormatCustom = "custom"
	// FormatSQL 是纯文本 SQL（-Fp）：可直接读、可用任意 psql 客户端恢复；可选 gzip。
	FormatSQL = "sql"

	// maxBackupBytes 是单个备份文件上限：1 GiB。导出超限会删除半成品文件。
	maxBackupBytes int64 = 1 << 30
	// defaultCompression 是未显式传 compress 时的压缩级别（1-9）。
	defaultCompression = 6
	// maxBackupTables 限制表级备份的表名个数，防止 argv 膨胀。
	maxBackupTables = 1024
	// maxBackupListLimit 是列表单次返回条数上限。
	maxBackupListLimit = 500
	// defaultBackupListLimit 是列表缺省条数。
	defaultBackupListLimit = 100
	// DefaultDownloadChunk 是分块下载缺省字节数：4 MiB（base64 后 ≈5.34 MiB，低于 8 MiB 帧上限）。
	DefaultDownloadChunk int64 = 4 << 20
	// MaxDownloadChunk 是单次分块上限。
	MaxDownloadChunk int64 = 4 << 20
	// backupStampLayout 是备份文件名的时间戳格式（与前端 stamp() 一致）。
	backupStampLayout = "20060102-150405"
)

// backupParams 是 db-postgres.backup 的入参形状。
//
// file_path 可省略：省略时按「<库名>-<时间戳>.<后缀>」生成；compress 用指针区分
// 「未传 = 默认 6」与「显式 0 = 不压缩」——两者语义不同，不能用一个零值混过去。
type backupParams struct {
	Instance   string   `json:"instance"`
	Database   string   `json:"database"`
	Tables     []string `json:"tables"`
	FilePath   string   `json:"file_path"`
	Format     string   `json:"format"`
	Compress   *int     `json:"compress"`
	SchemaOnly bool     `json:"schema_only"`
}

// restoreParams 是 db-postgres.restore 的入参形状。
//
// Confirm 必填 true（破坏性）；Snapshot 为 nil 表示缺省开启自动快照，
// 显式 false 才关闭（指针保证「没说」和「说不」可区分）。
type restoreParams struct {
	Instance       string `json:"instance"`
	Database       string `json:"database"`
	FilePath       string `json:"file_path"`
	Confirm        bool   `json:"confirm"`
	Snapshot       *bool  `json:"snapshot"`
	CreateDatabase bool   `json:"create_database"`
}

// backupListParams 是 db-postgres.backupList 的入参形状。
type backupListParams struct {
	Database string `json:"database"`
	Limit    int    `json:"limit"`
}

// backupDeleteParams 是 db-postgres.backupDelete 的入参形状。
type backupDeleteParams struct {
	FilePath string `json:"file_path"`
	Confirm  bool   `json:"confirm"`
}

// backupDownloadParams 是 db-postgres.backupDownload 的入参形状。
type backupDownloadParams struct {
	FilePath string `json:"file_path"`
	Offset   int64  `json:"offset"`
	Limit    int64  `json:"limit"`
}

// BackupSpec 是执行面收到的一次备份请求（库名 / 表名 / 路径均已校验）。
type BackupSpec struct {
	Database   string
	Tables     []string
	SchemaOnly bool
	Format     string
	Compress   int
	FilePath   string
}

// BackupData 是执行面返回的备份结果。
type BackupData struct {
	SizeBytes   int64
	Format      string
	Compression string
}

// RestoreSpec 是执行面收到的一次恢复请求（路径 / 目标库均已校验）。
type RestoreSpec struct {
	Database string
	FilePath string
	Format   string
	Gzip     bool
}

// BackupResult 是 db-postgres.backup 的结果。
type BackupResult struct {
	Database    string   `json:"database"`
	FilePath    string   `json:"file_path"`
	FileName    string   `json:"file_name"`
	Format      string   `json:"format"`
	Compression string   `json:"compression"`
	SizeBytes   int64    `json:"size_bytes"`
	Tables      []string `json:"tables,omitempty"`
	SchemaOnly  bool     `json:"schema_only"`
	CreatedAt   string   `json:"created_at"`
	Hint        string   `json:"hint,omitempty"`
}

// SnapshotInfo 是恢复前自动快照的摘要；为 nil 表示本次没有做快照。
type SnapshotInfo struct {
	FilePath  string `json:"file_path"`
	SizeBytes int64  `json:"size_bytes"`
}

// RestoreResult 是 db-postgres.restore 的结果。
type RestoreResult struct {
	Restored  bool          `json:"restored"`
	Database  string        `json:"database"`
	FilePath  string        `json:"file_path"`
	Format    string        `json:"format"`
	SizeBytes int64         `json:"size_bytes"`
	Snapshot  *SnapshotInfo `json:"snapshot,omitempty"`
	CreatedDB bool          `json:"created_database"`
	Hint      string        `json:"hint,omitempty"`
}

// BackupFile 是列表里的单个备份文件。
type BackupFile struct {
	FilePath   string `json:"file_path"`
	FileName   string `json:"file_name"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt string `json:"modified_at"`
	Format     string `json:"format"`
}

// BackupListResult 是 db-postgres.backupList 的结果。
type BackupListResult struct {
	Dir     string       `json:"dir"`
	Backups []BackupFile `json:"backups"`
	Count   int          `json:"count"`
	Hint    string       `json:"hint,omitempty"`
}

// BackupDeleteResult 是 db-postgres.backupDelete 的结果。
type BackupDeleteResult struct {
	Deleted   bool   `json:"deleted"`
	FilePath  string `json:"file_path"`
	SizeBytes int64  `json:"size_bytes"`
}

// BackupDownloadResult 是 db-postgres.backupDownload 的分块结果。
type BackupDownloadResult struct {
	FilePath      string `json:"file_path"`
	FileName      string `json:"file_name"`
	SizeBytes     int64  `json:"size_bytes"`
	Offset        int64  `json:"offset"`
	NextOffset    int64  `json:"next_offset"`
	EOF           bool   `json:"eof"`
	ChunkBytes    int    `json:"chunk_bytes"`
	MaxChunkBytes int64  `json:"max_chunk_bytes"`
	ContentType   string `json:"content_type"`
	DataBase64    string `json:"data_base64,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

// callBackup 处理 db-postgres.backup：校验库名/表名/格式 → 解析白名单路径 → pg_dump。
func callBackup(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p backupParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Database == "" {
		return nil, invalid("缺少必填参数 database（要备份的库名）")
	}
	if err := ValidateDatabaseName(p.Database); err != nil {
		return nil, invalid(err.Error())
	}
	tables, perr := validateBackupTables(p.Tables)
	if perr != nil {
		return nil, perr
	}
	format, perr := normalizeBackupFormat(p.Format)
	if perr != nil {
		return nil, perr
	}
	compress, perr := normalizeCompression(p.Compress, format)
	if perr != nil {
		return nil, perr
	}
	root, perr := backupRoot(cfg)
	if perr != nil {
		return nil, perr
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, internalErr("创建备份目录失败: " + err.Error())
	}
	relative := strings.TrimSpace(p.FilePath)
	if relative == "" {
		relative = defaultBackupName(p.Database, format, compress)
	}
	target, perr := ResolveBackupTarget(root, relative)
	if perr != nil {
		return nil, perr
	}
	if !isBackupFileName(target) {
		return nil, invalid("备份文件名必须以 .dump / .backup / .tar / .sql / .sql.gz 结尾，便于列表与恢复识别")
	}
	instance, perr := resolveInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	data, err := exec.Backup(instance, BackupSpec{
		Database: p.Database, Tables: tables, SchemaOnly: p.SchemaOnly,
		Format: format, Compress: compress, FilePath: target,
	})
	if !isNilError(err) {
		// 失败不留半份文件：pg_dump 可能已写出截断内容。
		_ = os.Remove(target)
		return nil, classify(err)
	}
	if data.SizeBytes > maxBackupBytes {
		_ = os.Remove(target)
		return nil, invalid("备份文件 " + strconv.FormatInt(data.SizeBytes, 10) + " 字节超过 1 GiB 上限，已删除；请改用表级备份或先清理历史数据")
	}
	if data.SizeBytes == 0 {
		_ = os.Remove(target)
		return nil, internalErr("pg_dump 未写出任何内容（0 字节），已删除空文件；请检查库名与账号权限")
	}
	return encode(BackupResult{
		Database: p.Database, FilePath: target, FileName: filepath.Base(target),
		Format: format, Compression: data.Compression, SizeBytes: data.SizeBytes,
		Tables: tables, SchemaOnly: p.SchemaOnly,
		CreatedAt: time.Now().Format(time.RFC3339),
		Hint:      "文件在备份目录内；「备份文件」里可下载或直接恢复。",
	})
}

// callRestore 处理 db-postgres.restore：强制 confirm → 目标库存在性 → 自动快照 → 恢复。
//
// 顺序不能换：先 confirm 再动数据库。目标库不存在时，create_database=true 先建库
// （此时没有可快照的对象，快照跳过并在 Hint 里说明）。
func callRestore(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p restoreParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Database == "" {
		return nil, invalid("缺少必填参数 database（恢复到哪个库）")
	}
	if err := ValidateDatabaseName(p.Database); err != nil {
		return nil, invalid(err.Error())
	}
	if !p.Confirm {
		return nil, invalid("恢复会覆盖目标库中的同名对象且不可撤销，请传 confirm=true 明确确认")
	}
	root, perr := backupRoot(cfg)
	if perr != nil {
		return nil, perr
	}
	source, perr := ResolveExistingBackup(root, p.FilePath)
	if perr != nil {
		return nil, perr
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, internalErr("读取备份文件信息失败: " + err.Error())
	}
	if info.Size() == 0 {
		return nil, invalid("备份文件为空: " + p.FilePath)
	}
	if info.Size() > maxBackupBytes {
		return nil, invalid("备份文件 " + strconv.FormatInt(info.Size(), 10) + " 字节超过 1 GiB 上限，拒绝恢复")
	}
	format, gzip, perr := sniffBackupFormat(source)
	if perr != nil {
		return nil, perr
	}
	instance, perr := resolveInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	exists, perr := databaseExists(exec, instance, p.Database)
	if perr != nil {
		return nil, perr
	}
	createdDB := false
	if !exists {
		if !p.CreateDatabase {
			return nil, invalid("目标库 " + p.Database + " 不存在：请先「新建数据库」，或在恢复表单里勾选「库不存在时自动创建」")
		}
		if _, err := exec.CreateDatabase(instance, p.Database, ""); !isNilError(err) {
			return nil, classify(err)
		}
		createdDB = true
	}
	var snapshot *SnapshotInfo
	wantSnapshot := p.Snapshot == nil || *p.Snapshot
	if wantSnapshot && !createdDB {
		snapshotPath := filepath.Join(root, p.Database+"-prerestore-"+time.Now().Format(backupStampLayout)+extDump)
		resolved, perr := ResolveBackupTarget(root, snapshotPath)
		if perr != nil {
			return nil, perr
		}
		data, err := exec.Backup(instance, BackupSpec{
			Database: p.Database, Format: FormatCustom, Compress: defaultCompression, FilePath: resolved,
		})
		if !isNilError(err) {
			_ = os.Remove(resolved)
			return nil, upstream("恢复前自动快照失败，已中止恢复（目标库未被改动）: " + errorText(err))
		}
		snapshot = &SnapshotInfo{FilePath: resolved, SizeBytes: data.SizeBytes}
	}
	if err := exec.Restore(instance, RestoreSpec{
		Database: p.Database, FilePath: source, Format: format, Gzip: gzip,
	}); !isNilError(err) {
		return nil, classify(err)
	}
	hint := "恢复完成。建议在 SQL 控制台抽查关键表行数确认数据完整。"
	if createdDB {
		hint = "目标库由本次恢复自动创建并完成恢复。建议在 SQL 控制台抽查关键表行数确认数据完整。"
	}
	return encode(RestoreResult{
		Restored: true, Database: p.Database, FilePath: source, Format: format,
		SizeBytes: info.Size(), Snapshot: snapshot, CreatedDB: createdDB, Hint: hint,
	})
}

// callBackupList 处理 db-postgres.backupList：只列备份目录顶层的备份文件。
func callBackupList(raw json.RawMessage, cfg Config) (json.RawMessage, *pluginapi.Error) {
	var p backupListParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	root, perr := backupRoot(cfg)
	if perr != nil {
		return nil, perr
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultBackupListLimit
	}
	if limit > maxBackupListLimit {
		return nil, invalid("limit 上限为 " + strconv.Itoa(maxBackupListLimit))
	}
	prefix := ""
	if p.Database != "" {
		if err := ValidateDatabaseName(p.Database); err != nil {
			return nil, invalid(err.Error())
		}
		prefix = p.Database + "-"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return encode(BackupListResult{Dir: root, Backups: []BackupFile{}, Count: 0,
				Hint: "备份目录还没有创建：先在库列表点「备份」生成第一份备份。"})
		}
		return nil, internalErr("读取备份目录失败: " + err.Error())
	}
	files := make([]BackupFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isBackupFileName(name) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) && !strings.HasPrefix(name, p.Database+".") {
			continue
		}
		// 同一备份目录与 db-mysql 共用：明显是 mysqldump 产物的 .sql 不列出来，
		// 否则用户在 PG 页会点到一份恢复必然失败的备份。
		if foreignDump(filepath.Join(root, name)) {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		format, _ := backupFormatOfName(name)
		files = append(files, BackupFile{
			FilePath: filepath.Join(root, name), FileName: name,
			SizeBytes: info.Size(), ModifiedAt: info.ModTime().Format(time.RFC3339), Format: format,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModifiedAt > files[j].ModifiedAt })
	total := len(files)
	if len(files) > limit {
		files = files[:limit]
	}
	result := BackupListResult{Dir: root, Backups: files, Count: total}
	if total == 0 {
		result.Hint = "备份目录里还没有备份文件：在库列表点「备份」生成第一份。"
	} else if total > len(files) {
		result.Hint = "共 " + strconv.Itoa(total) + " 份，仅列出最近 " + strconv.Itoa(len(files)) + " 份。"
	}
	return encode(result)
}

// callBackupDelete 处理 db-postgres.backupDelete：必须 confirm=true，只删备份目录内的备份文件。
func callBackupDelete(raw json.RawMessage, cfg Config) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p backupDeleteParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if !p.Confirm {
		return nil, invalid("删除备份文件不可恢复，请传 confirm=true")
	}
	root, perr := backupRoot(cfg)
	if perr != nil {
		return nil, perr
	}
	target, perr := ResolveExistingBackup(root, p.FilePath)
	if perr != nil {
		return nil, perr
	}
	if !isBackupFileName(target) {
		return nil, invalid("只能删除 .dump / .backup / .tar / .sql / .sql.gz 备份文件")
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, internalErr("读取备份文件信息失败: " + err.Error())
	}
	if err := os.Remove(target); err != nil {
		return nil, internalErr("删除备份文件失败: " + err.Error())
	}
	return encode(BackupDeleteResult{Deleted: true, FilePath: target, SizeBytes: info.Size()})
}

// callBackupDownload 处理 db-postgres.backupDownload：按 offset/limit 分块读 base64，
// 不把整份备份塞进单帧（4 MiB 块，base64 后 ≈5.34 MiB < 8 MiB 帧上限）。
func callBackupDownload(raw json.RawMessage, cfg Config) (json.RawMessage, *pluginapi.Error) {
	if perr := requireAdmin(cfg); perr != nil {
		return nil, perr
	}
	var p backupDownloadParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Offset < 0 {
		return nil, invalid("offset 不能为负数")
	}
	limit := p.Limit
	if limit == 0 {
		limit = DefaultDownloadChunk
	}
	if limit < 0 || limit > MaxDownloadChunk {
		return nil, invalid("limit 必须在 1-4194304 字节之间（不传按 4 MiB）")
	}
	root, perr := backupRoot(cfg)
	if perr != nil {
		return nil, perr
	}
	target, perr := ResolveExistingBackup(root, p.FilePath)
	if perr != nil {
		return nil, perr
	}
	if !isBackupFileName(target) {
		return nil, invalid("只能下载备份文件（.dump / .backup / .tar / .sql / .sql.gz）")
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, internalErr("打开备份文件失败: " + err.Error())
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, internalErr("读取备份文件信息失败: " + err.Error())
	}
	if p.Offset > info.Size() {
		return nil, invalid("offset 超过备份文件大小")
	}
	if _, err := file.Seek(p.Offset, io.SeekStart); err != nil {
		return nil, internalErr("定位备份文件偏移失败: " + err.Error())
	}
	chunk, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, internalErr("读取备份文件失败: " + err.Error())
	}
	next := p.Offset + int64(len(chunk))
	format, _ := backupFormatOfName(target)
	contentType := "text/plain; charset=utf-8"
	if format == FormatCustom {
		contentType = "application/octet-stream"
	}
	return encode(BackupDownloadResult{
		FilePath: target, FileName: filepath.Base(target), SizeBytes: info.Size(),
		Offset: p.Offset, NextOffset: next, EOF: next >= info.Size(),
		ChunkBytes: len(chunk), MaxChunkBytes: MaxDownloadChunk, ContentType: contentType,
		DataBase64: base64.StdEncoding.EncodeToString(chunk),
	})
}

// backupRoot 返回配置的备份目录；未配置时给出可操作的提示（与 mysql 同款文案口径）。
func backupRoot(cfg Config) (string, *pluginapi.Error) {
	root := strings.TrimSpace(cfg.ExportDir)
	if root == "" {
		return "", invalid("未配置备份目录：请设置 KORE_DB_EXPORT_DIR，或配置 KORE_STATE_DIR（默认 <state>/exports）")
	}
	return root, nil
}

// normalizeBackupFormat 归一化格式：缺省 custom（内置压缩），只接受 custom / sql。
func normalizeBackupFormat(raw string) (string, *pluginapi.Error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", FormatCustom:
		return FormatCustom, nil
	case FormatSQL:
		return FormatSQL, nil
	}
	return "", invalid("format 只支持 custom（默认，pg_dump 归档）或 sql（纯文本）")
}

// normalizeCompression 归一化压缩级别：nil 走默认 6，0 表示不压缩，1-9 显式级别。
func normalizeCompression(raw *int, format string) (int, *pluginapi.Error) {
	if raw == nil {
		return defaultCompression, nil
	}
	level := *raw
	if level < 0 || level > 9 {
		return 0, invalid("compress 必须在 0-9 之间（0 = 不压缩，1-9 为压缩级别）")
	}
	return level, nil
}

// defaultBackupName 生成缺省备份文件名：<库名>-<YYYYMMDD-HHMMSS>.<后缀>。
func defaultBackupName(database, format string, compress int) string {
	name := database + "-" + time.Now().Format(backupStampLayout)
	if format == FormatCustom {
		return name + extDump
	}
	if compress > 0 {
		return name + extSQLGzip
	}
	return name + extSQL
}

// validateBackupTables 校验表级备份的表名列表：白名单、去重、上限 1024。
func validateBackupTables(tables []string) ([]string, *pluginapi.Error) {
	if len(tables) > maxBackupTables {
		return nil, invalid("tables 数量超过上限 " + strconv.Itoa(maxBackupTables))
	}
	seen := make(map[string]struct{}, len(tables))
	out := make([]string, 0, len(tables))
	for _, table := range tables {
		trimmed := strings.TrimSpace(table)
		if trimmed == "" {
			continue
		}
		if err := validateTablePattern(trimmed); err != nil {
			return nil, invalid(err.Error())
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

// sniffBackupFormat 读文件头判定真实格式（不信任扩展名）：
//   - 前 5 字节 "PGDMP" → pg_dump 自定义归档；
//   - 头两字节 0x1f 0x8b → gzip 包裹的纯文本 SQL；
//   - 其余 → 纯文本 SQL。
func sniffBackupFormat(path string) (string, bool, *pluginapi.Error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, internalErr("打开备份文件失败: " + err.Error())
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 5)
	n, err := io.ReadFull(file, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", false, internalErr("读取备份文件头失败: " + err.Error())
	}
	header = header[:n]
	if strings.HasPrefix(string(header), "PGDMP") {
		return FormatCustom, false, nil
	}
	if len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		return FormatSQL, true, nil
	}
	return FormatSQL, false, nil
}

// databaseExists 通过只读库列表判断目标库是否存在（恢复前置检查）。
func databaseExists(exec Executor, instance InstanceConfig, name string) (bool, *pluginapi.Error) {
	databases, err := exec.Databases(instance, true)
	if !isNilError(err) {
		return false, classify(err)
	}
	for _, database := range databases {
		if database.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// foreignDump 判断文件是否明显来自别的数据库（当前只识别 mysqldump / MariaDB dump）。
//
// 判据是文件头 2 KiB：pg_dump 纯文本输出带「PostgreSQL database dump」，mysqldump 带
// 「MySQL dump」/「MariaDB dump」；gzip 包裹的 .sql.gz 先解压再判。识别不出的一律保留
// （用户自己的 PG SQL 不该被静默隐藏）。
func foreignDump(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	magic := make([]byte, 2)
	if n, _ := io.ReadFull(file, magic); n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return false
		}
		gz, gzErr := gzip.NewReader(file)
		if gzErr != nil {
			return false
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	} else if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	header := make([]byte, 2048)
	n, _ := io.ReadFull(reader, header)
	head := strings.ToLower(string(header[:n]))
	if strings.HasPrefix(strings.TrimSpace(head), strings.ToLower("PGDMP")) {
		return false
	}
	return strings.Contains(head, "mysql dump") || strings.Contains(head, "mariadb dump")
}
