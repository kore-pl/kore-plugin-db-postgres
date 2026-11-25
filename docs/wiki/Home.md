# PostgreSQL 管理插件（kore-plugin-db-postgres）

kore-plugin-db-postgres 是 kore 的 `db-postgres` 插件，独立仓库、独立版本线、独立 CI。当前版本 `0.4.0`，协议 `kore.plugin.v1`。

## 这是什么

本插件真实连接 PostgreSQL 实例、列库（含大小）、执行只读 SQL、读状态指标与慢查询，并提供「界面可配、保存即生效」的连接配置契约；0.4.0 起补齐备份/恢复（pg_dump 自定义归档或纯文本、pg_restore / psql，恢复强制 confirm=true 且自动快照目标库）。不引入任何 PostgreSQL driver、不内置客户端二进制：执行面是宿主机 `psql` 子进程，本机没有 `psql` 时回退到 `docker exec <容器> psql`。

## 快速入口

- [安装](Installation)：插件包获取与安装步骤
- [配置](Configuration)：配置项、能力声明与环境变量
- [使用方法](Usage)：协议方法与典型调用
- [常见问题](FAQ)：边界、权限与降级行为
- [变更记录](Changelog)：语义化版本历史

## 相关链接

- 源码：https://github.com/kore-pl/kore-plugin-db-postgres
- 内核与协议：https://github.com/kore-pl/kore
- 索引：https://github.com/kore-pl/kore-plugins
