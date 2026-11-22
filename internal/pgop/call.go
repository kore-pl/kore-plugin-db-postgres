package pgop

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// Call 执行一个 pg 方法：先校验方法名与参数，再调用执行面并结构化结果。
// exec 为 nil 时使用 noopExecutor（骨架口径，不连接数据库）。
func Call(method string, params json.RawMessage, scope string, exec Executor) (json.RawMessage, *pluginapi.Error) {
	verb, ok := businessVerb(method)
	if !ok {
		return nil, errNotFound(method)
	}
	if exec == nil {
		exec = noopExecutor{}
	}
	switch verb {
	case "instances":
		return callInstances(params, scope, exec)
	case "connections":
		return callConnections(params, scope, exec)
	}
	return nil, errNotFound(method)
}

// businessVerb 把 pg.<verb> 或裸 <verb> 归一为动词名。
func businessVerb(method string) (string, bool) {
	verb := method
	if strings.HasPrefix(method, "pg.") {
		verb = strings.TrimPrefix(method, "pg.")
	}
	switch verb {
	case "instances", "connections":
		return verb, true
	}
	return "", false
}

// classify 把执行面错误映射为协议错误码；两类故障各有对应，其余归 E_INTERNAL。
func classify(err error) *pluginapi.Error {
	var fe *FaultError
	if errors.As(err, &fe) {
		switch fe.Kind {
		case FaultKill:
			return upstream("进程被信号 9 终止: " + fe.Message)
		case FaultTimeout:
			return upstream("调用超时: " + fe.Message)
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
