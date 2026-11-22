package pgop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// callInstances 处理 db-postgres.instances：按可选 instance 过滤后对每个实例做连通性探测。
func callInstances(raw json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	var p instancesParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	selected := cfg.Instances
	if p.Instance != "" {
		instance, perr := findInstance(cfg, p.Instance)
		if perr != nil {
			return nil, perr
		}
		selected = []InstanceConfig{instance}
	}
	statuses, err := exec.Instances(selected)
	if err != nil {
		return nil, classify(err)
	}
	if statuses == nil {
		statuses = []InstanceStatus{}
	}
	return encode(InstancesResult{Instances: statuses, Count: len(statuses)})
}
