package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// newTestAPI 造一个最小可用的 API（临时 SQLite + 真实 store），
// 只覆盖 MCP 端点需要的依赖。
func newTestAPI(t *testing.T) *API {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("初始化 schema 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &API{
		DB:         db,
		Mon:        store.NewMonitorStore(db),
		Beat:       store.NewHeartbeatStore(db),
		Stat:       store.NewStatsStore(db),
		Set:        store.NewSettings(db),
		StatusPage: store.NewStatusPageStore(db),
	}
}

// rpcCall 发一条 JSON-RPC 请求并返回 SSE 里的 data 行解析结果。
func rpcCall(t *testing.T, api *API, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// 绕过 requireApiKey：直接走 dispatch
	var rpcReq rpcRequest
	if err := json.Unmarshal([]byte(body), &rpcReq); err != nil {
		t.Fatalf("测试 body 非法: %v", err)
	}
	rec := httptest.NewRecorder()
	api.mcpDispatch(rec, req, rpcReq)

	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	raw := rec.Body.String()
	if !strings.HasPrefix(raw, "event: message\ndata: ") || !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("SSE 帧格式不符: %q", raw)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(raw, "event: message\ndata: "), "\n\n")
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("SSE data 不是 JSON: %v (%q)", err, payload)
	}
	return rec.Code, out
}

// TestMCPInitialize 对齐官方 SDK：回显支持的版本，不支持则回最新版，
// capabilities 只有 tools.listChanged。
func TestMCPInitialize(t *testing.T) {
	api := newTestAPI(t)

	cases := map[string]string{
		"2024-11-05": "2024-11-05", // 支持 → 原样回显
		"2025-03-26": "2025-03-26",
		"2025-06-18": "2025-06-18",
		"2026-01-01": mcpProtocolVersion, // 不支持 → 最新版
		"1999-01-01": mcpProtocolVersion,
	}
	for in, want := range cases {
		_, out := rpcCall(t, api, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+in+`","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
		result, ok := out["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: 缺少 result: %+v", in, out)
		}
		if result["protocolVersion"] != want {
			t.Fatalf("%s: protocolVersion = %v, want %s", in, result["protocolVersion"], want)
		}
		caps, _ := result["capabilities"].(map[string]any)
		tools, _ := caps["tools"].(map[string]any)
		if tools["listChanged"] != true {
			t.Fatalf("%s: capabilities 应为 tools.listChanged=true，实际 %+v", in, caps)
		}
		info, _ := result["serverInfo"].(map[string]any)
		if info["name"] != "uptime-kuma" || info["version"] == "" {
			t.Fatalf("%s: serverInfo = %+v", in, info)
		}
	}
}

// TestMCPToolsList 校验工具清单与官方桥接结果一致。
func TestMCPToolsList(t *testing.T) {
	api := newTestAPI(t)
	_, out := rpcCall(t, api, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("工具数 = %d, want 2: %+v", len(tools), result)
	}

	want := []string{"get_info", "get_monitors"}
	for i, name := range want {
		tool, _ := tools[i].(map[string]any)
		if tool["name"] != name {
			t.Fatalf("第 %d 个工具 = %v, want %s", i, tool["name"], name)
		}
		if d, _ := tool["description"].(string); d == "" {
			t.Fatalf("%s 缺少 description", name)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("%s inputSchema.type = %v", name, schema["type"])
		}
		if props, ok := schema["properties"].(map[string]any); !ok || len(props) != 0 {
			t.Fatalf("%s inputSchema.properties 应为空对象: %+v", name, schema["properties"])
		}
	}
}

// TestMCPToolsCall 工具结果必须是单个 text 块，内容是 2 空格缩进的 JSON，
// 且 payload 顶层没有 REST 的 ok 字段（官方 jsonResult 语义）。
func TestMCPToolsCall(t *testing.T) {
	api := newTestAPI(t)

	for _, name := range []string{"get_info", "get_monitors"} {
		_, out := rpcCall(t, api, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`)
		if _, hasErr := out["error"]; hasErr {
			t.Fatalf("%s 报错: %+v", name, out["error"])
		}
		result, _ := out["result"].(map[string]any)
		content, _ := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("%s content 长度 = %d", name, len(content))
		}
		block, _ := content[0].(map[string]any)
		if block["type"] != "text" {
			t.Fatalf("%s content[0].type = %v", name, block["type"])
		}
		text, _ := block["text"].(string)
		if strings.Contains(text, "\n  \"ok\"") {
			t.Fatalf("%s payload 不应含 ok 字段: %s", name, text)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatalf("%s text 不是 JSON: %v", name, err)
		}
		if _, ok := payload[strings.TrimPrefix(name, "get_")]; !ok {
			t.Fatalf("%s payload 缺少 %s 键: %+v", name, strings.TrimPrefix(name, "get_"), payload)
		}
	}
}

// TestMCPErrors 对齐官方错误码：未知方法 -32601、未知工具 -32602。
func TestMCPErrors(t *testing.T) {
	api := newTestAPI(t)

	_, out := rpcCall(t, api, `{"jsonrpc":"2.0","id":4,"method":"nope"}`)
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != float64(-32601) || errObj["message"] != "Method not found" {
		t.Fatalf("未知方法错误 = %+v", errObj)
	}

	_, out = rpcCall(t, api, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != float64(-32602) || errObj["message"] != "Tool nope not found" {
		t.Fatalf("未知工具错误 = %+v", errObj)
	}

	// 能力未注册：官方 SDK 同样 -32601
	_, out = rpcCall(t, api, `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`)
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != float64(-32601) {
		t.Fatalf("resources/list = %+v", errObj)
	}

	// initialize 缺 protocolVersion → 规范化为 -32602
	_, out = rpcCall(t, api, `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`)
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != float64(-32602) {
		t.Fatalf("initialize 缺版本 = %+v", errObj)
	}
}

// TestMCPTransportGates 传输层门禁：Accept 必须字面包含两种类型（`*/*` 也不行）。
func TestMCPTransportGates(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"application/json, text/event-stream", true},
		{"text/event-stream, application/json", true},
		{"application/json,text/event-stream;q=0.9", true},
		{"application/json", false},
		{"text/event-stream", false},
		{"*/*", false},
		{"application/*, text/*", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := acceptsMCP(tc.accept); got != tc.want {
			t.Fatalf("acceptsMCP(%q) = %v, want %v", tc.accept, got, tc.want)
		}
	}

	if !isJSONContentType("application/json") || !isJSONContentType("application/json; charset=utf-8") {
		t.Fatal("application/json 应通过")
	}
	for _, bad := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		if isJSONContentType(bad) {
			t.Fatalf("%q 不应通过", bad)
		}
	}
}

// TestMCPJSONTextIndent 工具结果 text 块用 2 空格缩进（官方 JSON.stringify(payload, null, 2)）。
func TestMCPJSONTextIndent(t *testing.T) {
	got := mcpJSONText(map[string]any{"a": 1})
	if got != "{\n  \"a\": 1\n}" {
		t.Fatalf("缩进不符: %q", got)
	}
	// 不转义 HTML 字符（官方 JSON.stringify 行为）
	if out := mcpJSONText(map[string]any{"u": "a<b&c"}); !strings.Contains(out, "a<b&c") {
		t.Fatalf("HTML 字符被转义: %q", out)
	}
}

// TestMCPBodyKeepsContext 保证 dispatch 使用请求 ctx（取消后工具仍能读到依赖）。
func TestMCPBodyKeepsContext(t *testing.T) {
	api := newTestAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	api.mcpDispatch(rec, req, rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "ping"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"result":{}`) {
		t.Fatalf("ping 响应异常: %d %q", rec.Code, rec.Body.String())
	}
}
