# PostgreSQL 管理插件（kore-plugin-db-postgres）

## 简介

本插件真实连接 PostgreSQL 实例、列库（含大小）、执行只读 SQL、读状态指标与慢查询，并提供「界面可配、保存即生效」的连接配置契约；0.4.0 起补齐备份/恢复（pg_dump 自定义归档或纯文本、pg_restore / psql，恢复强制 confirm=true 且自动快照目标库）。不引入任何 PostgreSQL driver、不内置客户端二进制：执行面是宿主机 `psql` 子进程，本机没有 `psql` 时回退到 `docker exec <容器> psql`。

## 协议面

协议 `kore.plugin.v1`，能力 `system:admin`，与 `db-mysql` 互斥。方法：`config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`，写方法 `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword`，以及备份方法 `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`。方法名必须带 `db-postgres.` 域前缀，其他域返回 `E_NOTFOUND`。删除类方法须显式 `confirm=true`。

## 核心能力

配置优先级 `config.json` > 环境变量 > 代码默认值，口令只经 PGPASSWORD（psql 模式）或 stdin 首行（docker exec 模式）传递，不进 argv、ps 或日志。备份文件必须落在 `KORE_DB_EXPORT_DIR`（缺省 `<state>/exports`）内，经词法与符号链接二次归属校验；`backupDownload` 分块 base64 4 MiB 由前端拼 Blob 另存；备份列表按文件头跳过 mysqldump 产物。

## 安全边界

零 shell 注入面：pg_dump / pg_restore / psql 全部固定 argv，库名只作位置参数、表名只经 `--table=`；口令不进 argv / ps / 日志。恢复强制 `confirm=true`，恢复前自动快照目标库（可显式关闭），目标库不存在可勾选自动建库；纯文本恢复以 BEGIN/COMMIT 包裹保证原子性。备份路径词法 + 符号链接二次归属校验，失败不留半份文件。深度等级 `basic`：边界用例与故障注入延 v1.1。

## 构建与验证

日常验证用本地 `make check`（gofmt → go vet → go test -timeout 200s）；发布前 `make dist` 连跑两次核对 sha256 一致。类型化 nil 崩溃有专门回归闸门 `internal/pgop/typednil_test.go`；备份/恢复有契约与边界测试。版本 0.4.0；与 `db-mysql` 互斥，不要同时启用写能力。

## 相关链接

- [源码](https://github.com/kore-pl/kore-plugin-db-postgres)
- [kore 内核与协议](https://github.com/kore-pl/kore)
- [插件索引](https://github.com/kore-pl/kore-plugins)
