package pgop

import (
	"encoding/json"
	"fmt"

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
	return encode(InstancesResult{Instances: statuses, Count: len(statuses), Hint: instancesHint(statuses)})
}

// instancesHint 生成空态引导：count=0 的根因是「没有任何实例配置」，而
// 「配了但都连不上」的下一步完全不同，前端需要按这两种情况给不同按钮与文案。
// 只要有一个实例可达就不给 hint——有内容可点，不需要引导。
func instancesHint(statuses []InstanceStatus) string {
	if len(statuses) == 0 {
		return "尚未配置 PostgreSQL 连接。请在插件设置里填写 host/port/user/password，或用 KORE_PG_HOST 等环境变量"
	}
	for _, status := range statuses {
		if status.Reachable {
			return ""
		}
	}
	return fmt.Sprintf("配置了 %d 个实例但均不可达，请检查 host/port 与网络", len(statuses))
}
