package pgop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// Call 执行一个 db-postgres 方法：先校验方法名与参数，再调用执行面并结构化结果。
// exec 为 nil 时使用基于本机 psql（或 docker exec psql）的 Client。
func Call(method string, params json.RawMessage, cfg Config, exec Executor) (json.RawMessage, *pluginapi.Error) {
	verb, ok := businessVerb(method)
	if !ok {
		return nil, errNotFound(method)
	}
	if cfg.Err != nil {
		return nil, invalid("PostgreSQL 实例配置非法: " + cfg.Err.Error())
	}
	if exec == nil {
		exec = NewClient(cfg)
	}
	switch verb {
	case "instances":
		return callInstances(params, cfg, exec)
	case "databases":
		return callDatabases(params, cfg, exec)
	case "query":
		return callQuery(params, cfg, exec)
	case "status":
		return callStatus(params, cfg, exec)
	case "createDatabase":
		return callCreateDatabase(params, cfg, exec)
	case "dropDatabase":
		return callDropDatabase(params, cfg, exec)
	case "createUser":
		return callCreateUser(params, cfg, exec)
	case "dropUser":
		return callDropUser(params, cfg, exec)
	case "grant":
		return callGrant(params, cfg, exec)
	case "changePassword":
		return callChangePassword(params, cfg, exec)
	}
	return nil, errNotFound(method)
}

// methodPrefix 是方法名的插件域前缀：协议要求 `<plugin-id>.<action>`（PLUGIN-PROTOCOL §7.2）。
const methodPrefix = "db-postgres."

// businessVerb 把 db-postgres.<verb> 归一为动词名；域不是本插件 id 的一律拒绝。
func businessVerb(method string) (string, bool) {
	verb, ok := strings.CutPrefix(method, methodPrefix)
	if !ok {
		return "", false
	}
	switch verb {
	case "instances", "databases", "query", "status",
		"createDatabase", "dropDatabase", "createUser", "dropUser", "grant", "changePassword":
		return verb, true
	}
	return "", false
}

// findInstance 按名字取实例配置；名字非法返回 E_INVALID，未配置返回 E_NOTFOUND。
func findInstance(cfg Config, name string) (InstanceConfig, *pluginapi.Error) {
	if name == "" {
		return InstanceConfig{}, invalid("缺少必填参数 instance")
	}
	if err := validateInstanceName(name); err != nil {
		return InstanceConfig{}, invalid(err.Error())
	}
	for _, instance := range cfg.Instances {
		if instance.Name == name {
			return instance, nil
		}
	}
	return InstanceConfig{}, &pluginapi.Error{
		Code:    pluginapi.E_NOTFOUND,
		Message: fmt.Sprintf("未配置实例 %q（已配置: %s）", name, configuredNames(cfg)),
	}
}

// configuredNames 返回已配置实例名摘要，用于 E_NOTFOUND 提示（不含主机/密码）。
func configuredNames(cfg Config) string {
	if len(cfg.Instances) == 0 {
		return "无（请先在设置里填写数据库地址并按需保存）"
	}
	names := make([]string, 0, len(cfg.Instances))
	for i, instance := range cfg.Instances {
		if i == 10 {
			names = append(names, "…")
			break
		}
		names = append(names, instance.Name)
	}
	return strings.Join(names, ", ")
}

// classify 把执行面错误映射为协议错误码：四类故障各有对应，其余走 OpError / E_INTERNAL。
func classify(err error) *pluginapi.Error {
	var opErr *OpError
	if errors.As(err, &opErr) {
		return &pluginapi.Error{Code: opErr.Code, Message: opErr.Message}
	}
	var faultErr *FaultError
	if errors.As(err, &faultErr) {
		switch faultErr.Kind {
		case FaultKill:
			return upstream("进程被信号 9 终止: " + faultErr.Message)
		case FaultTimeout:
			return upstream("调用超时: " + faultErr.Message)
		case FaultDisk:
			return internalErr("磁盘满: " + faultErr.Message)
		case FaultCgroup:
			return unsupported("cgroup 限额触发: " + faultErr.Message)
		}
	}
	return internalErr("执行面失败: " + err.Error())
}

func invalid(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INVALID, Message: msg}
}

func errNotFound(method string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_NOTFOUND, Message: "未知方法: " + method}
}

func upstream(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_UPSTREAM, Message: msg}
}

func internalErr(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INTERNAL, Message: msg}
}

func unsupported(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_UNSUPPORTED, Message: msg}
}

// decodeParams 校验 params 是 JSON 对象并反序列化到目标形状。
func decodeParams(raw json.RawMessage, target any) *pluginapi.Error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return invalid("缺少必填参数对象")
	}
	if trimmed[0] != '{' {
		return invalid("params 必须是 JSON 对象")
	}
	if err := json.Unmarshal(trimmed, target); err != nil {
		return invalid("params 解析失败: " + err.Error())
	}
	return nil
}

// encode 把结果编码为 json.RawMessage；序列化失败归 E_INTERNAL。
func encode(v any) (json.RawMessage, *pluginapi.Error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, internalErr("序列化结果失败: " + err.Error())
	}
	return data, nil
}
