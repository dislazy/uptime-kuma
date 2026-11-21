package httpapi

// MCP（Model Context Protocol）端点，对齐官方 server/routers/mcp-router.ts。
//
// 官方做法是把 api-router 里**所有无路径参数的 GET 操作**自动桥接成 MCP 工具，
// 当前只有两个：get_info、get_monitors。协议层用 @modelcontextprotocol/server
// 的 createMcpHandler（streamable HTTP 传输）。Go 侧没有现成 SDK，这里按官方
// SDK 的实测行为手写 JSON-RPC 2.0 over HTTP：
//
//	POST /mcp + Authorization: Bearer <api key>
//	→ text/event-stream，单条 "event: message\ndata: {json}\n\n"
//
// 实测对齐的行为（probe 脚本见提交说明）：
//   - initialize：回显客户端 protocolVersion（不在支持集合内则回最新版），
//     capabilities 只有 tools.listChanged=true
//   - notifications/*：202 Accepted，无 body
//   - tools/list：tools[{name, description, inputSchema:{type:object,properties:{}}}]
//   - tools/call：result.content=[{type:text,text:<payload JSON，2 空格缩进>}]；
//     工具内部报错 → isError:true 且 text 为错误信息
//   - ping：result {}
//   - 未知方法 → -32601 "Method not found"；未知工具 → -32602 "Tool X not found"
//   - 非 POST → 405，Content-Type 错 → 415，Accept 缺 event-stream → 406，
//     三者都是普通 JSON：{"jsonrpc":"2.0","error":{...},"id":null}
//
// 门禁与官方 router.all("/mcp", requireApiKey, ...) 一致：apiEnabled 未开 403、
// 限流 429、缺 key 401、key 无效 401，**先鉴权再校验方法**（所以 GET /mcp 无 key
// 时回 401 而不是 405）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/version"
)

// mcpProtocolVersion 是不在客户端版本集合内时回落的最新协议版本。
const mcpProtocolVersion = "2025-11-25"

// mcpSupportedVersions 是官方 SDK 2.3.x 接受的协议版本（会原样回显）。
var mcpSupportedVersions = []string{
	"2024-10-07",
	"2024-11-05",
	"2025-03-26",
	"2025-06-18",
	mcpProtocolVersion,
}

// mcpMaxBody 限制单次请求体大小，防止超大 payload 打爆内存。
const mcpMaxBody = 4 << 20

// rpcRequest 是 JSON-RPC 2.0 请求。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcError 是 JSON-RPC 2.0 错误对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpTool 是一个桥接自 REST 路由的工具（对齐官方 ApiOperation）。
type mcpTool struct {
	Name        string
	Description string
	Resolve     func(ctx context.Context) (any, error)
}

// MountMCPRoute 挂载 MCP 端点。用无方法的 pattern 匹配所有 HTTP 方法，
// 方法本身的校验在 handleMCP 里做（对齐 express 的 router.all）。
func MountMCPRoute(mux *http.ServeMux, api *API) {
	mux.HandleFunc("/mcp", api.handleMCP)
}

// mcpTools 返回桥接的工具列表，顺序与官方 apiOperations 注册顺序一致。
func (a *API) mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "get_info",
			Description: "The same payload the dashboard receives from the `info` socket event.",
			Resolve: func(ctx context.Context) (any, error) {
				return map[string]any{"info": a.infoPayload(ctx)}, nil
			},
		},
		{
			Name:        "get_monitors",
			Description: "The compact monitor list the dashboard receives after logging in, without secrets.",
			Resolve: func(ctx context.Context) (any, error) {
				monitors, err := a.compactMonitors(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"monitors": monitors}, nil
			},
		},
	}
}

// handleMCP 是 /mcp 的入口：先鉴权，再走 streamable HTTP 传输层校验。
func (a *API) handleMCP(w http.ResponseWriter, r *http.Request) {
	// 官方把 requireApiKey 挂在 router.all 的第一个 handler，
	// 所以任何方法都先过鉴权。
	if !a.requireAPIKey(w, r) {
		return
	}

	if r.Method != http.MethodPost {
		mcpTransportError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}

	if !isJSONContentType(r.Header.Get("Content-Type")) {
		mcpTransportError(w, http.StatusUnsupportedMediaType,
			"Unsupported Media Type: Content-Type must be application/json")
		return
	}

	if !acceptsMCP(r.Header.Get("Accept")) {
		mcpTransportError(w, http.StatusNotAcceptable,
			"Not Acceptable: Client must accept both application/json and text/event-stream")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody))
	if err != nil {
		mcpTransportError(w, http.StatusBadRequest, "Failed to read the request body")
		return
	}

	var req rpcRequest
	// 官方 SDK 对批量数组只处理第一条（实测返回单条响应）
	if len(bytes.TrimSpace(body)) > 0 && bytes.TrimSpace(body)[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			mcpParseError(w)
			return
		}
		if len(batch) == 0 {
			mcpParseError(w)
			return
		}
		body = batch[0]
	}

	if err := json.Unmarshal(body, &req); err != nil {
		mcpParseError(w)
		return
	}
	if req.Method == "" {
		mcpReplyError(w, req.ID, -32600, "Invalid Request")
		return
	}

	// 通知（无 id）只回 202，不写 body
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	a.mcpDispatch(w, r, req)
}

// mcpDispatch 处理 JSON-RPC 方法。
func (a *API) mcpDispatch(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	switch req.Method {
	case "initialize":
		a.mcpInitialize(w, req)

	case "ping":
		mcpReply(w, req.ID, map[string]any{})

	case "tools/list":
		tools := make([]map[string]any, 0, 2)
		for _, t := range a.mcpTools() {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			})
		}
		mcpReply(w, req.ID, map[string]any{"tools": tools})

	case "tools/call":
		a.mcpToolsCall(w, r, req)

	default:
		mcpReplyError(w, req.ID, -32601, "Method not found")
	}
}

// mcpInitialize 回 capabilities/serverInfo，并对齐官方 SDK 的版本协商：
// 客户端版本在支持集合内就原样回显，否则回最新支持的版本。
func (a *API) mcpInitialize(w http.ResponseWriter, req rpcRequest) {
	var params struct {
		ProtocolVersion *string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	if params.ProtocolVersion == nil || *params.ProtocolVersion == "" {
		mcpReplyError(w, req.ID, -32602, "Invalid params")
		return
	}

	agreed := mcpProtocolVersion
	for _, v := range mcpSupportedVersions {
		if v == *params.ProtocolVersion {
			agreed = v
			break
		}
	}

	mcpReply(w, req.ID, map[string]any{
		"protocolVersion": agreed,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": true},
		},
		"serverInfo": map[string]any{
			"name":    "uptime-kuma",
			"version": version.Version,
		},
	})
}

// mcpToolsCall 执行工具并包装成 content 块。
func (a *API) mcpToolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}

	var tool *mcpTool
	tools := a.mcpTools()
	for i := range tools {
		if tools[i].Name == params.Name {
			tool = &tools[i]
			break
		}
	}
	if tool == nil {
		mcpReplyError(w, req.ID, -32602, "Tool "+params.Name+" not found")
		return
	}

	payload, err := tool.Resolve(r.Context())
	if err != nil {
		logx.Errorf("httpapi", "MCP 工具 %s 执行失败: %v", tool.Name, err)
		// 官方 jsonResult 之外的工具抛错路径：content 里放错误信息 + isError
		mcpReply(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
		return
	}

	mcpReply(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": mcpJSONText(payload)}},
	})
}

// ---------------- 传输层辅助 ----------------

// mcpReply 写一条成功的 SSE 响应。
func mcpReply(w http.ResponseWriter, id json.RawMessage, result any) {
	writeMCPSSE(w, map[string]any{
		"result":  result,
		"jsonrpc": "2.0",
		"id":      rawID(id),
	})
}

// mcpReplyError 写一条 JSON-RPC 错误的 SSE 响应（HTTP 仍为 200，规范要求）。
func mcpReplyError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeMCPSSE(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawID(id),
		"error":   rpcError{Code: code, Message: msg},
	})
}

// mcpParseError 处理 JSON 解析失败：官方是 express.json() 直接 400 空 body，
// 这里给出规范化的 -32700，比空 body 更便于客户端排查。
func mcpParseError(w http.ResponseWriter) {
	mcpTransportError(w, http.StatusBadRequest, "Parse error")
}

// mcpTransportError 写传输层错误（普通 JSON，非 SSE），对齐官方 SDK：
// {"jsonrpc":"2.0","error":{"code":-32000,...},"id":null}
func mcpTransportError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"error":   rpcError{Code: -32000, Message: msg},
		"id":      nil,
	})
}

// writeMCPSSE 按官方 SDK 的帧格式输出单条消息。
func writeMCPSSE(w http.ResponseWriter, payload any) {
	data, err := jsonNoEscape(payload)
	if err != nil {
		mcpTransportError(w, http.StatusInternalServerError, "Failed to handle the MCP request")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("event: message\ndata: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

// rawID 把请求 id 原样回显；缺失时给 null。
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

// mcpJSONText 生成工具结果的 text 块内容：官方用 JSON.stringify(payload, null, 2)。
func mcpJSONText(payload any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return fmt.Sprintf("%v", payload)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// jsonNoEscape 序列化时不转义 HTML 字符（官方 JSON.stringify 的行为）。
func jsonNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// isJSONContentType 判断 Content-Type 是否为 application/json（允许带 charset）。
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// acceptsMCP 校验 Accept 同时包含 application/json 与 text/event-stream。
//
// 官方 SDK 只认这两个字面值：`*/*`、`application/*` 一律 406（实测），
// 缺 Accept 头同样 406。
func acceptsMCP(accept string) bool {
	if accept == "" {
		return false
	}
	var jsonOK, sseOK bool
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch mediaType {
		case "application/json":
			jsonOK = true
		case "text/event-stream":
			sseOK = true
		}
	}
	return jsonOK && sseOK
}
