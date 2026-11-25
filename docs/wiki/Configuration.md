# 配置 — PostgreSQL 管理插件

## 能力声明（`info.json`）

- 协议：`kore.plugin.v1`；生命周期：`lazy`；深度等级见仓库 `info.json`。
- 能力：以 `info.json.capabilities` 与 `method_capabilities` 为准；读方法 `system:read`，写方法 `system:admin`。

## 配置优先级与存储

统一配置契约遵循 `kore/docs/PLUGIN-CONFIG.md`：`config.json` > 环境变量 > 代码默认值；配置文件位于 `<KORE_STATE_DIR>/config.json`，0600 原子写（临时文件 + fsync + rename）。保存后同一进程下一次调用立即生效（`restart_required=false`）；非法值返回 `E_INVALID` 且旧配置不变。

## 本插件可配项

配置优先级 `config.json` > 环境变量 > 代码默认值，口令只经 PGPASSWORD（psql 模式）或 stdin 首行（docker exec 模式）传递，不进 argv、ps 或日志。备份文件必须落在 `KORE_DB_EXPORT_DIR`（缺省 `<state>/exports`）内，经词法与符号链接二次归属校验；`backupDownload` 分块 base64 4 MiB 由前端拼 Blob 另存；备份列表按文件头跳过 mysqldump 产物。

## 状态目录

状态目录由环境变量指定（各插件不同，如 `KORE_APP_DEPLOY_STATE`、`KORE_STATE_DIR`），缺省一般位于 `<root>/state`。目录 0700、文件 0600；记录损坏或版本高于支持版本一律报错，不静默重置。
