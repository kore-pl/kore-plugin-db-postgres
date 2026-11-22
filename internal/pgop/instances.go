package pgop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// instancesParams 是 db-postgres.instances 的显式入参形状。
type instancesParams struct {
	Filter string `json:"filter"`
}

// InstancesResult 是 db-postgres.instances 的结构化结果。
type InstancesResult struct {
	Scope     string     `json:"scope"`
	Instances []Instance `json:"instances"`
	Count     int        `json:"count"`
	Applied   bool       `json:"applied"`
}

// callInstances 处理 db-postgres.instances：校验作用域与 filter 后返回实例列表占位。
func callInstances(raw json.RawMessage, scope string, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := validateScope(scope); perr != nil {
		return nil, perr
	}
	var p instancesParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if p.Filter != "" && !validName(p.Filter) {
		return nil, invalid("filter 不是合法逻辑名")
	}
	items, err := exec.Instances(scope, p.Filter)
	if err != nil {
		return nil, classify(err)
	}
	if items == nil {
		items = []Instance{}
	}
	return encode(InstancesResult{Scope: scope, Instances: items, Count: len(items), Applied: false})
}

// noopExecutor 是骨架默认执行面：不连接数据库，只返回空结果。
type noopExecutor struct{}

func (noopExecutor) Instances(scope, filter string) ([]Instance, error) { return nil, nil }
func (noopExecutor) Connections(scope, instance, database string) ([]Connection, error) {
	return nil, nil
}
