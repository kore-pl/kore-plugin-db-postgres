package pgop

import (
	"encoding/json"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// connectionsParams 是 pg.connections 的显式入参形状。
type connectionsParams struct {
	Instance string `json:"instance"`
	Database string `json:"database"`
}

// ConnectionsResult 是 pg.connections 的结构化结果（不含任何口令字段）。
type ConnectionsResult struct {
	Scope       string       `json:"scope"`
	Instance    string       `json:"instance"`
	Database    string       `json:"database"`
	Connections []Connection `json:"connections"`
	Count       int          `json:"count"`
	Applied     bool         `json:"applied"`
}

// callConnections 处理 pg.connections：校验作用域与必填 instance 后返回连接列表占位。
func callConnections(raw json.RawMessage, scope string, exec Executor) (json.RawMessage, *pluginapi.Error) {
	if perr := validateScope(scope); perr != nil {
		return nil, perr
	}
	var p connectionsParams
	if perr := decodeParams(raw, &p); perr != nil {
		return nil, perr
	}
	if !validName(p.Instance) {
		return nil, invalid("instance 必填且须为合法逻辑名")
	}
	if p.Database != "" && !validName(p.Database) {
		return nil, invalid("database 不是合法逻辑名")
	}
	items, err := exec.Connections(scope, p.Instance, p.Database)
	if err != nil {
		return nil, classify(err)
	}
	if items == nil {
		items = []Connection{}
	}
	result := ConnectionsResult{
		Scope: scope, Instance: p.Instance, Database: p.Database,
		Connections: items, Count: len(items), Applied: false,
	}
	return encode(result)
}
