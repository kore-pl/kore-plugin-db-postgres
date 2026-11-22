package pgop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callDatabases 处理 db-postgres.databases：列出库 + 大小，默认过滤模板库。
func callDatabases(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p databasesParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	includeSystem := cfg.IncludeSystem
	if p.IncludeSystem != nil {
		includeSystem = *p.IncludeSystem
	}
	databases, err := exec.Databases(instance, includeSystem)
	if err != nil {
		return nil, classify(err)
	}
	if databases == nil {
		databases = []Database{}
	}
	return encode(DatabasesResult{Instance: instance.Name, Databases: databases, Count: len(databases)})
}
