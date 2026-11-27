package pgop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// backendKind 是可用的执行通道。
type backendKind string

const (
	// kindPsql 直接用本机 psql 客户端（首选）。
	kindPsql backendKind = "psql"
	// kindDocker 用 `docker exec <容器> psql` 回退（本机没有 psql 时）。
	kindDocker backendKind = "docker"
)

// backend 是解析后的执行通道：psql 路径或 docker 容器。
type backend struct {
	kind      backendKind
	bin       string // psql 或 docker 可执行文件绝对路径
	container string // docker 模式下的容器名
}

// psqlTarget 是 psql 实际连接的目标（docker 模式下可能与配置不同）。
type psqlTarget struct {
	host     string
	port     int
	database string
}

// Client 通过本机 psql（或 docker exec psql）执行数据库操作（os/exec，不引入 driver）。
type Client struct {
	cfg Config

	mu      sync.Mutex
	backend *backend
}

// NewClient 用配置构造执行器。
func NewClient(cfg Config) *Client { return &Client{cfg: cfg} }

// 编译期断言：Client 实现 Executor。
var _ Executor = (*Client)(nil)

// Instances 并行为每个实例做连通性探测（SELECT version()），单实例失败只写入
// 该实例的 error 字段，不影响其它实例。
func (c *Client) Instances(instances []InstanceConfig) ([]InstanceStatus, error) {
	if len(instances) == 0 {
		return []InstanceStatus{}, nil
	}
	// 通道本身不可用时整体返回 E_UNSUPPORTED，避免每个实例重复同一句错误。
	if _, perr := c.resolved(); perr != nil {
		return nil, perr
	}
	statuses := make([]InstanceStatus, len(instances))
	// 固定 4 个 worker，避免实例多时 goroutine 线性膨胀（G-03 常驻资源预算）。
	workers := 4
	if len(instances) < workers {
		workers = len(instances)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				statuses[i] = c.probe(instances[i])
			}
		}()
	}
	for i := range instances {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return statuses, nil
}

// probe 做一次连通性探测并取回版本；错误文本不含密码。
func (c *Client) probe(instance InstanceConfig) InstanceStatus {
	status := InstanceStatus{Name: instance.Name, Host: instance.Host, Port: instance.Port}
	// 凭据来源只报「从哪来」，绝不回显口令本身（G-30）。
	_, status.CredentialSource = resolveCredential(instance)
	stdout, perr := c.run(instance, defaultDatabase, "SELECT version()", c.connectTimeout())
	if perr != nil {
		status.Error = perr.Message
		return status
	}
	columns, rows, _, err := parseCSV(stdout, 1, maxCellBytes)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	if len(columns) == 0 || len(rows) == 0 || len(rows[0]) == 0 {
		status.Error = "连通性探测返回空结果"
		return status
	}
	version := rows[0][0]
	if version == nullMarker || version == "" {
		status.Error = "连通性探测未返回版本"
		return status
	}
	status.Reachable = true
	status.Version = version
	return status
}

// Databases 列出库与大小；includeSystem 为 false 时排除模板库（template0/1）。
//
// 说明：`postgres` 是默认维护库，不算「系统模板库」——用户第一次打开库列表时它必须
// 可见，否则页面会像「一个库都没有」。真正的系统模板由 NOT datistemplate 排除。
func (c *Client) Databases(instance InstanceConfig, includeSystem bool) ([]Database, error) {
	query := "SELECT datname, pg_database_size(datname) FROM pg_database"
	if !includeSystem {
		query += " WHERE NOT datistemplate"
	}
	query += " ORDER BY datname"
	stdout, perr := c.run(instance, defaultDatabase, query, c.connectTimeout())
	if perr != nil {
		return nil, perr
	}
	columns, rows, _, err := parseCSV(stdout, maxDatabases, maxCellBytes)
	if err != nil {
		return nil, opErrorf(pluginapi.E_INTERNAL, "解析库列表失败: %v", err)
	}
	nameIdx, sizeIdx := columnIndex(columns, "datname"), columnIndex(columns, "pg_database_size")
	if nameIdx < 0 || sizeIdx < 0 {
		return nil, opErrorf(pluginapi.E_UPSTREAM, "库列表查询未返回 datname/pg_database_size 列: %v", columns)
	}
	databases := make([]Database, 0, len(rows))
	for _, row := range rows {
		database := Database{SizeBytes: -1}
		if nameIdx < len(row) {
			database.Name = row[nameIdx]
		}
		if sizeIdx < len(row) && row[sizeIdx] != nullMarker {
			if size, parseErr := strconv.ParseInt(strings.TrimSpace(row[sizeIdx]), 10, 64); parseErr == nil {
				database.SizeBytes = size
			}
		}
		if database.Name == "" || database.Name == nullMarker {
			continue
		}
		databases = append(databases, database)
	}
	return databases, nil
}

// statusQuery 一次取回 status 需要的全部指标（单语句，避免多次往返）。
const statusQuery = `SELECT
  version() AS version,
  (SELECT count(*) FROM pg_stat_activity) AS connections,
  (SELECT count(*) FROM pg_database WHERE NOT datistemplate) AS databases,
  (SELECT extract(epoch FROM now() - pg_postmaster_start_time()))::bigint AS uptime_s,
  (SELECT coalesce(sum(blks_hit), 0) FROM pg_stat_database) AS blocks_hit,
  (SELECT coalesce(sum(blks_read), 0) FROM pg_stat_database) AS blocks_read`

// Status 读取版本 / 连接数 / 库数 / 运行时长 / 缓存命中率。
func (c *Client) Status(instance InstanceConfig) (StatusResult, error) {
	stdout, perr := c.run(instance, defaultDatabase, statusQuery, c.connectTimeout())
	if perr != nil {
		return StatusResult{}, perr
	}
	columns, rows, _, err := parseCSV(stdout, 1, maxCellBytes)
	if err != nil {
		return StatusResult{}, opErrorf(pluginapi.E_INTERNAL, "解析 status 输出失败: %v", err)
	}
	if len(rows) == 0 {
		return StatusResult{}, opErrorf(pluginapi.E_UPSTREAM, "status 查询未返回数据行")
	}
	result := StatusResult{Instance: instance.Name}
	row := rows[0]
	result.Version = cellOf(columns, row, "version")
	result.Connections = int64Of(columns, row, "connections")
	result.Databases = int64Of(columns, row, "databases")
	result.UptimeS = int64Of(columns, row, "uptime_s")
	result.BlocksHit = int64Of(columns, row, "blocks_hit")
	result.BlocksRead = int64Of(columns, row, "blocks_read")
	if total := result.BlocksHit + result.BlocksRead; total > 0 {
		result.CacheHitRatio = round4(float64(result.BlocksHit) / float64(total))
	}
	if result.Version == "" || result.Version == nullMarker {
		return StatusResult{}, opErrorf(pluginapi.E_UPSTREAM, "status 查询未返回 version")
	}
	return result, nil
}

// Query 执行只读 SQL 并读取结果：最多 limit 行，单格 ≤8 KiB，总行数据 ≤8 MiB。
// database 为空时回落维护库（调用方已解析，这里再挡一次纵深校验）。
func (c *Client) Query(instance InstanceConfig, sql, database string, limit int) (QueryResult, error) {
	database, perrDB := resolveDatabase(database)
	if perrDB != nil {
		return QueryResult{}, perrDB
	}
	stdout, perr := c.run(instance, database, sql, c.queryTimeout())
	if perr != nil {
		return QueryResult{}, perr
	}
	columns, cells, truncated, err := parseCSV(stdout, limit, maxCellBytes)
	if err != nil {
		return QueryResult{}, opErrorf(pluginapi.E_INTERNAL, "解析查询结果失败: %v", err)
	}
	rows, budgetCut, err := buildRows(columns, cells, maxResultBytes)
	if err != nil {
		return QueryResult{}, opErrorf(pluginapi.E_INTERNAL, "编码查询结果失败: %v", err)
	}
	return QueryResult{
		Columns:   columns,
		Rows:      rows,
		RowCount:  len(rows),
		Truncated: truncated || budgetCut || len(rows) < len(cells),
	}, nil
}

// Exec 执行一条写语句（由 callExec 完成白名单与两级确认后才到这里）。
//
// 与 Query 的关键差异：SQL 走 runStdin 从 stdin 进 psql，不进 argv —— 写语句常带
// 字面量（甚至口令改密），进 argv 会暴露在本机 ps / docker 命令行里。
// psql 的 --csv / -P null / ON_ERROR_STOP 等参数与只读通道一致，RETURNING 的行
// 按 CSV 解析进 QueryResult，RowCount 为返回行数；无 RETURNING 时返回空数组。
func (c *Client) Exec(instance InstanceConfig, sql, database string, limit int) (QueryResult, error) {
	database, perrDB := resolveDatabase(database)
	if perrDB != nil {
		return QueryResult{}, perrDB
	}
	stdout, perr := c.runStdin(instance, database, sql, c.queryTimeout())
	if perr != nil {
		return QueryResult{}, perr
	}
	columns, cells, truncated, err := parseCSV(stdout, limit, maxCellBytes)
	if err != nil {
		return QueryResult{}, opErrorf(pluginapi.E_INTERNAL, "解析写入结果失败: %v", err)
	}
	rows, budgetCut, err := buildRows(columns, cells, maxResultBytes)
	if err != nil {
		return QueryResult{}, opErrorf(pluginapi.E_INTERNAL, "编码写入结果失败: %v", err)
	}
	return QueryResult{
		Columns:   columns,
		Rows:      rows,
		RowCount:  len(rows),
		Truncated: truncated || budgetCut || len(rows) < len(cells),
	}, nil
}

// connectTimeout 返回元数据方法的整体预算（缺省 5s）。
func (c *Client) connectTimeout() time.Duration {
	if c.cfg.ConnectTimeout > 0 {
		return c.cfg.ConnectTimeout
	}
	return defaultConnectTimeout
}

// queryTimeout 返回 db-postgres.query 的整体预算（缺省 10s）。
func (c *Client) queryTimeout() time.Duration {
	if c.cfg.QueryTimeout > 0 {
		return c.cfg.QueryTimeout
	}
	return defaultQueryTimeout
}

// run 执行一次 psql 调用并返回 stdout；错误映射为带协议码的 OpError。
// database 是 psql -d 的目标库（已过白名单），不拼进 SQL 字符串。
func (c *Client) run(instance InstanceConfig, database, sql string, timeout time.Duration) ([]byte, *OpError) {
	password := instancePassword(instance)
	for attempt := 0; ; attempt++ {
		backend, perr := c.resolved()
		if perr != nil {
			return nil, perr
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		stdout, stderr, err := c.exec(ctx, backend, instance, database, password, sql, timeout)
		ctxErr := ctx.Err()
		cancel()
		if err == nil {
			return stdout, nil
		}
		// context 到点被杀时 cmd.Run 只回 "signal: killed"，必须用 ctx.Err() 判定超时。
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, timeoutError(timeout)
		}
		// docker 容器可能被重建：缓存失效后重试一次，避免用户必须重启插件进程。
		if backend.kind == kindDocker && attempt == 0 && looksLikeMissingContainer(stderr) {
			c.invalidate()
			continue
		}
		return nil, c.execError(backend, err, stderr, password, timeout)
	}
}

// runStdin 用 stdin 执行一条 SQL（无 -c）：写操作（尤其带口令的 DDL）只能走这条通道，
// 因为 argv 会出现在本机 ps / docker 命令行里，口令一旦进 argv 就等于公开。
//
// psql 在既没有 -c 也没有 -f 时从 stdin 读命令；docker 模式下包装脚本先读走第一行
// （口令）再 exec psql，剩余 stdin 就是 SQL——两条通道都不把口令放进 argv。
func (c *Client) runStdin(instance InstanceConfig, database, sql string, timeout time.Duration) ([]byte, *OpError) {
	password := instancePassword(instance)
	backend, perr := c.resolved()
	if perr != nil {
		return nil, perr
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stdout, stderr, err := c.execStdin(ctx, backend, instance, database, password, sql, timeout)
	ctxErr := ctx.Err()
	cancel()
	if err == nil {
		return stdout, nil
	}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return nil, timeoutError(timeout)
	}
	return nil, c.execError(backend, err, stderr, password, timeout)
}

// execStdin 构造并执行「SQL 从 stdin 读」的子进程。
func (c *Client) execStdin(ctx context.Context, backend *backend, instance InstanceConfig, database, password, sql string, timeout time.Duration) ([]byte, string, error) {
	target, perr := c.targetFor(instance, backend, database)
	if perr != nil {
		return nil, "", perr
	}
	args := psqlStdinArgs(target, instance.User)
	var cmd *exec.Cmd
	if backend.kind == kindPsql {
		cmd = exec.CommandContext(ctx, backend.bin, args...)
		cmd.Env = psqlEnv(password, timeout)
		cmd.Stdin = strings.NewReader(sql)
	} else {
		// 第一行是口令（脚本 read 走），其后的内容是 SQL（psql 从剩余 stdin 读）。
		cmd = exec.CommandContext(ctx, backend.bin, dockerExecArgs(backend.container, args)...)
		cmd.Env = minimalEnv()
		cmd.Stdin = strings.NewReader(password + "\n" + sql)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

// targetFor 计算本次调用的 psql 目标地址（docker 模式按容器内地址映射）。
// database 是本次 SQL 的目标业务库；空串回落维护库，非空必须过 ValidateDatabaseName
// 白名单——它直接进 psql -d argv，绝不接受引号 / 分号 / 空白 / "-" 前缀。
func (c *Client) targetFor(instance InstanceConfig, backend *backend, database string) (psqlTarget, *OpError) {
	database = strings.TrimSpace(database)
	if database == "" {
		database = defaultDatabase
	}
	if err := ValidateDatabaseName(database); err != nil {
		return psqlTarget{}, opErrorf(pluginapi.E_INVALID, "database 参数非法：%v", err)
	}
	if backend.kind != kindDocker {
		return psqlTarget{host: instance.Host, port: instance.Port, database: database}, nil
	}
	return backend.dockerTarget(instance, database)
}

// exec 构造并执行子进程，返回 stdout 与 stderr 文本。
func (c *Client) exec(ctx context.Context, backend *backend, instance InstanceConfig, database, password, sql string, timeout time.Duration) ([]byte, string, error) {
	target, perr := c.targetFor(instance, backend, database)
	if perr != nil {
		return nil, "", perr
	}
	args := psqlArgs(target, instance.User, sql)
	var cmd *exec.Cmd
	if backend.kind == kindPsql {
		cmd = exec.CommandContext(ctx, backend.bin, args...)
		cmd.Env = psqlEnv(password, timeout)
	} else {
		// 密码经 stdin 喂给容器内的 sh，再 export 成 PGPASSWORD：
		// 既不进 argv（本机 ps 看不到），也不进 docker exec 的 -e 参数。
		cmd = exec.CommandContext(ctx, backend.bin, dockerExecArgs(backend.container, args)...)
		cmd.Env = minimalEnv()
		cmd.Stdin = strings.NewReader(password + "\n")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

// dockerPsqlScript 在容器内把 stdin 第一行读成密码并 exec psql。
// "$@" 承接 psql 参数，SQL 与目标地址都不拼进脚本字符串，无二次解析面。
const dockerPsqlScript = `read -r pw; if [ -n "$pw" ]; then export PGPASSWORD="$pw"; fi; exec psql "$@"`

// dockerExecArgs 构造 `docker exec` 参数：脚本只负责把 stdin 第一行变成 PGPASSWORD，
// psql 的参数整体放在 sh 的 "$@" 里，SQL / 地址 / 口令都不进脚本字符串。
// 密码不在返回值里——它只走 cmd.Stdin。
func dockerExecArgs(container string, psqlArgs []string) []string {
	args := []string{"exec", "-i", container, "sh", "-c", dockerPsqlScript, "sh"}
	return append(args, psqlArgs...)
}

// psqlArgs 构造 psql 参数。密码绝不进 argv：只通过 PGPASSWORD 环境变量（或容器内 stdin）传递。
//
// SQL 放进 argv 的 -c 是安全的（只读白名单已保证不含口令）；含口令的写语句走
// psqlStdinArgs + stdin，见 runStdin。
func psqlArgs(target psqlTarget, user, sql string) []string {
	return append(psqlBaseArgs(target, user), "-c", sql)
}

// psqlStdinArgs 构造「SQL 从 stdin 读」的参数：没有 -c，psql 会自动读 stdin。
func psqlStdinArgs(target psqlTarget, user string) []string {
	return psqlBaseArgs(target, user)
}

// psqlBaseArgs 是两种通道共用的连接参数（目标地址 + CSV 输出 + 遇错即停）。
func psqlBaseArgs(target psqlTarget, user string) []string {
	return []string{
		"--no-psqlrc",
		"--csv",
		"--quiet",
		"-v", "ON_ERROR_STOP=1",
		"-P", "null=" + nullMarker,
		"-h", target.host,
		"-p", strconv.Itoa(target.port),
		"-U", user,
		"-d", target.database,
	}
}

// psqlEnv 构造最小子进程环境：不透传插件进程里的其它变量，只提供 psql 必需项。
// PGCONNECT_TIMEOUT=3 与内核 5s 连接预算配套：先让客户端自己放弃，再由 context 兜底。
func psqlEnv(password string, timeout time.Duration) []string {
	connectSeconds := int(timeout.Seconds())
	if connectSeconds <= 0 || connectSeconds > 3 {
		connectSeconds = 3
	}
	env := []string{
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PGCONNECT_TIMEOUT=" + strconv.Itoa(connectSeconds),
		"PGAPPNAME=kore-db-postgres",
	}
	env = append(env, minimalEnv()...)
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	return env
}

// minimalEnv 返回子进程需要的白名单环境变量（PATH 让 psql 能找到自身依赖）。
func minimalEnv() []string {
	env := make([]string, 0, 4)
	for _, key := range []string{"PATH", "HOME", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// resolved 返回缓存的执行通道，未解析时做一次探测。
func (c *Client) resolved() (*backend, *OpError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backend != nil {
		return c.backend, nil
	}
	backend, perr := detectBackend(c.cfg)
	if perr != nil {
		return nil, perr
	}
	c.backend = backend
	return backend, nil
}

// invalidate 清空通道缓存（容器被重建后下一次调用重新探测）。
func (c *Client) invalidate() {
	c.mu.Lock()
	c.backend = nil
	c.mu.Unlock()
}

// detectBackend 决定执行通道：本机 psql → docker exec postgres 容器 → E_UNSUPPORTED。
func detectBackend(cfg Config) (*backend, *OpError) {
	psqlBin := strings.TrimSpace(cfg.PsqlBin)
	if psqlBin == "" {
		psqlBin = defaultPsqlBin
	}
	if path, err := exec.LookPath(psqlBin); err == nil {
		return &backend{kind: kindPsql, bin: path}, nil
	} else if strings.TrimSpace(cfg.PsqlBin) != "" {
		// 显式指定 KORE_PG_BIN 却不可执行：直接报错，不静默改用 docker。
		return nil, opErrorf(pluginapi.E_UNSUPPORTED,
			"KORE_PG_BIN=%q 不可执行：%v；请安装 postgresql-client 或修正该变量", cfg.PsqlBin, err)
	}

	dockerBin := strings.TrimSpace(cfg.DockerBin)
	if dockerBin == "" {
		dockerBin = defaultDockerBin
	}
	dockerPath, err := exec.LookPath(dockerBin)
	if err != nil {
		return nil, unsupportedHint("本机没有 psql 客户端，也没有 docker")
	}
	container := strings.TrimSpace(cfg.DockerContainer)
	if container == "" {
		container, err = discoverPostgresContainer(dockerPath)
		if err != nil {
			return nil, opErrorf(pluginapi.E_UPSTREAM, "枚举 docker 容器失败: %v", err)
		}
	}
	if container == "" {
		return nil, unsupportedHint("本机没有 psql 客户端，也没发现运行中的 postgres 容器")
	}
	return &backend{kind: kindDocker, bin: dockerPath, container: container}, nil
}

// unsupportedHint 给出可操作的两条路（装 psql 或起一个 postgres 容器）。
func unsupportedHint(prefix string) *OpError {
	return opErrorf(pluginapi.E_UNSUPPORTED,
		"%s：请安装 postgresql-client（提供 psql），或 `docker run -d --name kore-pg-test "+
			"-p 15432:5432 -e POSTGRES_PASSWORD=<口令> postgres:16-alpine` 后用 "+
			"KORE_PG_DOCKER_CONTAINER=kore-pg-test 指定容器", prefix)
}

// discoverPostgresContainer 在运行中的容器里找 postgres 服务端。
//
// 判定依据是镜像名（postgres / postgis / bitnami/postgresql 等）或容器名含 postgres；
// 同一台机器有多个候选时取第一个，可用 KORE_PG_DOCKER_CONTAINER 固定。
func discoverPostgresContainer(dockerBin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBin, "ps", "--format", "{{.Image}}\t{{.Names}}")
	cmd.Env = minimalEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 2 {
			continue
		}
		image, name := strings.ToLower(fields[0]), fields[1]
		imageBase := image
		if idx := strings.LastIndex(imageBase, "/"); idx >= 0 {
			imageBase = imageBase[idx+1:]
		}
		if idx := strings.Index(imageBase, ":"); idx >= 0 {
			imageBase = imageBase[:idx]
		}
		if strings.Contains(imageBase, "postgres") || strings.Contains(imageBase, "postgis") ||
			strings.Contains(strings.ToLower(name), "postgres") {
			return name, nil
		}
	}
	return "", nil
}

// dockerTarget 把「宿主机上的目标地址」映射成「容器内可达的地址」。
//
// 典型场景：postgres 跑在容器里、只发布到宿主机 15432（-p 15432:5432），而容器内部
// 监听 5432。此时在容器里必须连 127.0.0.1:5432，直连 15432 会 connection refused。
// 非本机目标（远程库）按原样透传。
func (b *backend) dockerTarget(instance InstanceConfig, database string) (psqlTarget, *OpError) {
	if database == "" {
		database = defaultDatabase
	}
	if !isLocalHost(instance.Host) {
		return psqlTarget{host: instance.Host, port: instance.Port, database: database}, nil
	}
	// 5432 是容器内 PostgreSQL 自己的端口：无需查询端口映射，直接回环访问。
	if instance.Port == 5432 {
		return psqlTarget{host: "127.0.0.1", port: 5432, database: database}, nil
	}
	published, perr := b.publishedPorts()
	if perr != nil {
		return psqlTarget{}, perr
	}
	if hostPort, ok := published[5432]; ok && hostPort == instance.Port {
		return psqlTarget{host: "127.0.0.1", port: 5432, database: database}, nil
	}
	return psqlTarget{}, opErrorf(pluginapi.E_INVALID,
		"容器 %s 内部的 PostgreSQL 只监听 5432（宿主机发布端口 %s）：请把实例端口改成该发布端口，"+
			"或用 KORE_PG_DOCKER_CONTAINER 指向正确的容器", b.container, formatPublished(published))
}

// publishedPorts 读取容器的端口映射（容器端口 → 宿主机端口）；只关心 5432。
func (b *backend) publishedPorts() (map[int]int, *OpError) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bin, "port", b.container)
	cmd.Env = minimalEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, opErrorf(pluginapi.E_UPSTREAM, "读取容器 %s 端口映射失败: %v", b.container, err)
	}
	ports := make(map[int]int, 2)
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), " -> ", 2)
		if len(parts) != 2 {
			continue
		}
		containerPort, convErr := strconv.Atoi(strings.TrimSuffix(parts[0], "/tcp"))
		if convErr != nil {
			continue
		}
		hostRaw := parts[1]
		if idx := strings.LastIndex(hostRaw, ":"); idx >= 0 {
			hostRaw = hostRaw[idx+1:]
		}
		hostPort, convErr := strconv.Atoi(hostRaw)
		if convErr != nil {
			continue
		}
		if _, seen := ports[containerPort]; !seen || strings.HasPrefix(parts[1], "0.0.0.0:") {
			ports[containerPort] = hostPort
		}
	}
	return ports, nil
}

// formatPublished 把端口映射压成可读摘要（用于错误提示）。
func formatPublished(ports map[int]int) string {
	if len(ports) == 0 {
		return "无"
	}
	keys := make([]int, 0, len(ports))
	for port := range ports {
		keys = append(keys, port)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, port := range keys {
		parts = append(parts, fmt.Sprintf("%d→%d", port, ports[port]))
	}
	return strings.Join(parts, ", ")
}

// isLocalHost 判断主机是否指向本机（本机目标才需要做容器内地址映射）。
func isLocalHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
		return true
	}
	return false
}

// execError 把子进程失败映射为协议错误；stderr 先脱敏再截断。
func (c *Client) execError(backend *backend, err error, stderr, password string, timeout time.Duration) *OpError {
	if errors.Is(err, context.DeadlineExceeded) {
		return timeoutError(timeout)
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && execErr != nil {
		return opErrorf(pluginapi.E_UNSUPPORTED,
			"无法执行 %s：%v；请安装 postgresql-client 或修正 KORE_PG_BIN", backend.describe(), execErr)
	}
	var opErr *OpError
	if errors.As(err, &opErr) && opErr != nil {
		return opErr
	}
	return opErrorf(pluginapi.E_UPSTREAM, "%s 失败: %s", backend.describe(), sanitizeStderr(stderr, password))
}

// describe 返回通道的可读名（psql / docker exec <容器>）。
func (b *backend) describe() string {
	if b.kind == kindPsql {
		return "psql"
	}
	return "docker exec " + b.container + " psql"
}

// looksLikeMissingContainer 判断 docker 是否报「容器不存在/没在运行」。
func looksLikeMissingContainer(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no such container") ||
		strings.Contains(lower, "is not running") ||
		strings.Contains(lower, "no such object")
}

// sanitizeStderr 过滤 NOTICE/WARNING 噪声、脱敏已知密码并截断，最多保留 500 字符。
func sanitizeStderr(stderr string, secrets ...string) string {
	lines := strings.Split(stderr, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "NOTICE:") || strings.HasPrefix(line, "WARNING:") ||
			strings.Contains(line, "psql: warning:") {
			continue
		}
		kept = append(kept, line)
	}
	message := strings.Join(kept, " | ")
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	if message == "" {
		message = "psql 无错误输出"
	}
	if runes := []rune(message); len(runes) > 500 {
		message = string(runes[:500]) + "…"
	}
	return message
}

// cellOf 按列名取单元格文本（找不到列时返回空串）。
func cellOf(columns, row []string, name string) string {
	idx := columnIndex(columns, name)
	if idx < 0 || idx >= len(row) {
		return ""
	}
	if row[idx] == nullMarker {
		return ""
	}
	return row[idx]
}

// int64Of 按列名取整数值；缺列或不可解析时返回 0（调用方已在 SQL 里 coalesce）。
func int64Of(columns, row []string, name string) int64 {
	value := strings.TrimSpace(cellOf(columns, row, name))
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// columnIndex 返回列名下标（找不到返回 -1）。
func columnIndex(columns []string, name string) int {
	for i, column := range columns {
		if column == name {
			return i
		}
	}
	return -1
}

// round4 保留 4 位小数，避免 float 噪声进响应。
func round4(value float64) float64 {
	return float64(int64(value*10000+0.5)) / 10000
}
