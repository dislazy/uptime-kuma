package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 这个文件是"表驱动渠道"的公共设施。
//
// 113 个渠道里有 80 多个是同一个模式：拼一个 URL、POST/GET 一段 JSON 或表单，
// 看 HTTP 状态码决定成败。为每个渠道写一个 struct + 两个方法太啰嗦，
// 所以统一用 providerFunc 注册，只写真正有差异的那部分。

// providerFunc 把"一个函数"适配成 Provider + ContextProvider。
type providerFunc struct {
	typ string
	fn  func(ctx context.Context, cfg map[string]any, m Message) (string, error)
}

// Type 实现 Provider。
func (p providerFunc) Type() string { return p.typ }

// Send 实现 Provider：只有文案没有结构化上下文时也能工作。
func (p providerFunc) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return p.fn(ctx, cfg, Message{Msg: msg, Test: true})
}

// SendMessage 实现 ContextProvider。
func (p providerFunc) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	return p.fn(ctx, cfg, m)
}

// registerFunc 注册一个函数式渠道。
func registerFunc(typ string, fn func(ctx context.Context, cfg map[string]any, m Message) (string, error)) {
	Register(providerFunc{typ: typ, fn: fn})
}

// ---------- 文案助手 ----------

// msgTitle 返回标题：正式告警用监控名，测试消息用 "Uptime Kuma"。
func msgTitle(m Message) string {
	if m.MonitorName() != "" {
		return m.MonitorName()
	}
	return "Uptime Kuma"
}

// msgBody 返回正文，空串兜底为 N/A。
func msgBody(m Message) string {
	if m.Msg == "" {
		return "N/A"
	}
	return m.Msg
}

// isUp 判断是否处于 UP；测试消息（状态 -1）返回 false。
func isUp(m Message) bool {
	return m.Status() == 1
}

// ---------- HTTP 助手 ----------

// notifyHTTP 是渠道专用的 HTTP 客户端。
// 别用 http.DefaultClient：它没有超时，一个卡住的 webhook 会拖住整个引擎
// （引擎侧虽然也有 ctx 超时，但连接建立阶段最好再兜一层）。
var notifyHTTP = &http.Client{Timeout: 20 * time.Second}

// httpDoRaw 发请求，返回状态码与响应体（截断到 4KB，避免把整页 HTML 塞进日志）。
func httpDoRaw(ctx context.Context, method, target, contentType string, body []byte, headers map[string]string) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return 0, "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Uptime-Kuma-Go")
	}

	res, err := notifyHTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	return res.StatusCode, string(raw), nil
}

// getURL 发 GET。
func getURL(ctx context.Context, target string, headers map[string]string) (int, string, error) {
	return httpDoRaw(ctx, http.MethodGet, target, "", nil, headers)
}

// postFormValues 以 application/x-www-form-urlencoded 发送。
func postFormValues(ctx context.Context, target string, form url.Values, headers map[string]string) (int, string, error) {
	return httpDoRaw(ctx, http.MethodPost, target, "application/x-www-form-urlencoded", []byte(form.Encode()), headers)
}

// postJSONBody 以 application/json 发送任意对象。
func postJSONBody(ctx context.Context, target string, payload any, headers map[string]string) (int, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	return httpDoRaw(ctx, http.MethodPost, target, "application/json", raw, headers)
}

// expectOK 把非 2xx 响应转成错误，错误里带状态码与响应体片段，方便用户自查。
//
// 官方的文案风格是直接抛 API 返回的错误文本，这里保持一致：
// 响应体不为空就用响应体，为空才用 "HTTP 500"。
func expectOK(provider string, code int, body string) error {
	if code >= 200 && code < 300 {
		return nil
	}
	snippet := strings.TrimSpace(body)
	if len(snippet) > 300 {
		snippet = snippet[:300] + "..."
	}
	if snippet == "" {
		return fmt.Errorf("%s: HTTP %d", provider, code)
	}
	return fmt.Errorf("%s: HTTP %d: %s", provider, code, snippet)
}

// postJSONOK 发 JSON 并在非 2xx 时报错，成功返回官方统一文案。
func postJSONOK(ctx context.Context, provider, target string, payload any, headers map[string]string) (string, error) {
	code, body, err := postJSONBody(ctx, target, payload, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK(provider, code, body); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// postFormOK 发表单并在非 2xx 时报错。
func postFormOK(ctx context.Context, provider, target string, form url.Values, headers map[string]string) (string, error) {
	code, body, err := postFormValues(ctx, target, form, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK(provider, code, body); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// getOK 发 GET 并在非 2xx 时报错。
func getOK(ctx context.Context, provider, target string, headers map[string]string) (string, error) {
	code, body, err := getURL(ctx, target, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK(provider, code, body); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------- 配置读取助手 ----------

// cfgRaw 取原始配置值。
func cfgRaw(cfg map[string]any, key string) any {
	if cfg == nil {
		return nil
	}
	return cfg[key]
}

// cfgFloat 读数字配置。
func cfgFloat(cfg map[string]any, key string) float64 {
	v := cfgRaw(cfg, key)
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		var f float64
		_, _ = fmt.Sscan(t, &f)
		return f
	default:
		return 0
	}
}

// cfgStringList 读数组配置（如 recipients / tags）。
func cfgStringList(cfg map[string]any, key string) []string {
	v := cfgRaw(cfg, key)
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, cfgValueString(item))
		}
		return out
	case []string:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		// 也允许逗号分隔
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return nil
	}
}

func cfgValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// requireCfg 检查必填项，缺失时返回与前端提示一致的错误。
func requireCfg(cfg map[string]any, key, label string) (string, error) {
	v := strings.TrimSpace(cfgStr(cfg, key))
	if v == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return v, nil
}

// joinURL 拼 base + path，自动处理斜杠。
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// ---------------- 全局设置 ----------------

// SettingsReader 让渠道读取全局设置（Web Push 的 VAPID 密钥、Steam Key 等）。
type SettingsReader interface {
	GetString(ctx context.Context, key, def string) string
}

var globalSettings SettingsReader

// SetSettings 注入设置读取器，由 main 启动时调用。
func SetSettings(r SettingsReader) { globalSettings = r }

// Setting 读取全局设置；未注入时返回 def。
func Setting(ctx context.Context, key, def string) string {
	if globalSettings == nil {
		return def
	}
	if v := globalSettings.GetString(ctx, key, ""); v != "" {
		return v
	}
	return def
}
