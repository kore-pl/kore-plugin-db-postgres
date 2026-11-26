package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"runtime"
	"time"

	"github.com/pengtang1024/kore-plugin-db-postgres/internal/pgop"
	"github.com/pengtang1024/kore-plugin-db-postgres/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// statusResult 是 kore.ping 的结果（PLUGIN-PROTOCOL §7.1）。
type statusResult struct {
	Status     string `json:"status"`
	UptimeS    int64  `json:"uptime_s"`
	Goroutines int    `json:"goroutines"`
	RSSKB      int64  `json:"rss_kb"`
}

// configValues 是 config.get 返回的 values 形状：password 始终存在，
// 有值时是占位符 "***"，无值时是空串（PLUGIN-CONFIG §6 原文口径）。
type configValues struct {
	Host                   string `json:"host"`
	Port                   int    `json:"port"`
	User                   string `json:"user"`
	Password               string `json:"password"`
	PasswordEnv            string `json:"password_env"`
	DatabasesIncludeSystem bool   `json:"databases_include_system"`
}

// dispatch 把请求分发为响应帧；第二个返回值表示收到 kore.shutdown。
func dispatch(request *pluginapi.Request, cfg *config, logger *slog.Logger) (*pluginapi.Response, bool) {
	response := &pluginapi.Response{Type: pluginapi.TypeResponse, ID: request.ID, ReqID: request.ReqID}
	switch request.Method {
	case "kore.shutdown":
		response.Result = json.RawMessage(`{"ok":true}`)
		logger.Info("收到 kore.shutdown，优雅退出")
		return response, true
	case "kore.ping":
		response.Result = pingResult(*cfg)
		return response, false
	case "kore.reload":
		response.Result = json.RawMessage(`{"ok":true,"config_digest":""}`)
		return response, false
	case cfg.id + ".config.get", "config.get":
		result, perr := handleConfigGet(cfg)
		if perr != nil {
			response.Error = perr
			return response, false
		}
		response.Result = result
		return response, false
	case cfg.id + ".config.set", "config.set":
		result, perr := handleConfigSet(request.Params, cfg)
		if perr != nil {
			response.Error = perr
			return response, false
		}
		response.Result = result
		return response, false
	}
	result, perr := pgop.Call(request.Method, request.Params, cfg.pg, cfg.client)
	if perr != nil {
		response.Error = perr
		response.Result = nil
		return response, false
	}
	response.Result = result
	return response, false
}

// handleConfigGet 返回表单 schema、当前值与来源；密码只回占位符，不回原值。
func handleConfigGet(cfg *config) (json.RawMessage, *pluginapi.Error) {
	masked := configValues{
		Host:                   cfg.values.Host,
		Port:                   cfg.values.Port,
		User:                   cfg.values.User,
		PasswordEnv:            cfg.values.PasswordEnv,
		DatabasesIncludeSystem: cfg.values.DatabasesIncludeSystem,
	}
	if cfg.values.Password != "" {
		masked.Password = plugincfg.SensitivePlaceholder
	}
	return encodeResult(plugincfg.GetResult{
		Schema: pgSchema(),
		Values: mustMarshal(masked),
		Source: cfg.source,
	}, "db-postgres.config.get")
}

// handleConfigSet 执行「校验 → 原子写 → 更新内存」：写盘成功后才替换运行期实例表，
// 下一次 db-postgres.* 调用立即使用新地址（无需重启，连接本身是每次调用建立的短连接）。
func handleConfigSet(params json.RawMessage, cfg *config) (json.RawMessage, *pluginapi.Error) {
	var p plugincfg.SetParams
	if perr := decodeParams(params, &p); perr != nil {
		return nil, perr
	}
	if len(p.Values) == 0 || string(p.Values) == "null" || p.Values[0] != '{' {
		return nil, invalid("values 需要是 JSON 对象，请传 {...} 格式")
	}
	// 显式传 port=0 属于越界（schema min=1）；只有文件里缺少该字段才按默认 5432 处理。
	var probe struct {
		Port *int `json:"port"`
	}
	if err := json.Unmarshal(p.Values, &probe); err == nil && probe.Port != nil && *probe.Port < 1 {
		return nil, invalid("端口请填 1 到 65535 之间的数字，不能填 0")
	}
	merged := cfg.values
	if err := strictDecode(p.Values, &merged); err != nil {
		return nil, invalid("配置值无效，请检查后重试: " + err.Error())
	}
	if merged.Password == plugincfg.SensitivePlaceholder {
		merged.Password = cfg.values.Password
	}
	normalized, err := validateValues(merged)
	if err != nil {
		return nil, invalid("PostgreSQL 设置无效，请按提示修改: " + err.Error())
	}
	if cfg.store == nil || cfg.store.Path() == "" {
		return nil, internalErr("KORE_STATE_DIR 未设置，无法保存插件配置")
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, internalErr("序列化配置值失败: " + err.Error())
	}
	if err := cfg.store.Write(raw); err != nil {
		return nil, internalErr("保存插件配置失败: " + err.Error())
	}

	cfg.values = normalized
	cfg.source = plugincfg.SourceFile
	cfg.pg.Instances = buildInstances(normalized, true, cfg.envInstances)
	cfg.pg.IncludeSystem = normalized.DatabasesIncludeSystem
	warnings := []string{}
	if cfg.envErr != nil {
		// 文件里已有可用的默认实例，多实例环境变量的解析错误不再阻断页面。
		warnings = append(warnings, "多实例环境变量配置有误，已忽略："+cfg.envErr.Error())
		cfg.pg.Err = nil
		cfg.envErr = nil
	}
	return encodeResult(plugincfg.SetResult{
		Saved: true, Applied: true, RestartRequired: false, Warnings: warnings,
	}, "db-postgres.config.set")
}

// pingResult 组装 kore.ping 结果。
func pingResult(cfg config) json.RawMessage {
	data, _ := json.Marshal(statusResult{
		Status:     "ready",
		UptimeS:    int64(time.Since(cfg.startedAt).Seconds()),
		Goroutines: runtime.NumGoroutine(),
		RSSKB:      rssKB(),
	})
	return data
}

// decodeParams 要求 params 是 JSON 对象并反序列化到目标形状。
func decodeParams(raw json.RawMessage, target any) *pluginapi.Error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return invalid("请传入参数对象 params（不能为空或 null）")
	}
	if trimmed[0] != '{' {
		return invalid("params 需要是 JSON 对象，请传 {...} 格式")
	}
	if err := json.Unmarshal(trimmed, target); err != nil {
		return invalid("params 解析失败，请检查 JSON 格式: " + err.Error())
	}
	return nil
}

func invalid(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INVALID, Message: msg}
}

func internalErr(msg string) *pluginapi.Error {
	return &pluginapi.Error{Code: pluginapi.E_INTERNAL, Message: msg}
}

// mustMarshal 序列化内部结构；失败只可能来自编程错误，返回空对象兜底而不是 panic。
func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

// encodeResult 序列化方法结果为响应载荷。
func encodeResult(v any, method string) (json.RawMessage, *pluginapi.Error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, internalErr("序列化 " + method + " 结果失败: " + err.Error())
	}
	return data, nil
}
