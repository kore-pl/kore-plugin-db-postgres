package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pengtang1024/kore-plugin-db-postgres/internal/pgop"
	"github.com/pengtang1024/kore-plugin-db-postgres/internal/plugincfg"
	"github.com/pengtang1024/kore/pkg/pluginapi"
)

// testConfig 构造握手与会话循环共用的一致配置（状态目录落 t.TempDir）。
func testConfig(t *testing.T) *config {
	t.Helper()
	dir := t.TempDir()
	values := defaultValues()
	return &config{
		id:           "db-postgres",
		version:      "0.2.0",
		requiresKore: ">=0.1.0 <0.2.0",
		capabilities: []string{"system:admin"},
		startedAt:    time.Now(),
		stateDir:     dir,
		store:        plugincfg.NewStore(dir),
		values:       values,
		source:       plugincfg.SourceDefault,
		pg: pgop.Config{
			Instances: buildInstances(values, true, nil),
			Admin:     true,
		},
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readFrame(t *testing.T, conn net.Conn) any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	payload, err := pluginapi.ReadFrame(conn, pluginapi.MaxFrameSize)
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	env, err := pluginapi.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("解码帧失败: %v", err)
	}
	return env
}

func readRequest(t *testing.T, conn net.Conn) *pluginapi.Request {
	t.Helper()
	request, ok := readFrame(t, conn).(*pluginapi.Request)
	if !ok {
		t.Fatal("帧类型不是 request")
	}
	return request
}

func writeResponse(t *testing.T, conn net.Conn, id, reqID string, result json.RawMessage) {
	t.Helper()
	if err := pluginapi.WriteJSON(conn, &pluginapi.Response{
		Type: pluginapi.TypeResponse, ID: id, ReqID: reqID, Result: result,
	}); err != nil {
		t.Fatalf("写响应失败: %v", err)
	}
}

// TestHandshake 校验 hello 首帧字段与三步握手。
func TestHandshake(t *testing.T) {
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
	go func() { done <- handshake(pluginConn, *testConfig(t)); _ = pluginConn.Close() }()

	request := readRequest(t, kernelConn)
	if request.Method != helloMethod {
		t.Fatalf("首帧方法=%q，期望 %q", request.Method, helloMethod)
	}
	var params helloParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("解析 hello 参数失败: %v", err)
	}
	if params.PluginID != "db-postgres" || params.ID != "db-postgres" {
		t.Fatalf("plugin_id/id=%q/%q，期望 db-postgres", params.PluginID, params.ID)
	}
	if params.Protocol != pluginapi.Protocol || params.Version != "0.2.0" {
		t.Fatalf("protocol/version=%q/%q 不符合 §5", params.Protocol, params.Version)
	}
	if params.PID != os.Getpid() {
		t.Fatalf("pid=%d，期望 %d", params.PID, os.Getpid())
	}
	result, err := json.Marshal(helloResult{OK: true, Protocol: pluginapi.Protocol})
	if err != nil {
		t.Fatalf("序列化握手响应失败: %v", err)
	}
	writeResponse(t, kernelConn, request.ID, request.ReqID, result)
	if err := <-done; err != nil {
		t.Fatalf("握手返回错误: %v", err)
	}
}

// TestServeShutdown 会话循环收到 kore.shutdown 后优雅退出。
// net.Pipe 无缓冲：写完 shutdown 请求后必须读走响应帧，否则 serve 写响应阻塞而死锁。
func TestServeShutdown(t *testing.T) {
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
	go func() { done <- serve(pluginConn, testConfig(t), testLogger()); _ = pluginConn.Close() }()

	request := &pluginapi.Request{
		Type: pluginapi.TypeRequest, ID: "id-1", ReqID: "r-1", Method: "kore.shutdown",
	}
	if err := pluginapi.WriteJSON(kernelConn, request); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	env := readFrame(t, kernelConn)
	response, ok := env.(*pluginapi.Response)
	if !ok || response.ID != "id-1" {
		t.Fatalf("响应帧异常: %+v", env)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
}

// TestServeMethods 会话循环上真实走一遍配置契约与只读白名单（不依赖数据库）。
func TestServeMethods(t *testing.T) {
	pluginConn, kernelConn := net.Pipe()
	defer func() { _ = kernelConn.Close() }()
	done := make(chan error, 1)
	go func() { done <- serve(pluginConn, testConfig(t), testLogger()); _ = pluginConn.Close() }()

	write := func(id, method, params string, wantErr string) {
		t.Helper()
		req := &pluginapi.Request{
			Type: pluginapi.TypeRequest, ID: id, ReqID: "r-" + id,
			Method: method, Params: json.RawMessage(params),
		}
		if err := pluginapi.WriteJSON(kernelConn, req); err != nil {
			t.Fatalf("写请求失败: %v", err)
		}
		env := readFrame(t, kernelConn)
		resp, ok := env.(*pluginapi.Response)
		if !ok || resp.ID != id {
			t.Fatalf("响应帧异常: %+v", env)
		}
		if wantErr == "" {
			if resp.Error != nil {
				t.Fatalf("%s 意外错误: %v", method, resp.Error)
			}
			return
		}
		if resp.Error == nil || resp.Error.Code != wantErr {
			t.Fatalf("%s 错误=%v，期望 %s", method, resp.Error, wantErr)
		}
	}
	write("a", "db-postgres.config.get", `{}`, "")
	write("b", "db-postgres.query", `{"instance":"default","sql":"DROP TABLE x"}`, pluginapi.E_INVALID)
	write("c", "db-postgres.exec", `{}`, pluginapi.E_INVALID)

	// 收尾 shutdown 也配对读帧。
	write("d", "kore.shutdown", `{}`, "")
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
}

// TestConfigFileMode0600 配置落盘后必须是 0600（密码只存在本机私密文件里）。
// 状态目录由内核负责创建并保持 0700；插件只在目录缺失时按 0700 补建。
func TestConfigFileMode0600(t *testing.T) {
	cfg := testConfig(t)
	cfg.stateDir = filepath.Join(t.TempDir(), "plugins", "db-postgres")
	cfg.store = plugincfg.NewStore(cfg.stateDir)
	params := json.RawMessage(`{"values":{"host":"127.0.0.1","port":15432,"user":"postgres","password":"pw-for-mode"}}`)
	if _, perr := handleConfigSet(params, cfg); perr != nil {
		t.Fatalf("保存配置失败: %v", perr)
	}
	path := filepath.Join(cfg.stateDir, "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 配置失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("配置文件权限=%o，期望 600", perm)
	}
	dirInfo, err := os.Stat(cfg.stateDir)
	if err != nil {
		t.Fatalf("stat 状态目录失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("状态目录权限=%o，期望 700", perm)
	}
}
