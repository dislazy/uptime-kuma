package notify

// 本文件移植官方 15 个通知渠道：
// alerta / ClickUp / fluxer / GrafanaOncall / line / OneChat / JiraServiceManagement /
// promosms / signal / squadcast / threema / SevenIO / Whatsapp360messenger / Brevo / VK
//
// 所有包级标识符都带 g1 前缀（渠道函数带渠道名），避免与同包其它文件重名。
// 请求地址、方法、字段名、URL 编码、必填校验与成功/失败文案均按官方 JS 忠实还原。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

func init() {
	registerFunc("alerta", sendAlerta)
	registerFunc("ClickUp", sendClickUp)
	registerFunc("fluxer", sendFluxer)
	registerFunc("GrafanaOncall", sendGrafanaOncall)
	registerFunc("line", sendLine)
	registerFunc("OneChat", sendOneChat)
	registerFunc("JiraServiceManagement", sendJiraServiceManagement)
	registerFunc("promosms", sendPromoSMS)
	registerFunc("signal", sendSignal)
	registerFunc("squadcast", sendSquadcast)
	registerFunc("threema", sendThreema)
	registerFunc("SevenIO", sendSevenIO)
	registerFunc("Whatsapp360messenger", sendWhatsapp360Messenger)
	registerFunc("Brevo", sendBrevo)
	registerFunc("VK", sendVK)
}

// ---------------- 公共助手（仅本文件使用，g1 前缀） ----------------

// g1BeatMsg 对应官方 heartbeatJSON["msg"]，空值兜底 N/A。
func g1BeatMsg(m Message) string {
	if m.Beat.Msg == "" {
		return "N/A"
	}
	return m.Beat.Msg
}

// g1ParseBeatTime 解析心跳时间，兼容官方 SQL 格式与 RFC3339。
func g1ParseBeatTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05.000", "2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// g1TimezoneName 对应官方 heartbeatJSON["timezone"]。
// 官方取的是服务端设置的时区；这里没有 settings 上下文，用本机时区尽力还原。
func g1TimezoneName() string {
	name := time.Local.String()
	if name == "" || name == "Local" {
		if z, _ := time.Now().Zone(); z != "" {
			return z
		}
		return "UTC"
	}
	return name
}

// g1LocalDateTime 对应官方 heartbeatJSON["localDateTime"]（SQL_DATETIME_FORMAT）。
func g1LocalDateTime(m Message) string {
	if t, ok := g1ParseBeatTime(m.Beat.Time); ok {
		return t.Local().Format("2006-01-02 15:04:05")
	}
	return m.Beat.Time
}

// g1BeatUnix 返回心跳时间的 Unix 秒，解析失败返回 0。
func g1BeatUnix(m Message) int64 {
	if t, ok := g1ParseBeatTime(m.Beat.Time); ok {
		return t.Unix()
	}
	return 0
}

// g1ExtractAddress 复刻官方 notification-provider.js 的 extractAddress。
func g1ExtractAddress(m Message) string {
	mp := m.Monitor
	if mp == nil {
		return ""
	}
	switch mp.Type {
	case "push":
		return "Heartbeat"
	case "ping", "dns":
		return mp.Hostname()
	case "port", "gamedig", "steam":
		if p := mp.Port(); p != 0 {
			return mp.Hostname() + ":" + strconv.Itoa(p)
		}
		return mp.Hostname()
	case "globalping":
		switch mp.Get("subtype") {
		case "ping", "dns":
			return mp.Hostname()
		case "http":
			return mp.URL()
		default:
			return ""
		}
	default:
		if u := mp.URL(); u != "https://" && u != "http://" && u != "" {
			return u
		}
		return ""
	}
}

// g1StatusText 复刻官方 renderTemplate 里的 status 变量。
func g1StatusText(m Message) string {
	if m.Monitor == nil {
		return "⚠️ Test"
	}
	if m.Beat.Status == model.StatusDown {
		return "🔴 Down"
	}
	return "✅ Up"
}

// g1BeatMap 把 Beat 还原成官方 heartbeatJSON 的形状（含 timezone/localDateTime）。
func g1BeatMap(m Message) map[string]any {
	out := map[string]any{
		"monitorID":     m.Beat.MonitorID,
		"status":        m.Beat.Status,
		"msg":           g1BeatMsg(m),
		"time":          m.Beat.Time,
		"duration":      m.Beat.Duration,
		"important":     m.Beat.Important,
		"timezone":      g1TimezoneName(),
		"localDateTime": g1LocalDateTime(m),
	}
	if m.Beat.Ping != nil {
		out["ping"] = *m.Beat.Ping
	}
	return out
}

var (
	g1TplVarRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)
	g1TplIfRe  = regexp.MustCompile(`(?s)\{%\s*if\s+([A-Za-z_][A-Za-z0-9_]*)\s*%\}(.*?)\{%\s*endif\s*%\}`)
)

// g1RenderTemplate 尽力还原官方 Liquid 模板的常用子集：
// {{ msg }} / {{ name }} / {{ status }} / {{ hostnameOrURL }} /
// {{ monitorJSON.name }} / {{ monitorJSON['url'] }} / {{ heartbeatJSON.msg }}，
// 以及 {% if monitorJSON %}...{% endif %} 条件块。
func g1RenderTemplate(tpl string, m Message) string {
	out := g1TplIfRe.ReplaceAllStringFunc(tpl, func(block string) string {
		sub := g1TplIfRe.FindStringSubmatch(block)
		if sub == nil {
			return block
		}
		if g1TplTruthy(sub[1], m) {
			return sub[2]
		}
		return ""
	})
	return g1TplVarRe.ReplaceAllStringFunc(out, func(token string) string {
		sub := g1TplVarRe.FindStringSubmatch(token)
		if sub == nil {
			return token
		}
		return g1TplValue(sub[1], m)
	})
}

func g1TplTruthy(expr string, m Message) bool {
	switch expr {
	case "msg":
		return m.Msg != ""
	case "monitorJSON", "heartbeatJSON":
		return m.Monitor != nil
	case "name":
		return m.MonitorName() != ""
	default:
		return false
	}
}

func g1TplValue(expr string, m Message) string {
	e := strings.TrimSpace(expr)
	// 去掉 Liquid 过滤器，如 {{ name | escape }}
	if i := strings.Index(e, "|"); i >= 0 {
		e = strings.TrimSpace(e[:i])
	}
	switch e {
	case "msg":
		return m.Msg
	case "name", "NAME":
		if m.MonitorName() != "" {
			return m.MonitorName()
		}
		return "Monitor Name not available"
	case "status", "STATUS":
		return g1StatusText(m)
	case "hostnameOrURL", "HOSTNAME_OR_URL":
		if addr := g1ExtractAddress(m); addr != "" {
			return addr
		}
		if m.Monitor != nil {
			return ""
		}
		return "testing.hostname"
	}

	obj, key, ok := g1TplField(expr)
	if !ok {
		return ""
	}
	switch obj {
	case "monitorJSON":
		mp := m.Monitor
		if mp == nil {
			return ""
		}
		switch key {
		case "name":
			return mp.Name
		case "url":
			return mp.URL()
		case "type":
			return mp.Type
		case "hostname":
			return mp.Hostname()
		case "port":
			return strconv.Itoa(mp.Port())
		}
		return mp.Get(key)
	case "heartbeatJSON":
		if m.Monitor == nil {
			return ""
		}
		switch key {
		case "msg":
			return g1BeatMsg(m)
		case "status":
			return strconv.Itoa(m.Beat.Status)
		case "time":
			return m.Beat.Time
		case "monitorID":
			return strconv.FormatInt(m.Beat.MonitorID, 10)
		case "timezone":
			return g1TimezoneName()
		case "localDateTime":
			return g1LocalDateTime(m)
		}
		return ""
	}
	return ""
}

// g1TplField 解析 monitorJSON.name / monitorJSON['name'] / monitorJSON["name"] 形式。
func g1TplField(expr string) (obj, key string, ok bool) {
	dot := strings.Index(expr, ".")
	if dot < 0 {
		return "", "", false
	}
	obj = strings.TrimSpace(expr[:dot])
	rest := strings.TrimSpace(expr[dot+1:])
	if strings.HasPrefix(rest, "[") {
		rest = strings.TrimSuffix(rest, "]")
		rest = strings.Trim(rest, `'"`)
	} else {
		rest = strings.Trim(rest, `'"`)
	}
	if rest == "" {
		return "", "", false
	}
	return obj, rest, true
}

// g1StripNonASCII 复刻 official promosms 的 msg.replace(/[^\x00-\x7F]/g, "")。
func g1StripNonASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r <= 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// g1StripWhitespace 复刻 signal 的 recipients.replace(/\s/g, "")。
func g1StripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f', 0x85, 0xA0:
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// g1MergeMaps 对应 JS 的 Object.assign(base, over)：over 覆盖 base。
func g1MergeMaps(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// ---------------- Alerta ----------------

func sendAlerta(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	headers := map[string]string{
		"Content-Type":  "application/json;charset=UTF-8",
		"Authorization": "Key " + cfgStr(cfg, "alertaApiKey"),
	}
	target := cfgStr(cfg, "alertaApiEndpoint")

	// 官方用 Object.assign({...}, data) 合并，data 里的同名字段会覆盖前者
	base := map[string]any{
		"environment": cfgStr(cfg, "alertaEnvironment"),
		"severity":    "critical",
		"correlate":   []any{},
		"service":     []string{"UptimeKuma"},
		"value":       "Timeout",
		"tags":        []string{"uptimekuma"},
		"attributes":  map[string]any{},
		"origin":      "uptimekuma",
		"type":        "exceptionAlert",
	}

	if m.Monitor == nil {
		payload := g1MergeMaps(base, map[string]any{
			"event":    "msg",
			"text":     m.Msg,
			"group":    "uptimekuma-msg",
			"resource": "Message",
		})
		return postJSONOK(ctx, "Alerta", target, payload, headers)
	}

	mp := m.Monitor
	payload := g1MergeMaps(base, map[string]any{
		// 注意：官方 data.correlate 是空数组，会覆盖前面写的 correlate
		"correlate": []any{},
		"event":     mp.Type,
		"group":     "uptimekuma-" + mp.Type,
		"resource":  mp.Name,
	})

	switch m.Status() {
	case model.StatusDown:
		payload["severity"] = cfgStr(cfg, "alertaAlertState")
		payload["text"] = "Service " + mp.Type + " is down."
		return postJSONOK(ctx, "Alerta", target, payload, headers)
	case model.StatusUp:
		payload["severity"] = cfgStr(cfg, "alertaRecoverState")
		payload["text"] = "Service " + mp.Type + " is up."
		return postJSONOK(ctx, "Alerta", target, payload, headers)
	default:
		// 官方在其它状态下不发送，直接返回成功文案
		return "Sent Successfully.", nil
	}
}

// ---------------- ClickUp ----------------

func sendClickUp(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	token := cfgStr(cfg, "clickupToken")
	workspaceID := cfgStr(cfg, "clickupWorkspaceId")
	channelID := cfgStr(cfg, "clickupChannelId")

	content := m.Msg
	if m.Monitor != nil {
		content += fmt.Sprintf("\n\n**Time (%s):** %s", g1TimezoneName(), g1LocalDateTime(m))
	}
	if addr := g1ExtractAddress(m); addr != "" && !cfgBool(cfg, "clickupDisableUrl") {
		content += "\n**Address:** " + addr
	}

	payload := map[string]any{
		"type":           "message",
		"content":        content,
		"content_format": "text/md",
	}
	target := fmt.Sprintf("https://api.clickup.com/api/v3/workspaces/%s/chat/channels/%s/messages", workspaceID, channelID)

	return postJSONOK(ctx, "ClickUp", target, payload, map[string]string{"Authorization": token})
}

// ---------------- Fluxer ----------------

const fluxerDefaultAvatar = "https://github.com/louislam/uptime-kuma/raw/master/public/icon.png"

func sendFluxer(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	displayName := cfgStr(cfg, "fluxerUsername")
	if displayName == "" {
		displayName = "Uptime Kuma"
	}

	webhookURL := cfgStr(cfg, "fluxerWebhookUrl")
	if webhookURL == "" {
		return "", fmt.Errorf("Invalid URL: %q", webhookURL)
	}
	if _, err := url.Parse(webhookURL); err != nil {
		return "", err
	}

	// 先探测 webhook 是否已有头像；失败时按官方逻辑假设"有"，避免强行覆盖默认头像
	hasAvatar := true
	if code, body, err := getURL(ctx, webhookURL, nil); err == nil && code >= 200 && code < 300 {
		var info struct {
			Avatar string `json:"avatar"`
		}
		if json.Unmarshal([]byte(body), &info) == nil {
			hasAvatar = info.Avatar != ""
		}
	}

	format := cfgStr(cfg, "fluxerMessageFormat")
	if format == "" {
		if cfgBool(cfg, "fluxerUseMessageTemplate") {
			format = "custom"
		} else {
			format = "normal"
		}
	}

	send := func(payload map[string]any) (string, error) {
		if !hasAvatar {
			payload["avatar_url"] = fluxerDefaultAvatar
		}
		return postJSONOK(ctx, "Fluxer", webhookURL, payload, nil)
	}

	// 测试消息：heartbeatJSON 为 null
	if m.Monitor == nil {
		content := m.Msg
		switch format {
		case "minimalist":
			content = "Test: " + m.Msg
		case "custom":
			if tpl := strings.TrimSpace(cfgStr(cfg, "fluxerMessageTemplate")); tpl != "" {
				content = g1RenderTemplate(tpl, m)
			}
		}
		return send(map[string]any{"username": displayName, "content": content})
	}

	mp := m.Monitor
	addr := g1ExtractAddress(m)
	disableURL := cfgBool(cfg, "disableUrl")

	if format == "minimalist" {
		content := "🟢 " + mp.Name + " is up."
		if m.Status() == model.StatusDown {
			content = "🔴 " + mp.Name + " is down."
		}
		return send(map[string]any{"username": displayName, "content": content})
	}

	if format == "custom" {
		if tpl := strings.TrimSpace(cfgStr(cfg, "fluxerMessageTemplate")); tpl != "" {
			return send(map[string]any{"username": displayName, "content": g1RenderTemplate(tpl, m)})
		}
	}

	switch m.Status() {
	case model.StatusDown:
		fields := []any{
			map[string]any{"name": "Service Name", "value": mp.Name},
		}
		if addr != "" && !disableURL {
			fieldName := "Service URL"
			if mp.Type == "push" {
				fieldName = "Service Type"
			}
			fields = append(fields, map[string]any{"name": fieldName, "value": addr})
		}
		if ts := g1BeatUnix(m); ts > 0 {
			fields = append(fields, map[string]any{"name": "Went Offline", "value": fmt.Sprintf("<t:%d:F>", ts)})
		}
		fields = append(fields,
			map[string]any{"name": "Time (" + g1TimezoneName() + ")", "value": g1LocalDateTime(m)},
			map[string]any{"name": "Error", "value": g1BeatMsg(m)},
		)

		payload := map[string]any{
			"username": displayName,
			"embeds": []any{
				map[string]any{
					"title":  "❌ Your service " + mp.Name + " went down. ❌",
					"color":  16711680,
					"fields": fields,
				},
			},
		}
		if prefix := cfgStr(cfg, "fluxerPrefixMessage"); prefix != "" {
			payload["content"] = prefix
		}
		return send(payload)

	case model.StatusUp:
		fields := []any{
			map[string]any{"name": "Service Name", "value": mp.Name},
		}
		if addr != "" && !disableURL {
			fieldName := "Service URL"
			if mp.Type == "push" {
				fieldName = "Service Type"
			}
			fields = append(fields, map[string]any{"name": fieldName, "value": addr})
		}
		// heartbeatJSON.lastDownTime 不在 Go 侧 Message 里，官方也是可选展示，这里省略
		fields = append(fields, map[string]any{"name": "Time (" + g1TimezoneName() + ")", "value": g1LocalDateTime(m)})
		if m.Beat.Ping != nil {
			fields = append(fields, map[string]any{"name": "Ping", "value": fmt.Sprintf("%d ms", *m.Beat.Ping)})
		}

		payload := map[string]any{
			"username": displayName,
			"embeds": []any{
				map[string]any{
					"title":  "✅ Your service " + mp.Name + " is up! ✅",
					"color":  65280,
					"fields": fields,
				},
			},
		}
		if prefix := cfgStr(cfg, "fluxerPrefixMessage"); prefix != "" {
			payload["content"] = prefix
		}
		return send(payload)
	}

	// 状态既不是 UP 也不是 DOWN：官方不发送
	return "", nil
}

// ---------------- Grafana OnCall ----------------

func sendGrafanaOncall(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := cfgStr(cfg, "GrafanaOncallURL")
	if target == "" {
		return "", fmt.Errorf("GrafanaOncallURL cannot be empty")
	}

	var payload map[string]any
	switch {
	case m.Monitor == nil:
		payload = map[string]any{
			"title":   "General notification",
			"message": m.Msg,
			"state":   "alerting",
		}
	case m.Status() == model.StatusDown:
		payload = map[string]any{
			"title":   m.Monitor.Name + " is down",
			"message": g1BeatMsg(m),
			"state":   "alerting",
		}
	case m.Status() == model.StatusUp:
		payload = map[string]any{
			"title":   m.Monitor.Name + " is up",
			"message": g1BeatMsg(m),
			"state":   "ok",
		}
	default:
		return "", nil
	}

	return postJSONOK(ctx, "GrafanaOncall", target, payload, nil)
}

// ---------------- LINE ----------------

const linePushURL = "https://api.line.me/v2/bot/message/push"

func sendLine(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + cfgStr(cfg, "lineChannelAccessToken"),
	}
	to := cfgStr(cfg, "lineUserID")

	if m.Monitor == nil {
		payload := map[string]any{
			"to": to,
			"messages": []any{
				map[string]any{"type": "text", "text": "Test Successful!"},
			},
		}
		return postJSONOK(ctx, "LINE", linePushURL, payload, headers)
	}

	// 官方只在 UP/DOWN 时发送，其它状态直接返回成功文案
	var status string
	switch m.Status() {
	case model.StatusDown:
		status = "[🔴 Down]"
	case model.StatusUp:
		status = "[✅ Up]"
	default:
		return "Sent Successfully.", nil
	}

	text := "UptimeKuma Alert: " + status + "\n" +
		"Name: " + m.Monitor.Name + " \n" +
		g1BeatMsg(m) +
		fmt.Sprintf("\nTime (%s): %s", g1TimezoneName(), g1LocalDateTime(m))

	payload := map[string]any{
		"to": to,
		"messages": []any{
			map[string]any{"type": "text", "text": text},
		},
	}
	return postJSONOK(ctx, "LINE", linePushURL, payload, headers)
}

// ---------------- OneChat ----------------

const oneChatPushURL = "https://chat-api.one.th/message/api/v1/push_message"

func sendOneChat(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + cfgStr(cfg, "accessToken"),
	}

	var text string
	if m.Monitor == nil {
		text = "Test Successful!"
	} else {
		switch m.Status() {
		case model.StatusDown:
			text = fmt.Sprintf("UptimeKuma Alert:\n[🔴 Down]\nName: %s\n%s\nTime (%s): %s",
				m.Monitor.Name, g1BeatMsg(m), g1TimezoneName(), g1LocalDateTime(m))
		case model.StatusUp:
			text = fmt.Sprintf("UptimeKuma Alert:\n[🟢 Up]\nName: %s\n%s\nTime (%s): %s",
				m.Monitor.Name, g1BeatMsg(m), g1TimezoneName(), g1LocalDateTime(m))
		default:
			// 官方只在 UP/DOWN 时发送
			return "Sent Successfully.", nil
		}
	}

	payload := map[string]any{
		"to":      cfgStr(cfg, "recieverId"),
		"bot_id":  cfgStr(cfg, "botId"),
		"type":    "text",
		"message": text,
	}

	code, body, err := postJSONBody(ctx, oneChatPushURL, payload, headers)
	if err != nil {
		return "", err
	}
	if code >= 200 && code < 300 {
		return "Sent Successfully.", nil
	}

	errMsg := "Unknown API error occurred."
	var res struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &res) == nil && res.Message != "" {
		errMsg = res.Message
	}
	return "", fmt.Errorf("OneChat API Error: %s", errMsg)
}

// ---------------- Jira Service Management ----------------

func sendJiraServiceManagement(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	baseURL := fmt.Sprintf("https://api.atlassian.com/jsm/ops/api/%s/v1", cfgStr(cfg, "jsmCloudId"))
	const textMsg = "Uptime Kuma Alert"

	authToken := base64.StdEncoding.EncodeToString([]byte(cfgStr(cfg, "jsmEmail") + ":" + cfgStr(cfg, "jsmApiToken")))
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"Authorization": "Basic " + authToken,
	}

	priority := cfgStr(cfg, "jsmPriority")
	if priority == "" {
		priority = "3"
	}

	// 测试通知
	if m.Monitor == nil {
		payload := map[string]any{
			"message":  m.Msg,
			"alias":    "uptime-kuma-notification-test",
			"source":   "Uptime Kuma",
			"priority": "P5",
			"tags":     []string{"Uptime Kuma"},
		}
		return postJSONOK(ctx, "Jira Service Management", baseURL+"/alerts", payload, headers)
	}

	mp := m.Monitor

	if m.Status() == model.StatusDown {
		payload := map[string]any{
			"message":     textMsg + ": " + mp.Name,
			"alias":       mp.Name,
			"description": m.Msg,
			"source":      "Uptime Kuma",
			"priority":    "P" + priority,
			"tags":        []string{"Uptime Kuma"},
		}
		return postJSONOK(ctx, "Jira Service Management", baseURL+"/alerts", payload, headers)
	}

	if m.Status() == model.StatusUp {
		// JSM 要求先按 alias 查到 alert id，再按 id 关闭
		getURLTarget := baseURL + "/alerts/alias?alias=" + url.QueryEscape(mp.Name)
		code, body, err := getURL(ctx, getURLTarget, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("Jira Service Management", code, body); err != nil {
			return "", err
		}
		var res struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			return "", fmt.Errorf("Jira Service Management notification failed with invalid response!")
		}
		alertID := strings.Trim(string(res.ID), `"`)
		if alertID == "" || alertID == "null" {
			return "", fmt.Errorf("Jira Service Management alert not found for alias %q", mp.Name)
		}

		payload := map[string]any{"source": "Uptime Kuma"}
		closeURL := fmt.Sprintf("%s/alerts/%s/close", baseURL, alertID)
		return postJSONOK(ctx, "Jira Service Management", closeURL, payload, headers)
	}

	return "", nil
}

// ---------------- PromoSMS ----------------

const promoSMSURL = "https://promosms.com/api/rest/v3_2/sms"

func sendPromoSMS(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	allowLongSMS := cfgBool(cfg, "promosmsAllowLongSMS")

	cleanMsg := g1StripNonASCII(m.Msg)
	limit := 159
	if allowLongSMS {
		limit = 639
	}
	if len(cleanMsg) > limit {
		cleanMsg = cleanMsg[:limit]
	}

	auth := base64.StdEncoding.EncodeToString([]byte(cfgStr(cfg, "promosmsLogin") + ":" + cfgStr(cfg, "promosmsPassword")))
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Basic " + auth,
		"Accept":        "text/json",
	}

	payload := map[string]any{
		"recipients": []string{cfgStr(cfg, "promosmsPhoneNumber")},
		"text":       cleanMsg,
		"long-sms":   allowLongSMS,
		"type":       cfgInt(cfg, "promosmsSMSType"),
		"sender":     cfgStr(cfg, "promosmsSenderName"),
	}

	code, body, err := postJSONBody(ctx, promoSMSURL, payload, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("PromoSMS", code, body); err != nil {
		return "", err
	}

	var res map[string]any
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", fmt.Errorf("PromoSMS: %v", err)
	}
	response, _ := res["response"].(map[string]any)
	status := response["status"]
	failed := false
	failedStatus := ""
	switch v := status.(type) {
	case nil:
		// 官方此处会抛 TypeError，这里给出等价的可读错误
		return "", fmt.Errorf("Something gone wrong. Api returned no status.")
	case float64:
		failed = v != 0
		failedStatus = strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		failed = v != "" && v != "0"
		failedStatus = v
	default:
		failed = true
		failedStatus = fmt.Sprintf("%v", v)
	}
	if failed {
		return "", fmt.Errorf("Something gone wrong. Api returned %s.", failedStatus)
	}

	return "Sent Successfully.", nil
}

// ---------------- Signal ----------------

func sendSignal(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	message := m.Msg
	if cfgBool(cfg, "signalUseTemplate") {
		message = g1RenderTemplate(cfgStr(cfg, "signalTemplate"), m)
	}

	payload := map[string]any{
		"message": message,
		"number":  cfgStr(cfg, "signalNumber"),
		// 官方：signalRecipients.replace(/\s/g, "").split(",")
		"recipients": strings.Split(g1StripWhitespace(cfgStr(cfg, "signalRecipients")), ","),
	}

	target := cfgStr(cfg, "signalURL")
	return postJSONOK(ctx, "Signal", target, payload, nil)
}

// ---------------- Squadcast ----------------

func sendSquadcast(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	tags := map[string]any{}

	payload := map[string]any{
		"message":     m.Msg,
		"description": "",
		"tags":        tags,
		"heartbeat":   nil,
		"source":      "uptime-kuma",
	}

	if m.Monitor != nil {
		mp := m.Monitor
		payload["heartbeat"] = g1BeatMap(m)
		payload["description"] = g1BeatMsg(m)
		payload["event_id"] = m.Beat.MonitorID
		if m.Status() == model.StatusDown {
			payload["message"] = mp.Name + " is DOWN"
			payload["status"] = "trigger"
		} else {
			payload["message"] = mp.Name + " is UP"
			payload["status"] = "resolve"
		}

		tags["AlertAddress"] = g1ExtractAddress(m)
		for _, tag := range mp.Tags {
			entry := map[string]any{"value": tag.Value}
			if tag.Color != "" {
				entry["color"] = tag.Color
			}
			tags[tag.Name] = entry
		}
	}

	target := cfgStr(cfg, "squadcastWebhookURL")
	return postJSONOK(ctx, "Squadcast", target, payload, nil)
}

// ---------------- Threema ----------------

const threemaSendURL = "https://msgapi.threema.ch/send_simple"

func sendThreema(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	form := url.Values{}
	form.Set("from", cfgStr(cfg, "threemaSenderIdentity"))
	form.Set("secret", cfgStr(cfg, "threemaSecret"))
	form.Set("text", m.Msg)

	recipient := cfgStr(cfg, "threemaRecipient")
	switch cfgStr(cfg, "threemaRecipientType") {
	case "identity":
		form.Set("to", recipient)
	case "phone":
		form.Set("phone", recipient)
	case "email":
		form.Set("email", recipient)
	default:
		return "", fmt.Errorf("Unsupported recipient type: %s", cfgStr(cfg, "threemaRecipientType"))
	}

	headers := map[string]string{
		"Accept":       "*/*",
		"Content-Type": "application/x-www-form-urlencoded; charset=utf-8",
	}

	code, body, err := postFormValues(ctx, threemaSendURL, form, headers)
	if err != nil {
		return "", err
	}
	if code >= 200 && code < 300 {
		return "Threema notification sent successfully.", nil
	}

	switch code {
	case 400:
		return "", fmt.Errorf("Invalid recipient identity or account not set up for basic mode (400).")
	case 401:
		return "", fmt.Errorf("Incorrect API identity or secret (401).")
	case 402:
		return "", fmt.Errorf("No credits remaining (402).")
	case 404:
		return "", fmt.Errorf("Recipient not found (404).")
	case 413:
		return "", fmt.Errorf("Message is too long (413).")
	case 500:
		return "", fmt.Errorf("Temporary internal server error (500).")
	default:
		return "", expectOK("Threema", code, body)
	}
}

// ---------------- SevenIO ----------------

const sevenIOSMSURL = "https://gateway.seven.io/api/sms"

func sendSevenIO(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	sender := cfgStr(cfg, "sevenioSender")
	if sender == "" {
		sender = "Uptime Kuma"
	}
	text := m.Msg

	headers := map[string]string{
		"Content-Type": "application/json",
		"X-API-Key":    cfgStr(cfg, "sevenioApiKey"),
	}

	if m.Monitor != nil {
		address := g1ExtractAddress(m)
		if address != "" {
			address = "(" + address + ") "
		}
		switch m.Status() {
		case model.StatusDown:
			text = fmt.Sprintf("Your service %s %swent down at %s (%s). Error: %s",
				m.Monitor.Name, address, g1LocalDateTime(m), g1TimezoneName(), g1BeatMsg(m))
		case model.StatusUp:
			text = fmt.Sprintf("Your service %s %swent back up at %s (%s).",
				m.Monitor.Name, address, g1LocalDateTime(m), g1TimezoneName())
		}
	}

	payload := map[string]any{
		"to":   cfgStr(cfg, "sevenioReceiver"),
		"from": sender,
		"text": text,
	}
	return postJSONOK(ctx, "SevenIO", sevenIOSMSURL, payload, headers)
}

// ---------------- 360messenger (WhatsApp) ----------------

const (
	whatsapp360SendMessageURL = "https://api.360messenger.com/v2/sendMessage"
	whatsapp360SendGroupURL   = "https://api.360messenger.com/v2/sendGroup"
)

func sendWhatsapp360Messenger(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	const okMsg = "Sent Successfully."

	headers := map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + cfgStr(cfg, "Whatsapp360messengerAuthToken"),
	}

	message := m.Msg
	if cfgBool(cfg, "Whatsapp360messengerUseTemplate") {
		if tpl := cfgStr(cfg, "Whatsapp360messengerTemplate"); tpl != "" {
			message = g1RenderTemplate(tpl, m)
		}
	}

	// 收件人：逗号/分号分隔
	var recipients []string
	for _, r := range strings.FieldsFunc(cfgStr(cfg, "Whatsapp360messengerRecipient"), func(r rune) bool {
		return r == ';' || r == ','
	}) {
		if r = strings.TrimSpace(r); r != "" {
			recipients = append(recipients, r)
		}
	}

	// 群组：兼容数组（多选）、对象数组 {id} 与分隔字符串。
	// 官方是 `GroupIds || GroupId || ""`：空数组在 JS 里是 truthy，因此只有缺失/空串才回退。
	rawGroups := cfgRaw(cfg, "Whatsapp360messengerGroupIds")
	if s, ok := rawGroups.(string); (ok && strings.TrimSpace(s) == "") || rawGroups == nil {
		rawGroups = cfgRaw(cfg, "Whatsapp360messengerGroupId")
	}
	groupIDs := whatsapp360GroupIDs(rawGroups)

	hasGroup := len(groupIDs) > 0
	hasRecipient := len(recipients) > 0

	sendToRecipient := func(recipient string) error {
		payload := map[string]any{"phonenumber": recipient, "text": message}
		_, err := postJSONOK(ctx, "360messenger", whatsapp360SendMessageURL, payload, headers)
		return err
	}
	sendToGroup := func(groupID string) error {
		payload := map[string]any{"groupId": groupID, "text": message}
		_, err := postJSONOK(ctx, "360messenger", whatsapp360SendGroupURL, payload, headers)
		return err
	}

	switch {
	case hasGroup && hasRecipient:
		for _, r := range recipients {
			if err := sendToRecipient(r); err != nil {
				return "", err
			}
		}
		for _, g := range groupIDs {
			if err := sendToGroup(g); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%s (Sent to %d recipient(s) and %d group(s))", okMsg, len(recipients), len(groupIDs)), nil

	case hasGroup:
		for _, g := range groupIDs {
			if err := sendToGroup(g); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%s (Sent to %d group(s))", okMsg, len(groupIDs)), nil

	case hasRecipient:
		for _, r := range recipients {
			if err := sendToRecipient(r); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%s (Sent to %d recipient(s))", okMsg, len(recipients)), nil

	default:
		return "", fmt.Errorf("No recipient or group specified")
	}
}

// whatsapp360GroupIDs 把配置里的群组值归一化成字符串列表。
func whatsapp360GroupIDs(v any) []string {
	var out []string
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		for _, g := range strings.FieldsFunc(t, func(r rune) bool { return r == ';' || r == ',' }) {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
	case []any:
		for _, item := range t {
			switch iv := item.(type) {
			case string:
				if s := strings.TrimSpace(iv); s != "" {
					out = append(out, s)
				}
			case map[string]any:
				if id, ok := iv["id"]; ok {
					if s := strings.TrimSpace(cfgValueString(id)); s != "" {
						out = append(out, s)
					}
				}
			default:
				if s := strings.TrimSpace(cfgValueString(iv)); s != "" {
					out = append(out, s)
				}
			}
		}
	case []string:
		for _, item := range t {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
	default:
		if s := strings.TrimSpace(cfgValueString(v)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------- Brevo ----------------

const brevoEmailURL = "https://api.brevo.com/v3/smtp/email"

func sendBrevo(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"api-key":      cfgStr(cfg, "brevoApiKey"),
	}

	fromName := cfgStr(cfg, "brevoFromName")
	if fromName == "" {
		fromName = "Uptime Kuma"
	}
	subject := cfgStr(cfg, "brevoSubject")
	if subject == "" {
		subject = "Notification from Your Uptime Kuma"
	}

	payload := map[string]any{
		"sender": map[string]any{
			"email": strings.TrimSpace(cfgStr(cfg, "brevoFromEmail")),
			"name":  fromName,
		},
		"to": []any{
			map[string]any{"email": cfgStr(cfg, "brevoToEmail")},
		},
		"subject":     subject,
		"htmlContent": "<html><head></head><body><p>" + strings.ReplaceAll(m.Msg, "\n", "<br>") + "</p></body></html>",
	}

	if cc := cfgStr(cfg, "brevoCcEmail"); cc != "" {
		var list []any
		for _, email := range strings.Split(cc, ",") {
			list = append(list, map[string]any{"email": strings.TrimSpace(email)})
		}
		payload["cc"] = list
	}
	if bcc := cfgStr(cfg, "brevoBccEmail"); bcc != "" {
		var list []any
		for _, email := range strings.Split(bcc, ",") {
			list = append(list, map[string]any{"email": strings.TrimSpace(email)})
		}
		payload["bcc"] = list
	}

	code, body, err := postJSONBody(ctx, brevoEmailURL, payload, headers)
	if err != nil {
		return "", err
	}
	if code == 201 {
		return "Sent Successfully.", nil
	}
	if code < 200 || code >= 300 {
		return "", expectOK("Brevo", code, body)
	}
	return "", fmt.Errorf("Unexpected status code: %d", code)
}

// ---------------- VK ----------------

const vkMessagesSendURL = "https://api.vk.ru/method/messages.send"

func sendVK(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	dontParseLinks := "0"
	if cfgBool(cfg, "vkDontParseLinks") {
		dontParseLinks = "1"
	}

	form := url.Values{}
	form.Set("access_token", cfgStr(cfg, "vkAccessToken"))
	form.Set("v", cfgStr(cfg, "vkApiVersion"))
	form.Set("peer_id", cfgStr(cfg, "vkPeerId"))
	form.Set("message", m.Msg)
	form.Set("dont_parse_links", dontParseLinks)
	form.Set("random_id", strconv.Itoa(rand.IntN(2147483647)))

	code, body, err := postFormValues(ctx, vkMessagesSendURL, form, nil)
	if err != nil {
		return "", err
	}
	// axios 在非 2xx 时直接抛错，不会解析业务错误体
	if err := expectOK("VK", code, body); err != nil {
		return "", err
	}

	var res struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code int    `json:"error_code"`
			Msg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", err
	}
	if res.Error != nil {
		return "", fmt.Errorf("VK API returned error %d: %s", res.Error.Code, res.Error.Msg)
	}
	if len(res.Response) == 0 || string(res.Response) == "null" {
		return "", fmt.Errorf("Invalid VK API response")
	}

	return "Sent Successfully.", nil
}
