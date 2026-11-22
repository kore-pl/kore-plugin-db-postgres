# Changelog

## 0.1.1 (2026-11-23)

- refactor: 方法名统一为 db-postgres.<action>，对齐协议域校验
- build: 位级可复现打包（stage chmod + tar --mode 归一、size_bytes 收敛），二进制改嵌 manifest.json，消除同版本 sha 漂移

## 0.1.0 (2026-11-22)

- feat: 新建 db-postgres 插件骨架，复用 kore.plugin.v1 握手与会话循环
- feat: db-postgres.instances / db-postgres.connections 方法级参数校验与结构化结果，逻辑名白名单 + 作用域约束，不真连接数据库、不内置 psql
- refactor: 方法名按协议统一为 `<plugin-id>.<action>`（原 `pg.*` → `db-postgres.*`），移除裸动词与旧域前缀兼容
- feat: 基础级故障注入测试（kill -9 / 握手超时），直接拉起真实二进制验证故障隔离
- build: 确定性打包（--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner + gzip -9n），size_bytes 回填偏差 ≤10%
