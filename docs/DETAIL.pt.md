# Plugin de gerenciamento PostgreSQL（kore-plugin-db-postgres）

## Visão geral

Este plugin conecta-se de verdade a instâncias PostgreSQL, lista bancos (com tamanhos), executa SQL somente leitura, lê métricas de estado e consultas lentas e oferece um contrato de conexão configurável na interface, com efeito ao salvar; desde 0.4.0 acrescenta backup/restauração (pg_dump em arquivo personalizado ou SQL simples, pg_restore / psql; a restauração exige confirm=true e snapshot automático do banco de destino). Não inclui driver PostgreSQL nem binários de cliente: a camada de execução é um subprocesso `psql` do host, com fallback para `docker exec <contêiner> psql` quando não há `psql` local.

## Superfície do protocolo

Protocolo `kore.plugin.v1`, capacidade `system:admin`, mutuamente exclusivo com `db-mysql`. Métodos: `config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`, métodos de escrita `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword` e métodos de backup `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`. Os nomes exigem o prefixo de domínio `db-postgres.`; outros domínios retornam `E_NOTFOUND`. Excluir exige `confirm=true` explícito.

## Funcionalidades principais

A precedência de configuração é `config.json` > ambiente > valores do código; senhas trafegam apenas por PGPASSWORD (modo psql) ou pela primeira linha do stdin (modo docker exec), nunca em argv, ps ou logs. Arquivos de backup devem ficar em `KORE_DB_EXPORT_DIR` (padrão `<state>/exports`), verificados lexicalmente e contra links simbólicos; `backupDownload` envia base64 em blocos de 4 MiB para o front-end montar um Blob; a lista de backups ignora artefatos mysqldump pelo cabeçalho do arquivo.

## Limite de segurança

Superfície zero de injeção de shell: pg_dump / pg_restore / psql usam argv fixo, nomes de banco apenas como argumento posicional e nomes de tabela apenas via `--table=`; senhas nunca entram em argv, ps ou logs. A restauração exige `confirm=true`, gera snapshot automático do banco de destino (desativável explicitamente) e pode criá-lo se ausente; restaurações em texto puro são envolvidas em BEGIN/COMMIT para atomicidade. Caminhos de backup passam por verificação lexical e de links simbólicos, sem deixar arquivos parciais em caso de falha. Nível de profundidade `basic`: casos limite e injeção de falhas ficam para v1.1.

## Build e verificação

A verificação diária usa `make check` local (gofmt → go vet → go test -timeout 200s); antes do lançamento, `make dist` duas vezes com sha256 idêntico. A falha por nil tipado tem uma barreira de regressão dedicada `internal/pgop/typednil_test.go`; backup/restauração tem testes de contrato e limite. Versão 0.4.0; mutuamente exclusivo com `db-mysql` — não ativar escrita para ambos ao mesmo tempo.

## Links relacionados

- [源码](https://github.com/kore-pl/kore-plugin-db-postgres)
- [kore 内核与协议](https://github.com/kore-pl/kore)
- [插件索引](https://github.com/kore-pl/kore-plugins)
