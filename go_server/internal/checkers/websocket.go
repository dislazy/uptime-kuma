package checkers

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// WebSocketChecker 尝试完成一次 WebSocket 升级握手，并按关闭码判定。
type WebSocketChecker struct{}

// Type 实现 Checker。
func (WebSocketChecker) Type() string { return "websocket-upgrade" }

func init() { Register(WebSocketChecker{}) }

// wsCloseCodes 是官方给关闭码配的友好名称，失败时直接展示给用户。
var wsCloseCodes = map[int]string{
	1002: "Protocol error",
	1003: "Unsupported Data",
	1005: "No Status Received",
	1006: "Abnormal Closure",
	1007: "Invalid frame payload data",
	1008: "Policy Violation",
	1009: "Message Too Big",
	1010: "Mandatory Extension Missing",
	1011: "Internal Error",
	1012: "Service Restart",
	1013: "Try Again Later",
	1014: "Bad Gateway",
	1015: "TLS Handshake Failed",
	3000: "Unauthorized",
	3003: "Forbidden",
	3008: "Timeout",
}

// Check 建立连接、等服务器是否立刻关闭，然后用关闭码对照"可接受状态码"。
//
// 注意这里的 accepted_statuscodes 语义是 **WebSocket 关闭码**（默认 ["1000"]），
// 不是 HTTP 状态码。
func (WebSocketChecker) Check(ctx context.Context, m *model.Monitor) Result {
	rawURL := strings.TrimSpace(m.URL())
	if rawURL == "" {
		return Down("URL is empty")
	}

	headers := wsHeaders(m)

	dialer := &websocket.Dialer{
		HandshakeTimeout: checkTimeoutOrDefault(ctx, 20*time.Second),
		Subprotocols:     parseSubprotocols(m.Get("ws_subprotocol")),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: m.GetBool("ignore_tls"),
		},
	}

	start := time.Now()
	conn, resp, err := dialer.DialContext(ctx, rawURL, headers)
	if err != nil {
		// 有些服务端完成了 101 升级却没按规范回 Sec-WebSocket-Accept，
		// 用户勾选"忽略该头"时应当视为成功
		if m.GetBool("ws_ignore_sec_websocket_accept_header") && resp != nil &&
			resp.StatusCode == http.StatusSwitchingProtocols {
			return Up("1000 - OK", time.Since(start).Milliseconds())
		}
		return Down("%s", wsDialError(err, resp))
	}
	defer conn.Close()

	ping := time.Since(start).Milliseconds()

	// 握手成功不代表服务可用：有些服务会立刻发 close 表示拒绝
	code, reason := awaitClose(conn)
	if code == 0 {
		code = 1000
	}

	spec := strings.TrimSpace(m.Get("accepted_statuscodes_json"))
	if spec == "" {
		spec = `["1000"]`
	}

	if statusAccepted(code, spec) {
		if reason != "" {
			return Up(reason, ping)
		}
		return Up(fmt.Sprintf("%d - OK", code), ping)
	}

	if name, ok := wsCloseCodes[code]; ok {
		return Down("%s", name)
	}
	return Down("Unexpected status code: %d", code)
}

// wsHeaders 组装握手请求头：自定义头 + Basic / Bearer 认证。
func wsHeaders(m *model.Monitor) http.Header {
	headers := http.Header{}

	if raw := strings.TrimSpace(m.Get("headers")); raw != "" {
		var custom map[string]string
		if err := json.Unmarshal([]byte(raw), &custom); err == nil {
			for k, v := range custom {
				headers.Set(k, v)
			}
		}
	}

	switch strings.ToLower(strings.TrimSpace(m.Get("auth_method"))) {
	case "basic":
		user := m.Get("basic_auth_user")
		pass := m.Get("basic_auth_pass")
		if user != "" || pass != "" {
			token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			headers.Set("Authorization", "Basic "+token)
		}
	case "bearer":
		if tok := strings.TrimSpace(m.Get("bearer_token")); tok != "" {
			headers.Set("Authorization", "Bearer "+tok)
		}
	}

	return headers
}

// parseSubprotocols 把 "graphql-ws, mqtt" 这种配置拆成列表。
func parseSubprotocols(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// 官方会先去掉所有空白字符
		p = strings.Join(strings.Fields(p), "")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// awaitClose 等服务器主动关闭，返回关闭码与原因。
//
// 超时未收到任何帧时返回 0，表示连接保持正常（这正是"服务可用"的信号）。
func awaitClose(conn *websocket.Conn) (int, string) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			// 收到业务消息说明连接是活的，继续等下一次读
			continue
		}

		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			return closeErr.Code, closeErr.Text
		}
		return 0, ""
	}
}

// wsDialError 优先用 HTTP 状态码描述握手失败，比 gorilla 的 "bad handshake" 更有用。
func wsDialError(err error, resp *http.Response) string {
	if resp != nil {
		if text := http.StatusText(resp.StatusCode); text != "" {
			return fmt.Sprintf("%d - %s", resp.StatusCode, text)
		}
		return fmt.Sprintf("%d", resp.StatusCode)
	}
	return simplifyNetErr(err).Error()
}

// checkTimeoutOrDefault 取 ctx 剩余时间，没有则用默认值。
func checkTimeoutOrDefault(ctx context.Context, def time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
	}
	return def
}
