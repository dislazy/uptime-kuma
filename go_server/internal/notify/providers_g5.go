package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// 本文件移植 g5 组 14 个官方渠道。
// URL / 方法 / 请求体 / headers / 必填校验 / 成功文案均照 server/notification-providers/*.js。
// 包级标识符统一加 g5 前缀，避免与其它 provider 文件重名。

func init() {
	registerFunc("bearsms", sendG5BearSMS)
	registerFunc("smsc", sendG5SMSC)
	registerFunc("GoogleSheets", sendG5GoogleSheets)
	registerFunc("Keep", sendG5Keep)
	registerFunc("notifyapp", sendG5NotifyApp)
	registerFunc("Opsgenie", sendG5Opsgenie)
	registerFunc("pinglet", sendG5Pinglet)
	registerFunc("pushy", sendG5Pushy)
	registerFunc("SMSManager", sendG5SMSManager)
	registerFunc("Teltonika", sendG5Teltonika)
	registerFunc("GoAlert", sendG5GoAlert)
	registerFunc("notifery", sendG5Notifery)
	registerFunc("SendGrid", sendG5SendGrid)
	registerFunc("HaloPSA", sendG5HaloPSA)
}

// ---------------- 通用小工具 ----------------

// g5ISO 返回与 JS new Date().toISOString() 相同格式的 UTC 时间。
func g5ISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// g5EncodeComponent 近似 JS encodeURIComponent：Go 的 QueryEscape 会把空格编成 +，
// 这里换回 %20，与官方拼出的 URL 保持一致。
func g5EncodeComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// g5StripNonASCII 删除非 ASCII 字符，对应官方 msg.replace(/[^\x00-\x7F]/g, "")。
func g5StripNonASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] < 0x80 {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// g5HasNonASCII 判断是否含非 ASCII 字符，对应 /[^\x00-\x7F]/.test(...)。
func g5HasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// g5MonitorMap 把监控还原成官方 monitorJSON 的形态；测试消息返回 nil（JSON null）。
func g5MonitorMap(m Message) map[string]any {
	if m.Monitor == nil {
		return nil
	}
	out := make(map[string]any, len(m.Monitor.Raw)+4)
	for k, v := range m.Monitor.Raw {
		out[k] = v
	}
	out["id"] = m.Monitor.ID
	out["name"] = m.Monitor.Name
	if _, ok := out["type"]; !ok && m.Monitor.Type != "" {
		out["type"] = m.Monitor.Type
	}
	return out
}

// g5HeartbeatMap 把心跳还原成官方 heartbeatJSON；测试消息返回 nil（JSON null）。
func g5HeartbeatMap(m Message) map[string]any {
	if m.Monitor == nil && m.Test {
		return nil
	}
	h := m.Beat.JSON()
	return h
}

// g5ExtractAddress 复刻 notification-provider.js 的 extractAddress。
func g5ExtractAddress(m *model.Monitor) string {
	if m == nil {
		return ""
	}
	switch strings.ToLower(g5MonitorType(m)) {
	case "push":
		return "Heartbeat"
	case "ping", "dns":
		return m.Hostname()
	case "port", "gamedig", "steam":
		if p := m.Port(); p > 0 {
			return fmt.Sprintf("%s:%d", m.Hostname(), p)
		}
		return m.Hostname()
	case "globalping":
		switch strings.ToLower(m.Get("subtype")) {
		case "ping", "dns":
			return m.Hostname()
		case "http":
			return m.URL()
		default:
			return ""
		}
	default:
		u := m.URL()
		if u != "" && u != "https://" && u != "http://" {
			return u
		}
		return ""
	}
}

// g5MonitorType 取监控类型，优先用强类型字段。
func g5MonitorType(m *model.Monitor) string {
	if m.Type != "" {
		return m.Type
	}
	return m.Get("type")
}

// g5Snippet 取响应体前 200 字符用于错误提示。
func g5Snippet(s string) string {
	return truncateStr(s, 200)
}

// ---------------- BearSMS ----------------

func sendG5BearSMS(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	username, err := requireCfg(cfg, "bearsmsUsername", "bearsmsUsername")
	if err != nil {
		return "", err
	}
	hashKey, err := requireCfg(cfg, "bearsmsHashKey", "bearsmsHashKey")
	if err != nil {
		return "", err
	}
	to, err := requireCfg(cfg, "bearsmsPhoneNumber", "bearsmsPhoneNumber")
	if err != nil {
		return "", err
	}

	// BearSMS 会拒绝含 emoji 的消息（返回 error 200），所以先去掉状态图标。
	cleanMsg := strings.ReplaceAll(m.Msg, "🔴 ", "")
	cleanMsg = strings.ReplaceAll(cleanMsg, "✅ ", "")

	q := url.Values{}
	q.Set("app", "ws")
	q.Set("u", username)
	q.Set("h", hashKey)
	q.Set("op", "pv")
	q.Set("to", to)
	q.Set("msg", cleanMsg)
	if s := strings.TrimSpace(cfgStr(cfg, "bearsmsSenderId")); s != "" {
		q.Set("from", s)
	}
	// 非 GSM 文本（如希伯来文）必须标记为 unicode
	if g5HasNonASCII(cleanMsg) {
		q.Set("unicode", "1")
	}

	target := "https://app.bearsms.com/index.php?" + q.Encode()
	code, body, err := getURL(ctx, target, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("bearsms", code, body); err != nil {
		return "", err
	}

	// BearSMS 失败时也返回 HTTP 200，必须看 body。
	var res struct {
		Status      string `json:"status"`
		ErrorString any    `json:"error_string"`
		Data        []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", fmt.Errorf("bearsms: 无法解析响应: %s", g5Snippet(body))
	}

	errStr := ""
	if res.ErrorString != nil {
		errStr = fmt.Sprintf("%v", res.ErrorString)
	}
	ok := false
	for _, r := range res.Data {
		if r.Status == "OK" {
			ok = true
			break
		}
	}
	if res.Status == "ERR" || errStr != "" || !ok {
		if errStr != "" {
			return "", fmt.Errorf("%s", errStr)
		}
		return "", fmt.Errorf("%s", strings.TrimSpace(body))
	}

	return "Sent Successfully.", nil
}

// ---------------- SMSC ----------------

func sendG5SMSC(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	login, err := requireCfg(cfg, "smscLogin", "smscLogin")
	if err != nil {
		return "", err
	}
	psw, err := requireCfg(cfg, "smscPassword", "smscPassword")
	if err != nil {
		return "", err
	}
	phones, err := requireCfg(cfg, "smscToNumber", "smscToNumber")
	if err != nil {
		return "", err
	}

	parts := []string{
		"fmt=3",
		"translit=" + cfgStr(cfg, "smscTranslit"),
		"login=" + login,
		"psw=" + psw,
		"phones=" + phones,
		// 官方先剔除非 ASCII，再做 encodeURIComponent
		"mes=" + g5EncodeComponent(g5StripNonASCII(m.Msg)),
	}
	if s := strings.TrimSpace(cfgStr(cfg, "smscSenderName")); s != "" {
		parts = append(parts, "sender="+s)
	}

	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/json",
	}
	target := "https://smsc.kz/sys/send.php?" + strings.Join(parts, "&")
	code, body, err := getURL(ctx, target, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("smsc", code, body); err != nil {
		return "", err
	}

	var res map[string]any
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", fmt.Errorf("Something gone wrong. Api returned code %s: %s", "", g5Snippet(body))
	}
	if _, ok := res["id"]; !ok {
		return "", fmt.Errorf("Something gone wrong. Api returned code %v: %v", res["error_code"], res["error"])
	}

	return "Sent Successfully.", nil
}

// ---------------- GoogleSheets ----------------

func sendG5GoogleSheets(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	webhook, err := requireCfg(cfg, "googleSheetsWebhookUrl", "googleSheetsWebhookUrl")
	if err != nil {
		return "", err
	}

	status := "N/A"
	monitorName := "N/A"
	monitorURL := "N/A"
	responseTime := "N/A"
	statusCode := "N/A"

	if m.Monitor != nil {
		if m.Monitor.Name != "" {
			monitorName = m.Monitor.Name
		}
		if a := g5ExtractAddress(m.Monitor); a != "" {
			monitorURL = a
		}
	}

	if m.Monitor != nil {
		switch m.Beat.Status {
		case model.StatusDown:
			status = "DOWN"
		case model.StatusUp:
			status = "UP"
		default:
			status = "UNKNOWN"
		}
		if m.Beat.Ping != nil {
			responseTime = cfgValueString(*m.Beat.Ping)
		}
	}

	// 注：官方还会写 heartbeat.statusCode（HTTP 监控的响应码），
	// 但 model.Beat 未持有该字段，此处按 N/A 处理。
	payload := map[string]any{
		"timestamp":    g5ISO(),
		"status":       status,
		"monitorName":  monitorName,
		"monitorUrl":   monitorURL,
		"message":      m.Msg,
		"responseTime": responseTime,
		"statusCode":   statusCode,
	}

	return postJSONOK(ctx, "GoogleSheets", webhook, payload, map[string]string{
		"Content-Type": "application/json",
	})
}

// ---------------- Keep ----------------

func sendG5Keep(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	rawURL, err := requireCfg(cfg, "webhookURL", "webhookURL")
	if err != nil {
		return "", err
	}
	apiKey, err := requireCfg(cfg, "webhookAPIKey", "webhookAPIKey")
	if err != nil {
		return "", err
	}

	base := strings.TrimSuffix(rawURL, "/")
	target := base + "/alerts/event/uptimekuma"

	payload := map[string]any{
		"heartbeat": g5HeartbeatMap(m),
		"monitor":   g5MonitorMap(m),
		"msg":       m.Msg,
	}

	return postJSONOK(ctx, "Keep", target, payload, map[string]string{
		"x-api-key":    apiKey,
		"content-type": "application/json",
	})
}

// ---------------- Notify! (notifyapp) ----------------

const g5NotifyAppBase = "https://push.getnotifyapp.com"

func sendG5NotifyApp(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	deviceID, err := requireCfg(cfg, "notifyAppDeviceId", "notifyAppDeviceId")
	if err != nil {
		return "", err
	}
	token, err := requireCfg(cfg, "notifyAppToken", "notifyAppToken")
	if err != nil {
		return "", err
	}

	title := "Uptime Kuma"
	if m.MonitorName() != "" {
		title = m.MonitorName()
	}
	if m.Monitor != nil {
		if m.Beat.Status == model.StatusUp {
			title += " is UP"
		} else {
			title += " is DOWN"
		}
	}

	body := map[string]any{
		"text":  m.Msg,
		"title": title,
	}
	if icon := strings.TrimSpace(cfgStr(cfg, "notifyAppIconUrl")); icon != "" {
		body["iconUrl"] = icon
	}
	// DOWN 时标记为 time-sensitive，穿透专注模式与免打扰
	if m.Monitor != nil && m.Beat.Status == model.StatusDown {
		body["timeSensitive"] = true
	}

	target := fmt.Sprintf("%s/notify-json/%s?token=%s",
		g5NotifyAppBase, g5EncodeComponent(deviceID), g5EncodeComponent(token))

	return postJSONOK(ctx, "notifyapp", target, body, nil)
}

// ---------------- Opsgenie ----------------

const (
	g5OpsgenieAlertsURLEU = "https://api.eu.opsgenie.com/v2/alerts"
	g5OpsgenieAlertsURLUS = "https://api.opsgenie.com/v2/alerts"
	g5OpsgenieText        = "Uptime Kuma Alert"
)

func sendG5Opsgenie(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "opsgenieApiKey", "opsgenieApiKey")
	if err != nil {
		return "", err
	}

	alertsURL := g5OpsgenieAlertsURLUS
	switch strings.ToLower(strings.TrimSpace(cfgStr(cfg, "opsgenieRegion"))) {
	case "us":
		alertsURL = g5OpsgenieAlertsURLUS
	case "eu":
		alertsURL = g5OpsgenieAlertsURLEU
	default:
		alertsURL = g5OpsgenieAlertsURLUS
	}

	priority := "3"
	if p := strings.TrimSpace(cfgStr(cfg, "opsgeniePriority")); p != "" && p != "0" {
		priority = p
	}

	// 测试消息：没有心跳上下文
	if m.Monitor == nil {
		return g5OpsgeniePost(ctx, apiKey, alertsURL, map[string]any{
			"message":  m.Msg,
			"alias":    "uptime-kuma-notification-test",
			"source":   "Uptime Kuma",
			"priority": "P5",
		})
	}

	switch m.Beat.Status {
	case model.StatusDown:
		return g5OpsgeniePost(ctx, apiKey, alertsURL, map[string]any{
			"message":     g5OpsgenieText + ": " + m.Monitor.Name,
			"alias":       m.Monitor.Name,
			"description": m.Msg,
			"source":      "Uptime Kuma",
			"priority":    "P" + priority,
		})
	case model.StatusUp:
		closeURL := fmt.Sprintf("%s/%s/close?identifierType=alias",
			alertsURL, g5EncodeComponent(m.Monitor.Name))
		return g5OpsgeniePost(ctx, apiKey, closeURL, map[string]any{
			"source": "Uptime Kuma",
		})
	}

	// 其它状态官方不发请求
	return "", nil
}

// g5OpsgeniePost 与官方一致：非 2xx 时以文案形式返回失败原因，而不是抛错。
func g5OpsgeniePost(ctx context.Context, apiKey, target string, payload map[string]any) (string, error) {
	code, _, err := postJSONBody(ctx, target, payload, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "GenieKey " + apiKey,
	})
	if err != nil {
		return "", err
	}
	if code == 0 {
		return "Opsgenie notification failed with invalid response!", nil
	}
	if code < 200 || code >= 300 {
		return fmt.Sprintf("Opsgenie notification failed with status code %d", code), nil
	}
	return "Sent Successfully.", nil
}

// ---------------- Pinglet ----------------

func sendG5Pinglet(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	publishURL, err := requireCfg(cfg, "pingletPublishUrl", "pingletPublishUrl")
	if err != nil {
		return "", err
	}
	apiKey, err := requireCfg(cfg, "pingletApiKey", "pingletApiKey")
	if err != nil {
		return "", err
	}

	base := strings.TrimSuffix(publishURL, "/")

	// 官方用 axios params 追加 rewrite，已有 query 时会用 & 拼接
	target := base
	if u, err := url.Parse(base); err == nil {
		q := u.Query()
		q.Set("rewrite", "uptimekuma")
		u.RawQuery = q.Encode()
		target = u.String()
	} else {
		target = base + "?rewrite=uptimekuma"
	}

	payload := map[string]any{
		"heartbeat": g5HeartbeatMap(m),
		"monitor":   g5MonitorMap(m),
		"msg":       m.Msg,
	}

	return postJSONOK(ctx, "pinglet", target, payload, map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
}

// ---------------- Pushy ----------------

func sendG5Pushy(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "pushyAPIKey", "pushyAPIKey")
	if err != nil {
		return "", err
	}
	token, err := requireCfg(cfg, "pushyToken", "pushyToken")
	if err != nil {
		return "", err
	}

	target := "https://api.pushy.me/push?api_key=" + g5EncodeComponent(apiKey)
	payload := map[string]any{
		"to": token,
		"data": map[string]any{
			"message": "Uptime-Kuma",
		},
		"notification": map[string]any{
			"body":  m.Msg,
			"badge": 1,
			"sound": "ping.aiff",
		},
	}

	return postJSONOK(ctx, "pushy", target, payload, nil)
}

// ---------------- SMSManager ----------------

func sendG5SMSManager(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "smsmanagerApiKey", "smsmanagerApiKey")
	if err != nil {
		return "", err
	}
	numbers, err := requireCfg(cfg, "numbers", "numbers")
	if err != nil {
		return "", err
	}

	q := url.Values{}
	q.Set("apikey", apiKey)
	q.Set("message", g5StripNonASCII(m.Msg))
	q.Set("number", numbers)
	q.Set("gateway", cfgStr(cfg, "messageType"))

	target := "https://http-api.smsmanager.cz/Send?" + q.Encode()
	return getOK(ctx, "SMSManager", target, nil)
}

// ---------------- Teltonika ----------------

func sendG5Teltonika(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	rawURL := strings.TrimSpace(cfgStr(cfg, "teltonikaUrl"))
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("Invalid URL: %s", rawURL)
	}
	base := parsed.Scheme + "://" + parsed.Host
	loginURL := base + "/api/login"
	smsURL := base + "/api/messages/actions/send"

	username, err := requireCfg(cfg, "teltonikaUsername", "teltonikaUsername")
	if err != nil {
		return "", err
	}
	password, err := requireCfg(cfg, "teltonikaPassword", "teltonikaPassword")
	if err != nil {
		return "", err
	}
	modem, err := requireCfg(cfg, "teltonikaModem", "teltonikaModem")
	if err != nil {
		return "", err
	}
	phone, err := requireCfg(cfg, "teltonikaPhoneNumber", "teltonikaPhoneNumber")
	if err != nil {
		return "", err
	}

	// 网关限制单条 160 字符，官方截到 159
	runes := []rune(m.Msg)
	if len(runes) > 159 {
		runes = runes[:159]
	}
	cleanMsg := string(runes)

	client := &http.Client{Timeout: 20 * time.Second}
	if cfgBool(cfg, "teltonikaUnsafeTls") {
		client = &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 官方同样提供该开关
			},
		}
	}

	headers := map[string]string{
		"Content-Type":  "application/json",
		"cache-control": "no-cache",
		"Accept":        "application/json",
	}

	// 令牌 5 分钟过期，所以每次都重新登录
	code, body, err := g5PostJSONWith(ctx, client, loginURL, map[string]any{
		"username": username,
		"password": password,
	}, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("Teltonika", code, body); err != nil {
		return "", err
	}

	var loginResp struct {
		Success bool `json:"success"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
		Errors struct {
			Error any `json:"error"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &loginResp); err != nil {
		return "", fmt.Errorf("Teltonika: 无法解析登录响应: %s", g5Snippet(body))
	}
	if !loginResp.Success {
		return "", fmt.Errorf("Login failed: %v", loginResp.Errors.Error)
	}

	smsHeaders := map[string]string{
		"Content-Type":  "application/json",
		"cache-control": "no-cache",
		"Accept":        "application/json",
		"Authorization": "Bearer " + loginResp.Data.Token,
	}

	code, body, err = g5PostJSONWith(ctx, client, smsURL, map[string]any{
		"data": map[string]any{
			"modem":   modem,
			"number":  phone,
			"message": cleanMsg,
		},
	}, smsHeaders)
	if err != nil {
		return "", err
	}
	if err := expectOK("Teltonika", code, body); err != nil {
		return "", err
	}

	var smsResp struct {
		Success bool `json:"success"`
		Errors  struct {
			Error any `json:"error"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &smsResp); err != nil {
		return "", fmt.Errorf("Teltonika: 无法解析响应: %s", g5Snippet(body))
	}
	if !smsResp.Success {
		return "", fmt.Errorf("Api returned: %v", smsResp.Errors.Error)
	}

	return "Sent Successfully.", nil
}

// g5PostJSONWith 用指定 client 发 JSON（Teltonika 需要自定义 TLS 配置）。
func g5PostJSONWith(ctx context.Context, client *http.Client, target string, payload any, headers map[string]string) (int, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Uptime-Kuma-Go")
	}

	res, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()

	respRaw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	return res.StatusCode, string(respRaw), nil
}

// ---------------- GoAlert ----------------

func sendG5GoAlert(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	baseURL, err := requireCfg(cfg, "goAlertBaseURL", "goAlertBaseURL")
	if err != nil {
		return "", err
	}
	token, err := requireCfg(cfg, "goAlertToken", "goAlertToken")
	if err != nil {
		return "", err
	}

	target := joinURL(baseURL, "api/v2/generic/incoming") + "?token=" + g5EncodeComponent(token)

	// 官方用 Content-Type: multipart/form-data 提交 {summary, action}，这里按表单字段发送
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("summary", m.Msg)
	if m.Monitor != nil && m.Beat.Status == model.StatusUp {
		_ = w.WriteField("action", "close")
	}
	_ = w.Close()

	code, body, err := httpDoRaw(ctx, http.MethodPost, target, w.FormDataContentType(), buf.Bytes(), nil)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		detail := strings.TrimSpace(body)
		if detail == "" {
			detail = "Error without response"
		}
		return "", fmt.Errorf("%s", detail)
	}

	return "Sent Successfully.", nil
}

// ---------------- Notifery ----------------

func sendG5Notifery(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "notiferyApiKey", "notiferyApiKey")
	if err != nil {
		return "", err
	}

	title := strings.TrimSpace(cfgStr(cfg, "notiferyTitle"))
	if title == "" {
		title = "Uptime Kuma Alert"
	}

	message := m.Msg
	payload := map[string]any{
		"title":   title,
		"message": message,
	}
	if group := strings.TrimSpace(cfgStr(cfg, "notiferyGroup")); group != "" {
		payload["group"] = group
	}

	// 注：官方会读取 setting("primaryBaseURL") 并追加监控详情链接；
	// notify 包无法访问设置仓库，这里省略该行（告警本身不受影响）。
	if m.Monitor != nil {
		if m.Beat.Status == model.StatusUp {
			payload["code"] = 0
		} else {
			payload["code"] = 1
		}
		if m.Beat.Ping != nil && *m.Beat.Ping != 0 {
			payload["duration"] = *m.Beat.Ping
		}
	}

	return postJSONOK(ctx, "notifery", "https://api.notifery.com/event", payload, map[string]string{
		"Content-Type": "application/json",
		"x-api-key":    apiKey,
	})
}

// ---------------- SendGrid ----------------

func sendG5SendGrid(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "sendgridApiKey", "sendgridApiKey")
	if err != nil {
		return "", err
	}
	fromEmail, err := requireCfg(cfg, "sendgridFromEmail", "sendgridFromEmail")
	if err != nil {
		return "", err
	}
	toEmail, err := requireCfg(cfg, "sendgridToEmail", "sendgridToEmail")
	if err != nil {
		return "", err
	}

	personalization := map[string]any{
		"to": []map[string]any{{"email": toEmail}},
	}

	if cc := strings.TrimSpace(cfgStr(cfg, "sendgridCcEmail")); cc != "" {
		personalization["cc"] = g5EmailList(cc)
	}
	if bcc := strings.TrimSpace(cfgStr(cfg, "sendgridBccEmail")); bcc != "" {
		personalization["bcc"] = g5EmailList(bcc)
	}

	subject := strings.TrimSpace(cfgStr(cfg, "sendgridSubject"))
	if subject == "" {
		subject = "Notification from Your Uptime Kuma"
	}

	payload := map[string]any{
		"personalizations": []map[string]any{personalization},
		"from":             map[string]any{"email": strings.TrimSpace(fromEmail)},
		"subject":          subject,
		"content": []map[string]any{
			{"type": "text/plain", "value": m.Msg},
		},
	}

	return postJSONOK(ctx, "SendGrid", "https://api.sendgrid.com/v3/mail/send", payload,
		map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + apiKey,
		})
}

// g5EmailList 把逗号分隔的收件人拆成 SendGrid 需要的 [{email}] 形式。
func g5EmailList(raw string) []map[string]any {
	out := make([]map[string]any, 0, 4)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, map[string]any{"email": p})
		}
	}
	return out
}

// ---------------- HaloPSA ----------------

func sendG5HaloPSA(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	webhook, err := requireCfg(cfg, "halowebhookurl", "halowebhookurl")
	if err != nil {
		return "", err
	}

	status := "UNKNOWN"
	if m.Monitor != nil {
		switch m.Beat.Status {
		case model.StatusUp:
			status = "UP"
		case model.StatusDown:
			status = "DOWN"
		}
	} else {
		// 没有监控对象（测试消息）但有心跳上下文时的分支
		status = "NOTIFICATION"
	}

	var monitorName any = "No Monitor"
	var monitorID any
	if m.Monitor != nil {
		if m.Monitor.Name != "" {
			monitorName = m.Monitor.Name
		}
		monitorID = m.Monitor.ID
	}

	payload := map[string]any{
		"title":               "Uptime Kuma Alert",
		"status":              status,
		"monitor":             monitorName,
		"monitor_id":          monitorID,
		"message":             m.Msg,
		"timestamp":           g5ISO(),
		"uptime_kuma_version": "unknown",
	}

	headers := map[string]string{"Content-Type": "application/json"}
	user := strings.TrimSpace(cfgStr(cfg, "haloUsername"))
	pass := cfgStr(cfg, "haloPassword")
	if user != "" && pass != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}

	code, _, err := postJSONBody(ctx, webhook, payload, headers)
	if err != nil {
		return "", err
	}
	if code == 200 || code == 201 || code == 204 {
		// 官方此处文案是小写 s，与其它渠道不同
		return "Sent successfully.", nil
	}
	return "", fmt.Errorf("Received unexpected status code %d from notification provider HaloPSA", code)
}
