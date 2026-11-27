package pgop

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// 备份 / 恢复的执行面：pg_dump 写文件，psql 或 pg_restore 从文件恢复。
//
// 通道与只读查询完全一致（psql 优先、docker exec psql 回退）：
//   - psql 模式：直接 exec 本机 pg_dump / pg_restore / psql，口令只经 PGPASSWORD 环境变量；
//   - docker 模式：docker exec -i <容器> sh -c 'read pw; export PGPASSWORD; exec <tool> "$@"'，
//     口令走 stdin 第一行（不进 argv、不进 -e 参数、不进 docker 命令行），
//     备份归档与 SQL 正文作为 stdin 的后续字节流，不进容器文件系统。
//
// 两条通道都不经过 shell 字符串拼接：库名 / 表名只作为 argv 元素，工具名固定。

// transferTimeout 是备份/恢复的独立预算：dump 大库远超普通 DDL 的 15s，
// 但也不能无限挂住（与 db-mysql 的 10 分钟同口径）。
const transferTimeout = 10 * time.Minute

const (
	// defaultDumpBin 是 pg_dump 缺省值；与 psql 一样由宿主机提供（不内置二进制）。
	defaultDumpBin = "pg_dump"
	// defaultRestoreBin 是 pg_restore 缺省值（只用于 custom 归档格式）。
	defaultRestoreBin = "pg_restore"
)

// dumpBin 返回 pg_dump 路径（KORE_PG_DUMP_BIN 覆盖，缺省 PATH 中的 pg_dump）。
func (c *Client) dumpBin() string {
	if bin := strings.TrimSpace(c.cfg.DumpBin); bin != "" {
		return bin
	}
	return defaultDumpBin
}

// restoreBin 返回 pg_restore 路径（KORE_PG_RESTORE_BIN 覆盖，缺省 PATH 中的 pg_restore）。
func (c *Client) restoreBin() string {
	if bin := strings.TrimSpace(c.cfg.RestoreBin); bin != "" {
		return bin
	}
	return defaultRestoreBin
}

// Backup 用 pg_dump 生成备份文件：custom 归档直接落盘，plain SQL 在进程内 gzip 后落盘。
func (c *Client) Backup(instance InstanceConfig, spec BackupSpec) (BackupData, error) {
	if err := ValidateDatabaseName(spec.Database); err != nil {
		return BackupData{}, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	for _, table := range spec.Tables {
		if err := validateTablePattern(table); err != nil {
			return BackupData{}, opErrorf(pluginapi.E_INVALID, "%s", err.Error())
		}
	}
	format, perr := normalizeBackupFormat(spec.Format)
	if perr != nil {
		return BackupData{}, opErrorf(perr.Code, "%s", perr.Message)
	}
	level := spec.Compress
	if level < 0 || level > 9 {
		return BackupData{}, opErrorf(pluginapi.E_INVALID, "compress 必须在 0-9 之间")
	}
	password := instancePassword(instance)
	backend, oerr := c.resolved()
	if oerr != nil {
		return BackupData{}, oerr
	}
	target, oerr := c.targetFor(instance, backend, spec.Database)
	if oerr != nil {
		return BackupData{}, oerr
	}
	bin := c.dumpBin()
	if backend.kind == kindPsql {
		if _, err := exec.LookPath(bin); err != nil {
			return BackupData{}, opErrorf(pluginapi.E_UNSUPPORTED,
				"未找到 pg_dump %q：请安装 postgresql 客户端工具，或用 %s 指定绝对路径", bin, EnvDumpBin)
		}
	}
	if err := os.MkdirAll(filepath.Dir(spec.FilePath), 0o700); err != nil {
		return BackupData{}, opErrorf(pluginapi.E_INTERNAL, "创建备份目录失败: %v", err)
	}
	file, err := os.OpenFile(spec.FilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return BackupData{}, opErrorf(pluginapi.E_INTERNAL, "创建备份文件失败: %v", err)
	}
	var gz *gzip.Writer
	var out io.Writer = file
	if format == FormatSQL && level > 0 {
		gz = gzip.NewWriter(file)
		out = gz
	}
	ctx, cancel := context.WithTimeout(context.Background(), transferTimeout)
	defer cancel()
	args := dumpArgs(spec, target, instance.User)
	var cmd *exec.Cmd
	if backend.kind == kindPsql {
		cmd = exec.CommandContext(ctx, bin, args...)
		cmd.Env = psqlEnv(password, transferTimeout)
	} else {
		cmd = exec.CommandContext(ctx, backend.bin, dockerToolArgs(backend.container, "pg_dump", args)...)
		cmd.Env = minimalEnv()
		cmd.Stdin = strings.NewReader(password + newline)
	}
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var closeErr error
	if gz != nil {
		closeErr = gz.Close()
	}
	if err := file.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if runErr != nil {
		_ = os.Remove(spec.FilePath)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return BackupData{}, opErrorf(pluginapi.E_UPSTREAM, "备份超时（%s），已终止 pg_dump 并删除半成品", transferTimeout)
		}
		return BackupData{}, c.execError(backend, runErr, stderr.String(), password, transferTimeout)
	}
	info, err := os.Stat(spec.FilePath)
	if err != nil {
		return BackupData{}, opErrorf(pluginapi.E_INTERNAL, "读取备份文件信息失败: %v", err)
	}
	return BackupData{SizeBytes: info.Size(), Format: format, Compression: compressionLabel(format, level)}, nil
}

// Restore 从备份文件恢复：custom 归档走 pg_restore，纯文本（可 gzip）走 psql。
func (c *Client) Restore(instance InstanceConfig, spec RestoreSpec) error {
	if err := ValidateDatabaseName(spec.Database); err != nil {
		return opErrorf(pluginapi.E_INVALID, "%s", err.Error())
	}
	file, err := os.Open(spec.FilePath)
	if err != nil {
		return opErrorf(pluginapi.E_INVALID, "无法读取备份文件: %v", err)
	}
	defer func() { _ = file.Close() }()
	password := instancePassword(instance)
	backend, perr := c.resolved()
	if perr != nil {
		return perr
	}
	target, perr := c.targetFor(instance, backend, spec.Database)
	if perr != nil {
		return perr
	}
	ctx, cancel := context.WithTimeout(context.Background(), transferTimeout)
	defer cancel()
	if spec.Format == FormatCustom {
		return c.restoreCustom(ctx, backend, target, instance, spec.Database, password, file)
	}
	return c.restoreSQL(ctx, backend, target, instance, spec.Database, password, file, spec.Gzip)
}

// newline 是口令行结束符（docker 模式 stdin 第一行）。
const newline = "\n"

// restoreCustom 用 pg_restore 恢复 custom 归档。
//
// 参数口径（破坏性但有界）：
//   - --clean --if-exists：先删同名对象再重建（与 mysqldump 默认带 DROP TABLE 同语义）；
//   - --single-transaction：整次恢复一个事务，中途失败整体回滚，不留半恢复状态；
//   - --no-owner --no-privileges：不把归档里的属主/ACL 强加给目标实例（跨实例恢复更可能成功）。
func (c *Client) restoreCustom(ctx context.Context, backend *backend, target psqlTarget, instance InstanceConfig, database, password string, file *os.File) error {
	bin := c.restoreBin()
	if backend.kind == kindPsql {
		if _, err := exec.LookPath(bin); err != nil {
			return opErrorf(pluginapi.E_UNSUPPORTED,
				"未找到 pg_restore %q：请安装 postgresql 客户端工具，或用 %s 指定绝对路径；也可改用 sql 格式备份", bin, EnvRestoreBin)
		}
	}
	args := []string{
		"--no-password", "--clean", "--if-exists", "--no-owner", "--no-privileges",
		"--single-transaction",
		"-h", target.host, "-p", strconv.Itoa(target.port), "-U", instance.User,
		"-d", database,
	}
	return c.runTransfer(ctx, backend, bin, "pg_restore", args, file, password, nil)
}

// restoreSQL 用 psql 恢复纯文本 SQL（可 gzip）。
//
// 事务包裹：pg_dump -Fp 的输出本身不带事务，这里在 psql stdin 首尾补 BEGIN/COMMIT，
// 配合 ON_ERROR_STOP=1——任一句失败即停、连接关闭、整次恢复回滚，不留半应用状态。
func (c *Client) restoreSQL(ctx context.Context, backend *backend, target psqlTarget, instance InstanceConfig, database, password string, file *os.File, gzipped bool) error {
	var reader io.Reader = file
	if gzipped {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return opErrorf(pluginapi.E_INVALID, "备份文件不是合法 gzip: %v", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	args := []string{
		"--no-psqlrc", "--quiet", "--no-password",
		"-v", "ON_ERROR_STOP=1",
		"-h", target.host, "-p", strconv.Itoa(target.port), "-U", instance.User,
		"-d", database,
	}
	stream := io.MultiReader(strings.NewReader("BEGIN;"+newline), reader, strings.NewReader(newline+"COMMIT;"+newline))
	return c.runTransfer(ctx, backend, c.cfg.PsqlBin, "psql", args, nil, password, stream)
}

// runTransfer 执行一次恢复子进程：stdin 可以是文件（归档）或任意流（SQL 正文）。
//
// docker 模式下 stdin 的第一行是口令，脚本 read 掉后 exec 目标工具，
// 余下字节流原样交给 psql / pg_restore（两者都能从 stdin 读命令/归档）。
func (c *Client) runTransfer(ctx context.Context, backend *backend, bin, tool string, args []string, file *os.File, password string, stream io.Reader) error {
	var cmd *exec.Cmd
	switch backend.kind {
	case kindPsql:
		if strings.TrimSpace(bin) == "" {
			bin = tool
		}
		cmd = exec.CommandContext(ctx, bin, args...)
		cmd.Env = psqlEnv(password, transferTimeout)
		if stream != nil {
			cmd.Stdin = stream
		} else {
			cmd.Stdin = file
		}
	default:
		cmd = exec.CommandContext(ctx, backend.bin, dockerToolArgs(backend.container, tool, args)...)
		cmd.Env = minimalEnv()
		if stream != nil {
			cmd.Stdin = io.MultiReader(strings.NewReader(password+newline), stream)
		} else {
			cmd.Stdin = io.MultiReader(strings.NewReader(password+newline), file)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return opErrorf(pluginapi.E_UPSTREAM, "恢复超时（%s），已终止并回滚", transferTimeout)
		}
		return c.execError(backend, runErr, stderr.String(), password, transferTimeout)
	}
	return nil
}

// dumpArgs 构造 pg_dump 固定 argv。
//
// 库名固定放在最后的位置参数位（所有 PostgreSQL 版本都支持），表名只经 --table= 传入；
// 没有任何用户输入被拼进 shell 字符串（两条通道都不经过 shell）。
func dumpArgs(spec BackupSpec, target psqlTarget, user string) []string {
	formatFlag := "--format=custom"
	if spec.Format == FormatSQL {
		formatFlag = "--format=plain"
	}
	args := []string{"--no-password", formatFlag}
	if spec.Format == FormatCustom {
		args = append(args, "--compress="+strconv.Itoa(spec.Compress))
	} else {
		// 纯文本备份带 DROP ... IF EXISTS，恢复时即可覆盖同名对象（与 mysqldump 默认一致）。
		args = append(args, "--clean", "--if-exists")
	}
	if spec.SchemaOnly {
		args = append(args, "--schema-only")
	}
	args = append(args, "-h", target.host, "-p", strconv.Itoa(target.port), "-U", user)
	for _, table := range spec.Tables {
		args = append(args, "--table="+table)
	}
	return append(args, spec.Database)
}

// dockerToolScript 在容器内把 stdin 第一行读成密码并 exec 指定工具。
// "$@" 承接工具参数，库名/表名/路径都不拼进脚本字符串，无二次解析面。
func dockerToolScript(tool string) string {
	return `read -r pw; if [ -n "$pw" ]; then export PGPASSWORD="$pw"; fi; exec ` + tool + ` "$@"`
}

// dockerToolArgs 构造 docker exec 参数（脚本固定，工具名来自本文件常量）。
func dockerToolArgs(container, tool string, toolArgs []string) []string {
	return append([]string{"exec", "-i", container, "sh", "-c", dockerToolScript(tool), "sh"}, toolArgs...)
}

// compressionLabel 是给前端展示的压缩说明（不含任何口令）。
func compressionLabel(format string, level int) string {
	if level <= 0 {
		return "无压缩"
	}
	if format == FormatCustom {
		return "pg_dump -Z " + strconv.Itoa(level)
	}
	return "gzip -" + strconv.Itoa(level)
}
