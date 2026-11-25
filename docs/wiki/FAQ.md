# 常见问题 — PostgreSQL 管理插件

## 为什么返回 `E_UNSUPPORTED`？

执行面依赖的宿主机工具缺失、daemon 不可达或平台不支持时会返回 `E_UNSUPPORTED`，这是诚实降级，不会伪造数据。请按[配置](Configuration)检查外部工具路径。

## 写操作为什么被拒绝？

写能力不是默认放开：插件须声明 `system:admin`，运行期再次校验；删除类方法还须显式 `confirm=true`。能力缺失返回 `E_UNAUTHORIZED`。

## 配置保存后需要重启插件吗？

不需要。统一配置契约保存后同进程下一次调用即生效（`restart_required=false`）；旧的多实例/环境变量路径继续兼容。

## 如何处理升级后的任务/记录指向旧版本？

安装器维护 `current` 稳定符号链接；提供 `post_upgrade` 后置动作的插件会在安装/升级后自动迁移存量记录到稳定路径，无需手工重写。

## 深度等级 `basic` / `production` 是什么意思？

`production` 表示核心路径与边界/故障覆盖达到生产判据；`basic` 表示核心路径可用、边界用例与故障注入延后（属定位，不是功能降级）。以仓库 `info.json` 为准。

## 去哪里报安全漏洞？

不要开公开 issue。按仓库 [SECURITY.md](https://github.com/kore-pl/kore-plugin-db-postgres/blob/main/SECURITY.md) 通过 GitHub Security Advisory 私密报告。
