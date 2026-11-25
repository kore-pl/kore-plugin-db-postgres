# PostgreSQL management plugin（kore-plugin-db-postgres）

## Overview

This plugin really connects to PostgreSQL instances, lists databases (with sizes), runs read-only SQL, reads status metrics and slow queries, and offers a UI-configurable connection contract that takes effect on save; since 0.4.0 it adds backup/restore (pg_dump custom archive or plain SQL, pg_restore / psql, restore requires confirm=true and auto-snapshots the target database). It brings no PostgreSQL driver and bundles no client binaries: the execution surface is a host `psql` subprocess, falling back to `docker exec <container> psql` when `psql` is absent locally.

## Protocol surface

Protocol `kore.plugin.v1`, capability `system:admin`, mutually exclusive with `db-mysql`. Methods: `config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`, write methods `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword`, and backup methods `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`. Method names require the `db-postgres.` domain prefix; other domains return `E_NOTFOUND`. Deletion methods require explicit `confirm=true`.

## Core capabilities

Config precedence is `config.json` > environment > code defaults; passwords travel only via PGPASSWORD (psql mode) or the first stdin line (docker exec mode), never argv, ps or logs. Backup files must live under `KORE_DB_EXPORT_DIR` (default `<state>/exports`), verified lexically and against symlinks; `backupDownload` streams base64 in 4 MiB chunks for the front end to assemble into a Blob; the backup list skips mysqldump artifacts by file header.

## Security boundary

Zero shell injection surface: pg_dump / pg_restore / psql all use fixed argv, database names only as positional arguments and table names only via `--table=`; passwords never enter argv / ps / logs. Restore requires `confirm=true`, auto-snapshots the target database beforehand (can be explicitly disabled) and can optionally create a missing target; plain-text restores are wrapped in BEGIN/COMMIT for atomicity. Backup paths undergo lexical plus symlink ownership checks and leave no partial files on failure. Depth level `basic`: boundary cases and fault injection are deferred to v1.1.

## Build and verification

Daily verification uses local `make check` (gofmt → go vet → go test -timeout 200s); before release run `make dist` twice and confirm identical sha256. The typed-nil crash has a dedicated regression gate `internal/pgop/typednil_test.go`; backup/restore has contract and boundary tests. Version 0.4.0; mutually exclusive with `db-mysql` — do not enable write capability for both at once.

## Related links

- [源码](https://github.com/kore-pl/kore-plugin-db-postgres)
- [kore 内核与协议](https://github.com/kore-pl/kore)
- [插件索引](https://github.com/kore-pl/kore-plugins)
