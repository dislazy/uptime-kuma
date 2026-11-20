package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// 本文件移植第二批官方通知渠道（G2 组）。
//
// 逐条对照 server/notification-providers/*.js 实现，
// URL / 方法 / 请求体 / headers / 必填校验 / 成功文案均保持一致。
// 唯一有意的降级：官方用 Settings.get("primaryBaseURL") 拼接面板跳转链接，
// 而 notify 包拿不到用户设置，这部分链接省略（不影响告警送达）。

// ---------------- 公共小工具 ----------------

// g2BasicAuth 生成 Basic 认证头所需的 base64(user:pass)。
func g2BasicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// g2StripNonASCII 对应 JS 的 msg.replace(/[^\x00-\x7F]/g, "")。
// 短信网关只吃 ASCII，emoji 与中文都会被丢掉。
func g2StripNonASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] < 0x80 {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// g2StripSpaces 对应 JS 的 name.replace(/\s/g, "")。
func g2StripSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// g2TruncRunes 按字符数截断，对应 JS 的 String.slice(0, n)。
func g2TruncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// g2NumOr 读取数字配置，键不存在时返回默认值（对应 JS 的 ?? default）。
func g2NumOr(cfg map[string]any, key string, def float64) float64 {
	v := cfgRaw(cfg, key)
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return def
	}
	return cfgFloat(cfg, key)
}

// g2JSONNum 对应 JS 的 .map(Number)：解析失败记为 null。
func g2JSONNum(s string) any {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return f
}

// g2SplitNums 把 "1,2,3" 转成 [1,2,3]。
func g2SplitNums(s string) []any {
	out := []any{}
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		out = append(out, g2JSONNum(part))
	}
	return out
}

// g2BeatTime 返回官方 heartbeatJSON 里的 timezone / localDateTime 两个字段。
// 本地 Beat 只存了一个时间字符串，这里尽力还原，缺失时用当前时间兜底。
func g2BeatTime(m Message) (tz, local string) {
	local = strings.TrimSpace(m.Beat.Time)
	if local == "" {
		local = time.Now().Format("2006-01-02 15:04:05")
	}
	tz = strings.TrimSpace(os.Getenv("TZ"))
	if tz == "" {
		if name := time.Local.String(); name != "" && name != "Local" {
			tz = name
		} else {
			tz = time.Now().Format("MST")
		}
	}
	return tz, local
}

// g2IsHTTPURL 判断能否作为 trigv 的 url 字段直接外发。
func g2IsHTTPURL(raw string) bool {
	if raw == "" || len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ---------------- AlertNow ----------------

func init() { registerFunc("AlertNow", sendAlertNow) }

func sendAlertNow(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	textMsg := ""
	status := "open"
	eventType := "ERROR"
	eventID := time.Now().UTC().Format("20060102")

	switch m.Status() {
	case 1:
		textMsg = fmt.Sprintf("[%s] ✅ Application is back online", m.MonitorName())
		status = "close"
		eventType = "INFO"
		eventID += "_" + g2StripSpaces(m.MonitorName())
	case 0:
		textMsg = fmt.Sprintf("[%s] 🔴 Application went down", m.MonitorName())
	}

	textMsg += " - " + m.Msg

	payload := map[string]any{
		"summary":    textMsg,
		"status":     status,
		"event_type": eventType,
		"event_id":   eventID,
	}

	return postJSONOK(ctx, "AlertNow", cfgStr(cfg, "alertNowWebhookURL"), payload, nil)
}

// ---------------- ClickSend SMS ----------------

func init() { registerFunc("clicksendsms", sendClickSendSMS) }

func sendClickSendSMS(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	const target = "https://rest.clicksend.com/v3/sms/send"

	headers := map[string]string{
		"Authorization": "Basic " + g2BasicAuth(cfgStr(cfg, "clicksendsmsLogin"), cfgStr(cfg, "clicksendsmsPassword")),
		"Accept":        "text/json",
	}

	payload := map[string]any{
		"messages": []any{
			map[string]any{
				"body":   g2StripNonASCII(m.Msg),
				"to":     cfgStr(cfg, "clicksendsmsToNumber"),
				"source": "uptime-kuma",
				"from":   cfgStr(cfg, "clicksendsmsSenderName"),
			},
		},
	}

	code, body, err := postJSONBody(ctx, target, payload, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("ClickSend SMS", code, body); err != nil {
		return "", err
	}

	// 官方只认 messages[0].status === "SUCCESS"
	var res struct {
		Data struct {
			Messages []struct {
				Status string `json:"status"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", fmt.Errorf("ClickSend SMS: invalid response: %s", body)
	}
	if len(res.Data.Messages) > 0 && res.Data.Messages[0].Status != "SUCCESS" {
		return "", fmt.Errorf("Something gone wrong. Api returned %s.", res.Data.Messages[0].Status)
	}

	return "Sent Successfully.", nil
}

// ---------------- 46elks ----------------

func init() { registerFunc("Elks", sendElks) }

func sendElks(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	const target = "https://api.46elks.com/a1/sms"

	form := url.Values{}
	form.Set("from", cfgStr(cfg, "elksFromNumber"))
	form.Set("to", cfgStr(cfg, "elksToNumber"))
	form.Set("message", m.Msg)

	headers := map[string]string{
		"Authorization": "Basic " + g2BasicAuth(cfgStr(cfg, "elksUsername"), cfgStr(cfg, "elksAuthToken")),
	}

	return postFormOK(ctx, "46elks", target, form, headers)
}

// ---------------- Home Assistant ----------------

const g2DefaultNotificationService = "notify"

func init() { registerFunc("HomeAssistant", sendHomeAssistant) }

func sendHomeAssistant(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	service := cfgStr(cfg, "notificationService")
	if service == "" {
		service = g2DefaultNotificationService
	}

	base := strings.TrimRight(strings.TrimSpace(cfgStr(cfg, "homeAssistantUrl")), "/")
	target := base + "/api/services/notify/" + service

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "longLivedAccessToken"),
	}

	payload := map[string]any{
		"title":   "Uptime Kuma",
		"message": m.Msg,
	}
	if service != "persistent_notification" {
		data := map[string]any{
			"channel":  "Uptime Kuma",
			"icon_url": "https://github.com/louislam/uptime-kuma/blob/master/public/icon.png?raw=true",
		}
		// 测试消息没有 monitorJSON / heartbeatJSON，官方对应字段为 undefined，直接省略
		if m.Monitor != nil {
			data["name"] = m.MonitorName()
			data["status"] = m.Status()
		}
		payload["data"] = data
	}

	return postJSONOK(ctx, "HomeAssistant", target, payload, headers)
}

// ---------------- LunaSea ----------------

func init() { registerFunc("lunasea", sendLunaSea) }

func sendLunaSea(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	const base = "https://notify.lunasea.app/v1"

	target := "device/" + cfgStr(cfg, "lunaseaDevice")
	if cfgStr(cfg, "lunaseaTarget") == "user" {
		target = "user/" + cfgStr(cfg, "lunaseaUserID")
	}
	endpoint := base + "/custom/" + target

	if m.Monitor == nil {
		// 测试消息
		payload := map[string]any{
			"title": "Uptime Kuma Alert",
			"body":  m.Msg,
		}
		return postJSONOK(ctx, "LunaSea", endpoint, payload, nil)
	}

	var mark string
	switch m.Status() {
	case 0:
		mark = "🔴 Down"
	case 1:
		mark = "✅ Up"
	default:
		return "", nil
	}

	tz, local := g2BeatTime(m)
	payload := map[string]any{
		"title": "UptimeKuma Alert: " + m.MonitorName(),
		"body":  "[" + mark + "] " + m.Beat.Msg + "\nTime (" + tz + "): " + local,
	}

	return postJSONOK(ctx, "LunaSea", endpoint, payload, nil)
}

// ---------------- OneBot ----------------

func init() { registerFunc("OneBot", sendOneBot) }

func sendOneBot(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	addr := cfgStr(cfg, "httpAddr")
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}
	if !strings.HasSuffix(addr, "/") {
		addr += "/"
	}
	addr += "send_msg"

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "accessToken"),
	}

	payload := map[string]any{
		"auto_escape": true,
		"message":     "UptimeKuma Alert: " + m.Msg,
	}
	if cfgStr(cfg, "msgType") == "group" {
		payload["message_type"] = "group"
		payload["group_id"] = cfgStr(cfg, "recieverId")
	} else {
		payload["message_type"] = "private"
		payload["user_id"] = cfgStr(cfg, "recieverId")
	}

	return postJSONOK(ctx, "OneBot", addr, payload, headers)
}

// ---------------- FlashDuty ----------------

func init() { registerFunc("FlashDuty", sendFlashDuty) }

func sendFlashDuty(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	// 测试消息没有 monitorJSON，官方现造一个假的 ping 监控
	if m.Monitor == nil {
		return g2FlashDutyPost(ctx, cfg, "Uptime Kuma Alert", m.Msg,
			map[string]any{"type": "ping", "url": m.Msg, "name": "https://flashcat.cloud"}, "Info")
	}

	var (
		title       string
		eventStatus string
	)
	switch m.Status() {
	case 1:
		title = "Uptime Kuma Monitor ✅ Up"
		eventStatus = "Ok"
	case 0:
		title = "Uptime Kuma Monitor 🔴 Down"
		eventStatus = cfgStr(cfg, "flashdutySeverity")
	default:
		return "", nil
	}

	monitor := map[string]any{
		"id":       m.Monitor.ID,
		"name":     m.MonitorName(),
		"type":     m.Monitor.Type,
		"url":      m.Monitor.Get("url"),
		"hostname": m.Monitor.Get("hostname"),
		"port":     m.Monitor.Get("port"),
	}
	if len(m.Monitor.Tags) > 0 {
		tags := make([]any, 0, len(m.Monitor.Tags))
		for _, t := range m.Monitor.Tags {
			tags = append(tags, map[string]any{"name": t.Name, "value": t.Value})
		}
		monitor["tags"] = tags
	}

	return g2FlashDutyPost(ctx, cfg, title, m.Beat.Msg, monitor, eventStatus)
}

// g2FlashDutyGenMonitorURL 对应官方的 genMonitorUrl。
func g2FlashDutyGenMonitorURL(monitor map[string]any) string {
	get := func(k string) string {
		if v, ok := monitor[k]; ok {
			return cfgValueString(v)
		}
		return ""
	}
	port := get("port")
	if get("type") == "port" && port != "" {
		return get("hostname") + ":" + port
	}
	if h := get("hostname"); h != "" {
		return h
	}
	return get("url")
}

func g2FlashDutyPost(ctx context.Context, cfg map[string]any, title, body string, monitor map[string]any, eventStatus string) (string, error) {
	labels := map[string]string{
		"resource": g2FlashDutyGenMonitorURL(monitor),
		"check":    cfgValueString(monitor["name"]),
	}
	if tags, ok := monitor["tags"].([]any); ok {
		for _, raw := range tags {
			if tag, ok := raw.(map[string]any); ok {
				labels[cfgValueString(tag["name"])] = cfgValueString(tag["value"])
			}
		}
	}

	if eventStatus == "" {
		eventStatus = "Info"
	}

	alertKey := "0"
	if id := cfgValueString(monitor["id"]); id != "" && id != "0" {
		alertKey = id
	} else {
		alertKey = strconv.FormatInt(time.Now().UnixNano(), 36)
	}

	key := cfgStr(cfg, "flashdutyIntegrationKey")
	target := key
	if !strings.HasPrefix(key, "http") {
		target = "https://api.flashcat.cloud/event/push/alert/standard?integration_key=" + key
	}

	payload := map[string]any{
		"description":  fmt.Sprintf("[%s] [%s] %s", title, cfgValueString(monitor["name"]), body),
		"title":        title,
		"event_status": eventStatus,
		"alert_key":    alertKey,
		"labels":       labels,
	}

	code, _, err := postJSONBody(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("FlashDuty notification failed with status code %d", code)
	}
	return "FlashDuty notification succeed: " + http.StatusText(code), nil
}

// ---------------- Pumble ----------------

func init() { registerFunc("pumble", sendPumble) }

func sendPumble(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := cfgStr(cfg, "webhookURL")

	var attachment map[string]any
	if m.Monitor == nil {
		attachment = map[string]any{
			"title": "Uptime Kuma Alert",
			"text":  m.Msg,
			"color": "#5BDD8B",
		}
	} else {
		state := "down"
		color := "#DC3645"
		if m.Status() == 1 {
			state = "up"
			color = "#5BDD8B"
		}
		attachment = map[string]any{
			"title": m.MonitorName() + " is " + state,
			"text":  m.Beat.Msg,
			"color": color,
		}
	}

	payload := map[string]any{"attachments": []any{attachment}}
	return postJSONOK(ctx, "Pumble", target, payload, nil)
}

// ---------------- Signalgrid ----------------

func init() { registerFunc("signalgrid", sendSignalGrid) }

func sendSignalGrid(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	kind := "INFO"
	title := "Uptime Kuma"
	switch m.Status() {
	case 0:
		kind = "CRIT"
		title = m.MonitorName() + " Down"
	case 1:
		kind = "SUCCESS"
		title = m.MonitorName() + " Up"
	}

	form := url.Values{}
	form.Set("client_key", cfgStr(cfg, "signalgridClientKey"))
	form.Set("channel", cfgStr(cfg, "signalgridChannel"))
	form.Set("title", title)
	form.Set("body", m.Msg)
	form.Set("type", kind)
	form.Set("critical", "false")

	code, body, err := postFormValues(ctx, "https://api.signalgrid.co/v1/push", form, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("Signalgrid", code, body); err != nil {
		return "", err
	}

	var res struct {
		Code any    `json:"code"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	// 官方只在 code 为真值时才判错（code=0 / 空表示未返回错误码）
	if res.Code != nil {
		if code := fmt.Sprint(res.Code); code != "0" && code != "" && code != "200" {
			if res.Text != "" {
				return "", fmt.Errorf("%s", res.Text)
			}
			return "", fmt.Errorf("Signalgrid API request failed")
		}
	}

	return "Sent Successfully.", nil
}

// ---------------- SMSEagle ----------------

func init() { registerFunc("SMSEagle", sendSMSEagle) }

func sendSMSEagle(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	switch cfgStr(cfg, "smseagleApiType") {
	case "smseagle-apiv1":
		return g2SMSEagleV1(ctx, cfg, m)
	case "smseagle-apiv2":
		return g2SMSEagleV2(ctx, cfg, m)
	default:
		return "", fmt.Errorf("SMSEagle API type is invalid")
	}
}

// g2SMSEagleV1 走 https://www.smseagle.eu/apiv1/ 的 GET 接口。
func g2SMSEagleV1(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	recipientType := cfgStr(cfg, "smseagleRecipientType")

	var (
		recipientParam string
		sendMethod     string
		duration       string
		voiceID        string
	)

	switch recipientType {
	case "smseagle-contact":
		recipientParam = "contactname"
		sendMethod = "/send_tocontact"
	case "smseagle-group":
		recipientParam = "groupname"
		sendMethod = "/send_togroup"
	case "smseagle-to":
		recipientParam = "to"
		sendMethod = "/send_sms"
		if cfgStr(cfg, "smseagleMsgType") != "smseagle-sms" {
			duration = strconv.FormatInt(int64(g2NumOr(cfg, "smseagleDuration", 10)), 10)
			switch cfgStr(cfg, "smseagleMsgType") {
			case "smseagle-ring":
				sendMethod = "/ring_call"
			case "smseagle-tts":
				sendMethod = "/tts_call"
			case "smseagle-tts-advanced":
				sendMethod = "/tts_adv_call"
				voiceID = strconv.FormatInt(int64(g2NumOr(cfg, "smseagleTtsModel", 1)), 10)
			}
		}
	default:
		return "", fmt.Errorf("smseagleRecipientType is invalid")
	}

	q := url.Values{}
	q.Set("access_token", cfgStr(cfg, "smseagleToken"))
	q.Set(recipientParam, cfgStr(cfg, "smseagleRecipient"))

	// 官方这里比对的是 recipientType 原值，含一个历史遗留判断，保持原样
	if recipientType == "" || recipientType == "smseagle-sms" {
		unicodeFlag := "0"
		if cfgBool(cfg, "smseagleEncoding") {
			unicodeFlag = "1"
		}
		q.Set("unicode", unicodeFlag)
		priority := cfgStr(cfg, "smseaglePriority")
		if cfgRaw(cfg, "smseaglePriority") == nil {
			priority = "0"
		}
		q.Set("highpriority", priority)
	} else {
		q.Set("duration", duration)
	}

	if recipientType != "smseagle-ring" {
		q.Set("message", m.Msg)
	}
	if voiceID != "" {
		q.Set("voice_id", voiceID)
	}

	target := strings.TrimRight(cfgStr(cfg, "smseagleUrl"), "/") + "/http_api" + sendMethod
	code, body, err := getURL(ctx, target+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("SMSEagle", code, body); err != nil {
		return "", err
	}
	if !strings.Contains(body, "OK") {
		return "", fmt.Errorf("SMSEagle API returned error: %s", body)
	}

	return "Sent Successfully.", nil
}

// g2SMSEagleV2 走 https://www.smseagle.eu/docs/apiv2/ 的 JSON 接口。
func g2SMSEagleV2(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	encoding := "standard"
	if cfgBool(cfg, "smseagleEncoding") {
		encoding = "unicode"
	}

	var priority any = 0
	if v := cfgRaw(cfg, "smseaglePriority"); v != nil {
		priority = v
	}

	payload := map[string]any{
		"text":     m.Msg,
		"encoding": encoding,
		"priority": priority,
	}
	if v := cfgStr(cfg, "smseagleRecipientContact"); v != "" {
		payload["contacts"] = g2SplitNums(v)
	}
	if v := cfgStr(cfg, "smseagleRecipientGroup"); v != "" {
		payload["groups"] = g2SplitNums(v)
	}
	if v := cfgStr(cfg, "smseagleRecipientTo"); v != "" {
		payload["to"] = strings.Split(v, ",")
	}

	endpoint := "/messages/sms"
	if cfgStr(cfg, "smseagleMsgType") != "smseagle-sms" {
		payload["duration"] = int64(g2NumOr(cfg, "smseagleDuration", 10))
		switch cfgStr(cfg, "smseagleMsgType") {
		case "smseagle-ring":
			endpoint = "/calls/ring"
		case "smseagle-tts":
			endpoint = "/calls/tts"
		case "smseagle-tts-advanced":
			endpoint = "/calls/tts_advanced"
			payload["voice_id"] = int64(g2NumOr(cfg, "smseagleTtsModel", 1))
		}
	}

	headers := map[string]string{
		"access-token": cfgStr(cfg, "smseagleToken"),
	}
	target := strings.TrimRight(cfgStr(cfg, "smseagleUrl"), "/") + "/api/v2" + endpoint

	code, body, err := postJSONBody(ctx, target, payload, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("SMSEagle", code, body); err != nil {
		return "", err
	}

	var results []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		return "", fmt.Errorf("SMSEagle API returned error: %s", body)
	}
	if len(results) == 0 {
		return "", fmt.Errorf("SMSEagle API returned an empty response")
	}

	queued := 0
	for _, r := range results {
		if r.Status == "queued" {
			queued++
		}
	}
	if queued == 0 {
		return "", fmt.Errorf("SMSEagle API returned error: %s", body)
	}
	if queued < len(results) {
		return fmt.Sprintf("Sent %d/%d Messages Successfully.", queued, len(results)), nil
	}

	return "Sent Successfully.", nil
}

// ---------------- trigv ----------------

func init() { registerFunc("trigv", sendTrigv) }

func sendTrigv(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	title := msgTitle(m)
	description := m.Msg
	level := "info"

	if m.Monitor != nil {
		switch m.Status() {
		case 0:
			title += " is down"
			level = "error"
		case 1:
			title += " is up"
			level = "success"
		}
		tz, local := g2BeatTime(m)
		description = m.Beat.Msg + "\nTime (" + tz + "): " + local
	}

	channel := cfgStr(cfg, "trigvChannel")
	if channel == "" {
		channel = "general"
	}

	payload := map[string]any{
		"channel":     channel,
		"title":       g2TruncRunes(title, 255),
		"description": g2TruncRunes(description, 1000),
		"level":       level,
	}
	if m.Monitor != nil {
		if u := m.Monitor.Get("url"); g2IsHTTPURL(u) {
			payload["url"] = u
		}
	}

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "trigvApiKey"),
		"Accept":        "application/json",
	}

	// 官方只把 202 视为成功
	code, body, err := postJSONBody(ctx, "https://api.trigv.com/v1/events", payload, headers)
	if err != nil {
		return "", err
	}
	if code != 202 {
		if snippet := strings.TrimSpace(body); snippet != "" {
			return "", fmt.Errorf("trigv: HTTP %d: %s", code, g2TruncRunes(snippet, 300))
		}
		return "", fmt.Errorf("trigv: HTTP %d", code)
	}

	return "Sent Successfully.", nil
}

// ---------------- Whapi ----------------

func init() { registerFunc("whapi", sendWhapi) }

func sendWhapi(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	base := cfgStr(cfg, "whapiApiUrl")
	if base == "" {
		base = "https://gate.whapi.cloud/"
	}
	target := strings.TrimRight(base, "/") + "/messages/text"

	headers := map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer " + cfgStr(cfg, "whapiAuthToken"),
	}

	payload := map[string]any{
		"to":   cfgStr(cfg, "whapiRecipient"),
		"body": m.Msg,
	}

	return postJSONOK(ctx, "Whapi", target, payload, headers)
}

// ---------------- GTX Messaging ----------------

func init() { registerFunc("gtxmessaging", sendGtxMessaging) }

func sendGtxMessaging(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	// gtx-messaging 会把 UP/DOWN 符号转成 "???"，所以先去掉
	text := strings.ReplaceAll(m.Msg, "🔴 ", "")
	text = strings.ReplaceAll(text, "✅ ", "")

	form := url.Values{}
	form.Set("from", strings.TrimSpace(cfgStr(cfg, "gtxMessagingFrom")))
	form.Set("to", strings.TrimSpace(cfgStr(cfg, "gtxMessagingTo")))
	form.Set("text", text)

	target := "https://rest.gtx-messaging.net/smsc/sendsms/" + cfgStr(cfg, "gtxMessagingApiKey") + "/json"

	return postFormOK(ctx, "GTX Messaging", target, form, nil)
}

// ---------------- Resend ----------------

func init() { registerFunc("Resend", sendResend) }

func sendResend(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	email := strings.TrimSpace(cfgStr(cfg, "resendFromEmail"))
	fromName := strings.TrimSpace(cfgStr(cfg, "resendFromName"))
	if fromName == "" {
		fromName = "Uptime Kuma"
	}

	subject := cfgStr(cfg, "resendSubject")
	if subject == "" {
		subject = "Notification from Your Uptime Kuma"
	}

	payload := map[string]any{
		"from":    fromName + " <" + email + ">",
		"to":      cfgStr(cfg, "resendToEmail"),
		"subject": subject,
		"text":    m.Msg,
	}

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "resendApiKey"),
	}

	code, body, err := postJSONBody(ctx, "https://api.resend.com/emails", payload, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("Resend", code, body); err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("Unexpected status code: %d", code)
	}

	return "Sent Successfully.", nil
}

// ---------------- VK Teams ----------------

func init() { registerFunc("VKTeams", sendVKTeams) }

func sendVKTeams(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	baseURL := strings.TrimRight(cfgStr(cfg, "vkteamsBaseUrl"), "/")
	if baseURL == "" {
		baseURL = "https://myteam.mail.ru"
	}

	text := m.Msg
	parseMode := ""
	if cfgBool(cfg, "vkteamsUseTemplate") {
		if tpl := cfgStr(cfg, "vkteamsTemplate"); tpl != "" {
			text = g2RenderTemplate(tpl, m)
			format := cfgStr(cfg, "vkteamsTemplateFormat")
			if format != "" && format != "plain" {
				parseMode = format
			}
		}
	}

	q := url.Values{}
	q.Set("token", cfgStr(cfg, "vkteamsBotToken"))
	q.Set("chatId", cfgStr(cfg, "vkteamsChatId"))
	q.Set("text", text)
	if parseMode != "" {
		q.Set("parseMode", parseMode)
	}

	target := baseURL + "/bot/v1/messages/sendText?" + q.Encode()
	code, body, err := getURL(ctx, target, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("VKTeams", code, body); err != nil {
		return "", err
	}

	var res struct {
		OK          *bool  `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	if res.OK != nil && !*res.OK {
		return "", fmt.Errorf("VKTeams API returned error: %s", res.Description)
	}

	return "Sent Successfully.", nil
}

// g2RenderTemplate 是官方 liquidjs 模板的极简替代。
//
// 支持官方文档里出现的变量：status / name / hostnameOrURL / msg，
// 以及 v1 兼容的大写别名；同时支持 monitorJSON.x / heartbeatJSON.x 点取值
// 和 upcase / downcase 两个常用过滤器。
// 复杂表达式（循环、条件、默认值）不支持，按原样留空——
// 官方模板的常见用法（拼标题、拼正文）都能正常工作。
func g2RenderTemplate(tpl string, m Message) string {
	status := "⚠️ Test"
	if m.Monitor != nil {
		if m.Status() == 0 {
			status = "🔴 Down"
		} else {
			status = "✅ Up"
		}
	}

	name := "Monitor Name not available"
	addr := "testing.hostname"
	if m.Monitor != nil {
		name = m.MonitorName()
		addr = g2ExtractAddress(m.Monitor)
	}

	values := map[string]string{
		"STATUS":          status,
		"NAME":            name,
		"HOSTNAME_OR_URL": addr,
		"status":          status,
		"name":            name,
		"hostnameOrURL":   addr,
		"msg":             m.Msg,
	}
	monitorFields := map[string]string{}
	beatFields := map[string]string{
		"status": strconv.Itoa(m.Status()),
		"msg":    m.Beat.Msg,
		"time":   m.Beat.Time,
	}
	if m.Monitor != nil {
		monitorFields = map[string]string{
			"id":       strconv.FormatInt(m.Monitor.ID, 10),
			"name":     m.Monitor.Name,
			"type":     m.Monitor.Type,
			"url":      m.Monitor.Get("url"),
			"hostname": m.Monitor.Get("hostname"),
			"port":     m.Monitor.Get("port"),
		}
	}

	var out strings.Builder
	rest := tpl
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:start])
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			out.WriteString(rest[start:])
			break
		}
		expr := rest[start+2 : start+end]
		out.WriteString(g2EvalExpr(expr, values, monitorFields, beatFields))
		rest = rest[start+end+2:]
	}

	// Liquid 的 block 标签（{% if %} 等）在极简实现里整体丢弃
	result := out.String()
	for {
		start := strings.Index(result, "{%")
		if start < 0 {
			break
		}
		end := strings.Index(result[start:], "%}")
		if end < 0 {
			break
		}
		result = result[:start] + result[start+end+2:]
	}
	return result
}

func g2EvalExpr(expr string, values, monitorFields, beatFields map[string]string) string {
	parts := strings.Split(expr, "|")
	key := strings.TrimSpace(parts[0])

	var val string
	switch {
	case strings.HasPrefix(key, "monitorJSON."):
		val = monitorFields[strings.TrimPrefix(key, "monitorJSON.")]
	case strings.HasPrefix(key, "heartbeatJSON."):
		val = beatFields[strings.TrimPrefix(key, "heartbeatJSON.")]
	default:
		val = values[key]
	}

	for _, raw := range parts[1:] {
		switch strings.TrimSpace(raw) {
		case "upcase":
			val = strings.ToUpper(val)
		case "downcase":
			val = strings.ToLower(val)
		}
	}
	return val
}

// g2ExtractAddress 对应基类的 extractAddress，用于 {{ hostnameOrURL }}。
func g2ExtractAddress(monitor *model.Monitor) string {
	if monitor == nil {
		return ""
	}
	hostname := monitor.Get("hostname")
	port := monitor.Get("port")
	switch monitor.Type {
	case "push":
		return "Heartbeat"
	case "ping", "dns":
		return hostname
	case "port", "gamedig", "steam":
		if port != "" {
			return hostname + ":" + port
		}
		return hostname
	case "globalping":
		switch monitor.Get("subtype") {
		case "ping", "dns":
			return hostname
		case "http":
			return monitor.Get("url")
		default:
			return ""
		}
	default:
		if u := monitor.Get("url"); u != "https://" && u != "http://" && u != "" {
			return u
		}
		return ""
	}
}
