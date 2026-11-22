# kore-plugin-db-postgres

PostgreSQL 管理插件（**骨架**）：覆盖 PostgreSQL 实例 / 连接管理的方法面。本仓只交付协议骨架与参数校验，**不真连接数据库、不内置 psql / pg_dump 等任何工具二进制**（裁决 37 / 提案 §2.1 批 2）。

深度等级为 `basic`：核心路径可用，边界用例延 v1.1，不得标"生产级 / 开箱可用"。

## 协议面

- `kore.plugin.v1`，`lifecycle=lazy`，`depth_level=basic`
- 能力声明：`system:admin`
- 方法：`db-postgres.instances` / `db-postgres.connections`（域必须等于插件 id `db-postgres`，PLUGIN-PROTOCOL §7.2；不接受裸动词或旧 `pg.` 前缀，v0.1.0 无存量用户不做兼容层）
- 握手的 stdout 留给协议帧，日志一律写 stderr

## 方法参数（骨架口径）

- `db-postgres.instances`：`{"filter":"<逻辑名>"}` —— filter 可省略；非空时须为合法逻辑名
- `db-postgres.connections`：`{"instance":"<逻辑名>","database":"<逻辑名>"}` —— instance 必填，database 可选
- 逻辑名规则：`[a-z0-9_-]`，长度 1..128；拒绝空、大写、空格、路径段与注入字符
- 作用域由 `KORE_PG_SCOPE` 提供，缺省 `/var/lib/kore/db-postgres`；只做字符串校验，**不连接数据库、不读配置、不落盘**

结果均为结构化占位（实例 / 连接列表为空 + `applied=false`），不含任何口令字段。非法参数返回 `E_INVALID`，未知方法返回 `E_NOTFOUND`。

## 基础级故障注入

`tests/fault/` 注入 kill -9 与握手超时两类，直接拉起真实 `bin/db-postgres` 二进制（黑盒），不 mock、不 t.Skip：

| 故障 | 用例 | 期望 |
| --- | --- | --- |
| kill -9 | `TestKill9` | 进程被信号终止，UDS 连接随后不可读 |
| 握手超时 | `TestTimeout` | 2s 内超时，以退出码 1 退出，不永久挂起 |

## 构建与验收

```bash
gofmt -l . && go vet ./... && go test -timeout 200s ./...
make build && make dist
tar -tzf dist/db-postgres-0.1.0.tar.gz | head -1   # info.json
```

`dist` 使用确定性参数归档（`--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner` + `gzip -9n`，显式列条目、无 `./` 根条目），重复构建 sha256 不变；`size_bytes` 由打包脚本回填，与实际包体偏差 ≤10%。
