package pgop

import (
	"encoding/json"
	"strings"

	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// validName 判断名字是否为安全逻辑名：小写字母 / 数字 / - / _，长度 1..maxNameLen。
// 骨架口径下名字只做字符串校验，不解析为路径、不进入任何 shell / SQL。
func validName(name string) bool {
	if name == "" || len(name) > maxNameLen {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// validateScope 校验配置作用域：非空、无 NUL / 反斜杠、无 .. 段。
// 只做字符串校验，不访问文件系统、不连接数据库。
func validateScope(scope string) *pluginapi.Error {
	if scope == "" {
		return invalid("作用域不能为空")
	}
	if strings.ContainsRune(scope, 0) {
		return invalid("作用域含 NUL 字节")
	}
	if strings.Contains(scope, "\\") {
		return invalid("作用域含反斜杠")
	}
	for _, seg := range strings.Split(scope, "/") {
		if seg == ".." {
			return invalid("作用域含 .. 段，禁止穿越")
		}
	}
	return nil
}

// decodeParams 校验 params 是 JSON 对象并反序列化到目标形状。
func decodeParams(raw json.RawMessage, target any) *pluginapi.Error {
	if len(raw) == 0 || string(raw) == "null" {
		return invalid("缺少必填参数对象")
	}
	if raw[0] != '{' {
		return invalid("params 必须是 JSON 对象")
	}
	if err := json.Unmarshal(raw, target); err != nil {
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
