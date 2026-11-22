package pgop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callStatus 处理 db-postgres.status：版本 / 连接数 / 库数 / 运行时长 / 缓存命中率。
func callStatus(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p statusParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	instance, perr := findInstance(cfg, p.Instance)
	if perr != nil {
		return nil, perr
	}
	result, err := exec.Status(instance)
	if err != nil {
		return nil, classify(err)
	}
	return encode(result)
}
