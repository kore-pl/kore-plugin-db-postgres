# Complemento de gestión PostgreSQL（kore-plugin-db-postgres）

## Resumen

Este complemento se conecta realmente a instancias PostgreSQL, lista bases de datos (con tamaño), ejecuta SQL de solo lectura, lee métricas de estado y consultas lentas, y ofrece un contrato de conexión configurable en la interfaz que surte efecto al guardar; desde 0.4.0 añade copia de seguridad/restauración (pg_dump archivo personalizado o SQL plano, pg_restore / psql; restaurar exige confirm=true y snapshot automático de la base destino). No incluye ningún driver PostgreSQL ni binarios de cliente: la capa de ejecución es un subproceso `psql` del host, con respaldo a `docker exec <contenedor> psql` si no hay `psql` local.

## Superficie del protocolo

Protocolo `kore.plugin.v1`, capacidad `system:admin`, mutuamente excluyente con `db-mysql`. Métodos: `config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`, métodos de escritura `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword` y métodos de copia `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`. Los nombres requieren el prefijo de dominio `db-postgres.`; otros dominios devuelven `E_NOTFOUND`. Borrar exige `confirm=true` explícito.

## Funcionalidades principales

La prioridad de configuración es `config.json` > entorno > valores de código; las contraseñas solo viajan por PGPASSWORD (modo psql) o la primera línea de stdin (modo docker exec), nunca en argv, ps ni registros. Los archivos de copia deben estar bajo `KORE_DB_EXPORT_DIR` (por defecto `<state>/exports`), verificados léxicamente y contra enlaces simbólicos; `backupDownload` envía base64 en bloques de 4 MiB para que el frontend lo ensamble en un Blob; la lista de copias omite artefactos mysqldump por cabecera de archivo.

## Límite de seguridad

Superficie de inyección shell cero: pg_dump / pg_restore / psql usan argv fijo, los nombres de base solo como argumento posicional y los de tabla solo vía `--table=`; las contraseñas nunca entran en argv / ps / registros. Restaurar exige `confirm=true`, hace snapshot automático de la base destino (desactivable explícitamente) y puede crearla si falta; las restauraciones en texto plano se envuelven en BEGIN/COMMIT para garantizar atomicidad. Las rutas de copia se validan léxicamente y contra enlaces simbólicos, sin dejar archivos parciales si falla. Nivel de profundidad `basic`: casos límite e inyección de fallos se posponen a v1.1.

## Compilación y verificación

La verificación diaria usa `make check` local (gofmt → go vet → go test -timeout 200s); antes de publicar, `make dist` dos veces con sha256 idéntico. El bloqueo por nil tipado tiene una compuerta de regresión dedicada `internal/pgop/typednil_test.go`; copia/restauración tiene pruebas de contrato y límite. Versión 0.4.0; mutuamente excluyente con `db-mysql`: no habilitar escritura en ambos a la vez.

## Enlaces relacionados

- [源码](https://github.com/kore-pl/kore-plugin-db-postgres)
- [kore 内核与协议](https://github.com/kore-pl/kore)
- [插件索引](https://github.com/kore-pl/kore-plugins)
