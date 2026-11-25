# Plugin zur PostgreSQL-Verwaltung（kore-plugin-db-postgres）

## Überblick

Dieses Plugin verbindet sich tatsächlich mit PostgreSQL-Instanzen, listet Datenbanken (mit Größen), führt schreibgeschütztes SQL aus, liest Statusmetriken und langsame Abfragen und bietet einen UI-konfigurierbaren Verbindungsvertrag, der beim Speichern wirksam wird; seit 0.4.0 kommen Backup/Restore hinzu (pg_dump Custom-Archiv oder Klartext-SQL, pg_restore / psql; Restore erfordert confirm=true und automatischen Snapshot der Zieldatenbank). Es bringt keinen PostgreSQL-Treiber und keine Client-Binaries mit: die Ausführungsebene ist ein `psql`-Subprozess des Hosts, mit Rückfall auf `docker exec <Container> psql`, wenn `psql` lokal fehlt.

## Protokollfläche

Protokoll `kore.plugin.v1`, Fähigkeit `system:admin`, gegenseitig ausschließend mit `db-mysql`. Methoden: `config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`, Schreibmethoden `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword` sowie Backup-Methoden `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`. Methodennamen benötigen das Domain-Präfix `db-postgres.`; andere Domains liefern `E_NOTFOUND`. Löschen erfordert explizites `confirm=true`.

## Kernfunktionen

Die Konfigurationsreihenfolge ist `config.json` > Umgebung > Code-Defaults; Passwörter laufen nur über PGPASSWORD (psql-Modus) oder die erste stdin-Zeile (docker-exec-Modus), nie in argv, ps oder Logs. Backup-Dateien müssen unter `KORE_DB_EXPORT_DIR` liegen (Standard `<state>/exports`), lexikalisch und gegen Symlinks geprüft; `backupDownload` überträgt base64 in 4-MiB-Blöcken zum frontend-seitigen Blob-Zusammenbau; die Backup-Liste überspringt mysqldump-Artefakte anhand der Dateikopfzeile.

## Sicherheitsgrenze

Keine Shell-Injektionsfläche: pg_dump / pg_restore / psql verwenden festes argv, Datenbanknamen nur als Positionsargument und Tabellennamen nur über `--table=`; Passwörter gelangen nie in argv / ps / Logs. Restore erfordert `confirm=true`, erstellt vorher automatisch einen Snapshot der Zieldatenbank (explizit abschaltbar) und kann eine fehlende Zielbank optional anlegen; Klartext-Restores werden in BEGIN/COMMIT für Atomarität eingeschlossen. Backup-Pfade werden lexikalisch und gegen Symlinks geprüft und hinterlassen bei Fehlern keine Teildateien. Tiefe `basic`: Grenzfälle und Fehlerinjektion auf v1.1 verschoben.

## Build und Verifizierung

Die tägliche Prüfung nutzt lokales `make check` (gofmt → go vet → go test -timeout 200s); vor dem Release `make dist` zweimal mit identischem sha256. Der Typed-nil-Absturz hat ein eigenes Regressions-Gate `internal/pgop/typednil_test.go`; Backup/Restore verfügt über Vertrags- und Grenztests. Version 0.4.0; gegenseitig ausschließend mit `db-mysql` — nicht für beide gleichzeitig Schreibrechte aktivieren.

## Verwandte Links

- [源码](https://github.com/kore-pl/kore-plugin-db-postgres)
- [kore 内核与协议](https://github.com/kore-pl/kore)
- [插件索引](https://github.com/kore-pl/kore-plugins)
