# Plugin de gestion PostgreSQL（kore-plugin-db-postgres）

## Aperçu

Ce plugin se connecte réellement aux instances PostgreSQL, liste les bases (avec tailles), exécute du SQL en lecture seule, lit les indicateurs d’état et les requêtes lentes, et propose un contrat de connexion configurable dans l’interface, effectif à l’enregistrement ; depuis 0.4.0, il ajoute la sauvegarde/restauration (pg_dump archive personnalisée ou SQL brut, pg_restore / psql ; la restauration exige confirm=true et snapshot automatique de la base cible). Il n’embarque aucun driver PostgreSQL ni binaire client : la couche d’exécution est un sous-processus `psql` de l’hôte, avec repli sur `docker exec <conteneur> psql` si `psql` est absent localement.

## Surface du protocole

Protocole `kore.plugin.v1`, capacité `system:admin`, mutuellement exclusif avec `db-mysql`. Méthodes : `config.get` / `config.set` / `instances` / `databases` / `query` / `status` / `slowlog`, méthodes d’écriture `createDatabase` / `dropDatabase` / `createUser` / `dropUser` / `grant` / `changePassword`, et méthodes de sauvegarde `backup` / `restore` / `backupList` / `backupDelete` / `backupDownload`. Les noms exigent le préfixe de domaine `db-postgres.` ; les autres domaines renvoient `E_NOTFOUND`. Les suppressions exigent `confirm=true` explicite.

## Fonctionnalités clés

La priorité de configuration est `config.json` > environnement > valeurs du code ; les mots de passe passent uniquement par PGPASSWORD (mode psql) ou la première ligne de stdin (mode docker exec), jamais dans argv, ps ni les journaux. Les fichiers de sauvegarde doivent être sous `KORE_DB_EXPORT_DIR` (par défaut `<state>/exports`), vérifiés lexicalement et contre les liens symboliques ; `backupDownload` transmet le base64 par blocs de 4 Mio pour assemblage côté frontal en Blob ; la liste des sauvegardes ignore les artefacts mysqldump par en-tête de fichier.

## Périmètre de sécurité

Aucune surface d’injection shell : pg_dump / pg_restore / psql utilisent un argv fixe, les noms de base uniquement comme argument positionnel et les noms de table uniquement via `--table=` ; les mots de passe n’entrent jamais dans argv / ps / journaux. La restauration exige `confirm=true`, snapshot automatiquement la base cible (désactivable explicitement) et peut la créer si elle manque ; les restaurations en texte brut sont encapsulées dans BEGIN/COMMIT pour l’atomicité. Les chemins de sauvegarde sont vérifiés lexicalement et contre les liens symboliques, sans fichier partiel en cas d’échec. Niveau de profondeur `basic` : cas limites et injection de fautes reportés à v1.1.

## Compilation et vérification

La vérification quotidienne utilise `make check` en local (gofmt → go vet → go test -timeout 200s) ; avant publication, `make dist` deux fois avec sha256 identique. Le crash par nil typé dispose d’un garde-fou de régression dédié `internal/pgop/typednil_test.go` ; la sauvegarde/restauration a des tests de contrat et de limites. Version 0.4.0 ; mutuellement exclusif avec `db-mysql` : ne pas activer l’écriture pour les deux en même temps.

## Liens connexes

- [源码](https://github.com/kore-pl/kore-plugin-db-postgres)
- [kore 内核与协议](https://github.com/kore-pl/kore)
- [插件索引](https://github.com/kore-pl/kore-plugins)
