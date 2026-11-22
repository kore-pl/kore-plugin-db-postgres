# Changelog

## 0.2.0 (2026-11-23)

- feat: 去掉骨架占位（`noopExecutor`）——`db-postgres.instances/databases/query/status` 四个方法真实连接 PostgreSQL：`instances` 逐实例 `SELECT version()` 返回 `{name,host,port,reachable,version,error}`；`databases` 用 `pg_database_size` 返回库名 + 真实字节数；`query` 执行只读 SQL（默认 100 行 / 上限 1000 行、单格 8 KiB、总行数据 8 MiB）；`status` 从 `pg_stat_database` / `pg_stat_activity` 取版本、连接数、库数、运行时长、缓存命中率
- feat: 新增配置契约 `db-postgres.config.get` / `db-postgres.config.set`（`<KORE_STATE_DIR>/config.json`，0600 原子写）——默认实例 `host/port/user/password/password_env/databases_include_system`，优先级 config.json > 环境变量 > 默认值，保存后同一进程下一次调用即生效
- feat: 默认实例密码可由界面填写并保存在本机 0600 配置文件；`config.get` 只回 `***`，`config.set` 收到 `***` 保留原值；`password_env`（默认 `KORE_PG_PASSWORD`）继续兼容且被直接密码覆盖；未配置密码不再报错（本机 trust/peer 是合法配置）
- feat: 执行通道两级回退——本机有 `psql` 就用 `psql` 子进程（`KORE_PG_BIN` 可指定绝对路径）；没有 `psql` 时自动发现运行中的 postgres 容器并用 `docker exec <容器> psql`（`KORE_PG_DOCKER_CONTAINER` 可固定容器、`KORE_PG_DOCKER_BIN` 可指定 docker 路径），两者都没有才返回 `E_UNSUPPORTED` + 可操作 hint
- feat: docker 回退自动做端口映射（宿主机发布端口 15432 → 容器内 127.0.0.1:5432），映射不上时给出「容器只监听 5432、请把端口改成发布端口」的明确 `E_INVALID`
- security: 密码不进 argv/ps——psql 走 `PGPASSWORD` 环境变量（最小子进程环境），docker 回退走 `sh -c 'read -r pw; export PGPASSWORD="$pw"'` 的 stdin 通道；响应与错误文本永不含口令，stderr 先脱敏再截断
- security: SQL 只读白名单（`SELECT`/`SHOW`/`EXPLAIN`/`WITH` 开头）+ 拒绝多语句（字面量/注释/美元引号之外出现分号即拒）+ `EXPLAIN` 内层只允许 `SELECT/WITH/VALUES/TABLE`（`EXPLAIN ANALYZE <写语句>` 会真的执行）+ 拒绝 `SELECT ... INTO`、加锁读 `FOR UPDATE/SHARE`、`pg_sleep`/`pg_read_file`/`lo_import`/`set_config`/`nextval` 等副作用函数、psql 反斜杠元命令与可执行注释
- security: 标识符白名单——库名 `^[a-zA-Z0-9_]{1,63}$`，实例名/主机/用户名各自正则且拒绝 `-` 开头（防 argv 选项注入）；多实例配置拒绝未知字段（避免 `password` 明文被静默忽略）
- refactor: 移除骨架期占位方法 `db-postgres.connections` 与 `KORE_PG_SCOPE`（无真实用途）；方法面收敛为 4 个业务方法 + 配置契约
- test: 单测覆盖 SQL 白名单（`DROP TABLE x`、`SELECT 1; DROP TABLE x`、`WITH ... DELETE`、`EXPLAIN ANALYZE DELETE` 全部被拒）、标识符正则、密码不进 argv（psql 与 docker exec 双通道）、NULL 与空串区分、行/单元格/总字节三重截断、配置优先级与掩码、非法值不落盘
- test: `tests/fault/` 四类故障（kill/timeout/disk/cgroup）覆盖全部 4 个方法；`tests/e2e/` 真实 postgres:16-alpine 端到端（配置契约 → 探测 → 库列表 → 只读查询 → 容器外造表读取 → 写操作拒绝 → 1000 行/8 KiB 上限 → status 指标 → 密码不外泄）
- docs: README 补方法表、配置契约、执行通道与安全边界、真实 E2E 命令与已知边界

## 0.1.1 (2026-11-23)

- refactor: 方法名统一为 db-postgres.<action>，对齐协议域校验
- build: 位级可复现打包（stage chmod + tar --mode 归一、size_bytes 收敛），二进制改嵌 manifest.json，消除同版本 sha 漂移

## 0.1.0 (2026-11-22)

- feat: 新建 db-postgres 插件骨架，复用 kore.plugin.v1 握手与会话循环
- feat: db-postgres.instances / db-postgres.connections 方法级参数校验与结构化结果，逻辑名白名单 + 作用域约束，不真连接数据库、不内置 psql
- refactor: 方法名按协议统一为 `<plugin-id>.<action>`（原 `pg.*` → `db-postgres.*`），移除裸动词与旧域前缀兼容
- feat: 基础级故障注入测试（kill -9 / 握手超时），直接拉起真实二进制验证故障隔离
- build: 确定性打包（--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner + gzip -9n），size_bytes 回填偏差 ≤10%
