# 使用方法 — PostgreSQL 管理插件

## 协议面

协议 `kore.plugin.v1`，能力 `system:admin`，与 `db-mysql` 互斥。方法：`config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`，写方法 `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword`，以及备份方法 `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`。方法名必须带 `db-postgres.` 域前缀，其他域返回 `E_NOTFOUND`。删除类方法须显式 `confirm=true`。

## 核心能力

配置优先级 `config.json` > 环境变量 > 代码默认值，口令只经 PGPASSWORD（psql 模式）或 stdin 首行（docker exec 模式）传递，不进 argv、ps 或日志。备份文件必须落在 `KORE_DB_EXPORT_DIR`（缺省 `<state>/exports`）内，经词法与符号链接二次归属校验；`backupDownload` 分块 base64 4 MiB 由前端拼 Blob 另存；备份列表按文件头跳过 mysqldump 产物。

## 典型流程

1. 握手：调用 `kore.ping` 确认插件存活（`kore.plugin.v1`）。
2. 读操作：调用本插件的只读方法（列表 / 详情 / 探测 / 查询），确认返回结构。
3. 写操作：确认 `info.json` 已声明对应能力，按方法要求提供显式确认参数（如 `confirm: true`）。
4. 失败处理：错误以结构化 `{"code","message"}` 返回；按 `code` 分流，不解析中文文案。

## 安全边界

零 shell 注入面：pg_dump / pg_restore / psql 全部固定 argv，库名只作位置参数、表名只经 `--table=`；口令不进 argv / ps / 日志。恢复强制 `confirm=true`，恢复前自动快照目标库（可显式关闭），目标库不存在可勾选自动建库；纯文本恢复以 BEGIN/COMMIT 包裹保证原子性。备份路径词法 + 符号链接二次归属校验，失败不留半份文件。深度等级 `basic`：边界用例与故障注入延 v1.1。
