package pgop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 备份目录白名单：file_path 只允许落在 KORE_DB_EXPORT_DIR（默认 <state>/exports）内。
//
// 与 db-mysql 的 mysqlop/path.go 同源语义——两域共用同一个根目录与同一套判定，
// 用户从 MySQL 页切到 PostgreSQL 页时「导出到哪 / 从哪恢复」的行为完全一致：
//  1. 词法层先做（NUL / 反斜杠 / .. 段 / 绝对路径归属 / Clean+Rel 越界）；
//  2. 再跟随符号链接解析并与真实根目录做归属比对，挡住「父目录被换成软链」的逃逸。
//
// 与 file 插件一致：这里**接受绝对路径**，但必须位于备份根目录内——前端会把
// 后端返回的绝对路径回填进「恢复/下载」表单，拒绝绝对路径会逼用户手转一次。

// 备份文件扩展名白名单：只认这三种，列表/删除/下载都按它过滤，
// 避免把同一目录里 MySQL 的 .sql 或用户的其它文件当成本插件产物。
const (
	extDump    = ".dump"
	extBackup  = ".backup"
	extTar     = ".tar"
	extSQL     = ".sql"
	extSQLGzip = ".sql.gz"
)

// backupFormats 是后缀 → 备份格式的映射（custom = pg_dump 归档格式，sql = 纯文本）。
var backupFormats = map[string]string{
	extDump:    FormatCustom,
	extBackup:  FormatCustom,
	extTar:     FormatCustom,
	extSQL:     FormatSQL,
	extSQLGzip: FormatSQL,
}

// backupFormatOfName 从文件名后缀判断格式；第二个返回值表示是否 gzip 包裹（仅 .sql.gz）。
// 不在白名单内返回空格式，调用方据此拒绝。
func backupFormatOfName(name string) (string, bool) {
	lower := strings.ToLower(name)
	format, ok := backupFormats[filepath.Ext(lower)]
	if !ok {
		// .tar.gz / .dump.gz 等双后缀：先剥掉 .gz 再判一次。
		if strings.HasSuffix(lower, ".gz") {
			trimmed := strings.TrimSuffix(lower, ".gz")
			if format, ok = backupFormats[filepath.Ext(trimmed)]; ok {
				return format, true
			}
		}
		return "", false
	}
	return format, lower == extSQLGzip || strings.HasSuffix(lower, ".gz")
}

// isBackupFileName 判断文件名是否属于本插件产物（列表/删除/下载共用同一判据）。
func isBackupFileName(name string) bool {
	format, _ := backupFormatOfName(name)
	return format != ""
}

// validateBackupPath 做纯词法校验，返回归一化后的绝对路径（不访问文件系统）。
func validateBackupPath(root, raw string) (string, *pluginapi.Error) {
	if strings.TrimSpace(raw) == "" {
		return "", invalid("file_path 不得为空")
	}
	if raw != strings.TrimSpace(raw) {
		return "", invalid("file_path 不得含首尾空白")
	}
	if strings.ContainsRune(raw, 0) {
		return "", invalid("file_path 含 NUL 字节")
	}
	if strings.Contains(raw, "\\") {
		return "", invalid("file_path 含反斜杠")
	}
	rootClean := filepath.Clean(strings.TrimSpace(root))
	if rootClean == "" || !filepath.IsAbs(rootClean) {
		return "", internalErr("备份目录必须是绝对路径")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", invalid("file_path 含 .. 段，禁止穿越")
		}
	}
	var joined string
	if filepath.IsAbs(raw) {
		joined = filepath.Clean(raw)
	} else {
		joined = filepath.Join(rootClean, raw)
	}
	if !withinRoot(rootClean, joined) {
		return "", invalid("file_path 越出备份目录")
	}
	return joined, nil
}

// evalRealRoot 解析备份根目录的真实路径（跟随根自身的符号链接）。
func evalRealRoot(root string) (string, *pluginapi.Error) {
	clean := filepath.Clean(strings.TrimSpace(root))
	if clean == "" || !filepath.IsAbs(clean) {
		return "", internalErr("备份目录必须是绝对路径")
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", invalid("备份目录不存在或不可访问，请先创建")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", invalid("备份目录不可用")
	}
	return real, nil
}

// withinRoot 判定 p 是否等于 root 或位于 root 之下（基于 Rel，避免字符串前缀误判）。
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ResolveExistingBackup 校验并解析一个必须已存在的备份文件：跟随符号链接后二次校验归属。
func ResolveExistingBackup(root, raw string) (string, *pluginapi.Error) {
	joined, perr := validateBackupPath(root, raw)
	if perr != nil {
		return "", perr
	}
	realRoot, perr := evalRealRoot(root)
	if perr != nil {
		return "", perr
	}
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", invalid("备份文件不存在: " + raw + "（先「备份」生成文件，或刷新列表确认文件名）")
		}
		return "", internalErr("解析备份文件失败: " + err.Error())
	}
	if !withinRoot(realRoot, real) {
		return "", invalid("file_path 经符号链接解析后越出备份目录")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", internalErr("读取备份文件信息失败: " + err.Error())
	}
	if !info.Mode().IsRegular() {
		return "", invalid("file_path 必须是普通文件")
	}
	return real, nil
}

// ResolveBackupTarget 校验可能尚不存在的备份目标：解析最近的已存在祖先并校验归属，
// 再把尚未创建的尾段拼回去；祖先链上任何符号链接逃逸都会被拒绝。
func ResolveBackupTarget(root, raw string) (string, *pluginapi.Error) {
	joined, perr := validateBackupPath(root, raw)
	if perr != nil {
		return "", perr
	}
	realRoot, perr := evalRealRoot(root)
	if perr != nil {
		return "", perr
	}
	current, tail := joined, ""
	for {
		real, err := filepath.EvalSymlinks(current)
		if err == nil {
			if !withinRoot(realRoot, real) {
				return "", invalid("file_path 经符号链接解析后越出备份目录")
			}
			return filepath.Join(real, tail), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", internalErr(fmt.Sprintf("解析备份路径失败: %v", err))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", invalid("备份路径不存在")
		}
		tail = filepath.Join(filepath.Base(current), tail)
		current = parent
	}
}
