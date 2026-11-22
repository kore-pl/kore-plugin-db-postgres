# kore-plugin-db-postgres

PostgreSQL 管理插件：**真实连接**实例、列库（含大小）、执行只读 SQL、读状态指标，并提供「界面可配、保存即生效」的连接配置契约。

- 深度等级 `basic`：核心路径（配置 / 探测 / 库列表 / 只读查询 / 状态）可用，写操作与备份恢复延后版本，**不得标"生产级 / 开箱可用"**。
- 不引入任何 PostgreSQL driver、不内置客户端二进制：执行面是宿主机 `psql` 子进程；本机没有 `psql` 时自动回退到 `docker exec <postgres 容器> psql`。

## 方法

方法名必须是 `<插件 id>.<action>`（PLUGIN-PROTOCOL §7.2），域不是 `db-postgres` 一律 `E_NOTFOUND`。

| 方法 | 入参 | 结果 |
| --- | --- | --- |
| `db-postgres.config.get` | `{}` | `{schema, values, source}`；`password` 只回 `***` |
| `db-postgres.config.set` | `{values:{...}}` | `{saved, applied, restart_required, warnings}` |
| `db-postgres.instances` | `{instance?}` | `{instances:[{name,host,port,reachable,version,error}], count}` |
| `db-postgres.databases` | `{instance, include_system?}` | `{instance, databases:[{name,size_bytes}], count}` |
| `db-postgres.query` | `{instance, sql, limit?}` | `{columns, rows, row_count, truncated}` |
| `db-postgres.status` | `{instance}` | `{instance, version, connections, databases, uptime_s, cache_hit_ratio, blocks_hit, blocks_read}` |

要点：

- `instances` 对每个实例执行 `SELECT version()`；单实例失败只写该实例的 `error`，不影响其它实例。
- `databases` 的 `size_bytes` 来自 `pg_database_size(datname)`；取不到时为 `-1`（不是 0）。
- `query` 默认返回 100 行、上限 1000 行；单格超 8 KiB 截断，行数据总量超 8 MiB 截断，都落 `truncated: true`。
- `status` 的 `cache_hit_ratio = blocks_hit / (blocks_hit + blocks_read)`，取自 `pg_stat_database`；`connections` 取自 `pg_stat_activity`。

## 配置契约

落盘 `<KORE_STATE_DIR>/config.json`（文件 `0600`、目录 `0700`、同目录临时文件 `fsync` 后 `rename` 的原子写）。优先级 **config.json > 环境变量 > 代码默认值**；保存成功后同一进程的下一次方法调用立即使用新地址（短连接，无需重启）。

| key | type | 默认 | 说明 |
| --- | --- | --- | --- |
| `host` | `string` | `127.0.0.1` | 数据库地址（环境变量 `KORE_PG_HOST`） |
| `port` | `int` | `5432` | 端口；容器发布端口（如 15432）也填这里（`KORE_PG_PORT`） |
| `user` | `string` | `postgres` | 连接账号（`KORE_PG_USER`） |
| `password` | `string`（`sensitive`） | `""` | 只存本机 0600 文件；`config.get` 只回 `***` |
| `password_env` | `string` | `KORE_PG_PASSWORD` | 密码环境变量名；`password` 优先（`KORE_PG_PASSWORD_ENV`） |
| `databases_include_system` | `bool` | `false` | 库列表是否包含 `template0`/`template1`（`KORE_PG_DATABASES_INCLUDE_SYSTEM`） |

- 配置文件只描述实例名 `default`。多实例走 `KORE_PG_INSTANCES`（JSON 数组）/ `KORE_PG_CONFIG`（文件路径）/ `<KORE_PG_ROOT>/instances.json`，条目形状 `{name,host,port,user,password_env}`，**拒绝未知字段与 `password` 明文**；与默认实例同名时由配置文件覆盖默认实例。
- `config.set` 提交 `"***"` 表示「不修改密码」；提交 `port: 0`、非整数端口、未知字段、非法主机/用户名都返回 `E_INVALID`，且不落盘半份文件。
- 没有密码不是错误：PostgreSQL 本机常见 `trust`/`peer` 认证，认证失败由服务端回 `E_UPSTREAM`，比"密码环境变量未设置"更准确。

## 执行通道（psql → docker exec 回退）

1. **本机 psql**（首选）：`psql --no-psqlrc --csv --quiet -v ON_ERROR_STOP=1 -P null=<SOH> -h <host> -p <port> -U <user> -d postgres -c <sql>`；`KORE_PG_BIN` 可指定绝对路径（指定了却不可执行会直接报 `E_UNSUPPORTED`，不静默换通道）。
2. **docker exec 回退**（本机没有 psql）：`docker ps` 里找镜像/容器名含 postgres 的运行中容器（`KORE_PG_DOCKER_CONTAINER` 可固定，`KORE_PG_DOCKER_BIN` 可指定 docker），执行 `docker exec -i <容器> sh -c '<读 stdin 当口令>; exec psql "$@"' sh <psql 参数…>`。
   - 本机目标自动做端口映射：配置 `127.0.0.1:15432`（容器发布端口）→ 容器内连 `127.0.0.1:5432`；映射不上时返回 `E_INVALID` 并列出实际映射。
3. 两者都没有 → `E_UNSUPPORTED` + 「装 postgresql-client 或起一个 postgres 容器并用 KORE_PG_DOCKER_CONTAINER 指定」的 hint。

超时：元数据方法（instances/databases/status）整体 5s、`query` 整体 10s（`KORE_PG_CONNECT_TIMEOUT` / `KORE_PG_TIMEOUT` 可覆盖），子进程带 `PGCONNECT_TIMEOUT=3`；到点杀进程并回 `E_UPSTREAM`。

## 安全边界

- **密码不进 argv/ps**：psql 走 `PGPASSWORD`（最小子进程环境，只导出 PATH/HOME/TMPDIR + LANG）；docker 回退把口令经 stdin 喂给容器内的 `sh` 再 export，`ps` 与 `docker exec` 参数里都看不到。响应、错误文本、stderr 一律先脱敏（把口令替换成 `***`）再截断。
- **只读白名单**：语句必须以 `SELECT` / `SHOW` / `EXPLAIN` / `WITH` 开头。
  - 拒绝多语句：去掉字符串字面量、引号标识符、`$tag$` 美元引号与注释后，分号只允许出现在末尾（`SELECT 1; DROP TABLE x` → `E_INVALID`）。
  - `EXPLAIN` 内层只允许 `SELECT/WITH/VALUES/TABLE`——`EXPLAIN ANALYZE DELETE …` 会**真的执行**删除。
  - `WITH` 子句里扫描数据修改关键字（`WITH x AS (DELETE … RETURNING *)` 是真实写路径）。
  - 拒绝 `SELECT … INTO`（建表）、`FOR UPDATE/SHARE`（加锁读）、`pg_sleep`/`pg_read_file`/`pg_ls_dir`/`lo_import`/`lo_export`/`dblink`/`set_config`/`nextval`/`pg_terminate_backend` 等副作用函数、psql 反斜杠元命令、可执行注释。
- **标识符白名单**：库名 `^[a-zA-Z0-9_]{1,63}$`；实例名 `^[a-z0-9][a-z0-9.-]{0,63}$`；主机/用户名各自正则且拒绝 `-` 开头（防被当成 psql 选项）。
- 写操作、连接配置之外的任何状态变更都不在 v0.2 范围。

### 已知边界（v1.1 处理）

- `query` 的值从 psql 文本推断：合法 JSON 数字 → 数字，其余 → 字符串。因此**布尔显示为 `t`/`f`**、`numeric` 会变成数字、看起来像数字的文本（如 `'001'`）仍是字符串（`001` 不是合法 JSON 数字）。
- 只读白名单拦的是语句骨架与已知危险函数；`SELECT` 里调用自定义函数无法穷尽枚举——请用只读账号连接生产库。
- 所有查询都连 `postgres` 维护库；跨库查询需 `dblink`/`postgres_fdw`，不在 v1 范围。

## 故障注入（四类）

`tests/fault/` 覆盖 kill / timeout / disk / cgroup 四类：

| 故障 | 用例 | 期望 |
| --- | --- | --- |
| kill -9 | `TestKill9` | 进程被信号终止，UDS 连接随后不可读 |
| 握手超时 | `TestTimeout` | 2s 内超时，以退出码 1 退出，不永久挂起 |
| 客户端被杀 | `TestKillFault` | 四个方法均 `E_UPSTREAM`（"信号 9"） |
| 调用超时 | `TestTimeoutFault` | 四个方法均 `E_UPSTREAM`（"超时"） |
| 磁盘满 | `TestDiskFullFault` | 四个方法均 `E_INTERNAL`（"磁盘满"） |
| cgroup 超限 | `TestCgroupLimitFault` | 四个方法均 `E_UNSUPPORTED`（"cgroup"） |

kill / 握手超时是**黑盒**用例（直接拉起真实 `bin/db-postgres`）；disk / cgroup 是**注入级**用例（执行面注入故障，验证错误映射链路），因为插件没有开关去真的写满磁盘或压爆 cgroup。

## 真实端到端验证

默认跳过；本机没有 `psql` 时插件会自动用 `docker exec`：

```bash
docker run -d --name kore-pg-test -p 15432:5432 -e POSTGRES_PASSWORD=<口令> postgres:16-alpine
KORE_PG_E2E=1 KORE_PG_DOCKER_CONTAINER=kore-pg-test \
  go test -timeout 300s -v ./tests/e2e
docker rm -f kore-pg-test
```

用例走完整链路并断言：默认配置 → `config.set` 指向 15432（`restart_required=false`）→ 配置文件 0600 → `config.get` 密码 `***` → `instances` 可达且版本 `PostgreSQL 16.x` → `databases` 含 `postgres` 且大小 >0、默认不含模板库 → `query "SELECT 1 AS n"` → 容器外造表后用 `query` 读到 → NULL 与空串可区分 → 写操作/多语句/`EXPLAIN ANALYZE DELETE` 被 `E_INVALID` 拒绝且表数据未变 → 1000 行与 8 KiB 上限 → `status` 真实连接数/库数/运行时长/命中率 → 响应中无机密。

## 构建与验收

```bash
gofmt -l . && GOWORK=off go vet ./... && GOWORK=off go test -timeout 200s ./...
make build && make dist
```

`dist` 用确定性参数归档（`--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner` + `gzip -9n`，显式列条目、无 `./` 根条目），重复构建 sha256 不变；`size_bytes` 由打包脚本回填并收敛到 ±2 字节。
