# 安装 — PostgreSQL 管理插件

## 前置条件

- kore 内核（要求 `requires_kore: >=0.1.0 <0.2.0`）。
- 本插件声明的能力由宿主按 `info.json` 授权；写操作需要 `system:admin`。
- 执行面依赖宿主机工具（详见[配置](Configuration)）：如 docker CLI、mysql/mysqldump、psql 等，缺失时插件返回 `E_UNSUPPORTED`，不静默降级。

## 获取与安装

1. 从 [Releases](https://github.com/kore-pl/kore-plugin-db-postgres/releases) 下载对应版本归档（如 `db-postgres-0.4.0.tar.gz` 与同名 `.sha256`），先核对 sha256。
2. 归档内首条目为 `info.json`，随后是 `bin/db-postgres` 与 `SHA256SUMS`；由 kore 安装器按 `info.json` 能力声明安装。
3. 安装器维护 `current` 稳定符号链接；升级后 wrapper 指向稳定路径，无需重写任务/部署记录。

## 从源码构建

```bash
make check                  # gofmt → go vet → go test -timeout 200s
make build                  # bin/db-postgres
make dist                   # dist/db-postgres-0.4.0.tar.gz + .sha256（位级可复现）
```

`make dist` 连跑两次的 sha256 必须一致（`--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner` + `gzip -9n`）。

## 验证安装

握手存活后调用只读方法确认（如 `kore.ping` / 各插件的 list 方法）；日志一律写 stderr，stdout 留给协议帧。
