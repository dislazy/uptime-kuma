package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// 本文件移植 g4 组的 14 个官方渠道（server/notification-providers/*.js）。
//
// 约定：
//   - 配置键名与官方 this.config 完全一致（前端 v-model 直接绑定这些键）；
//   - 成功文案沿用官方："Sent Successfully."；
//   - 官方读全局 setting("primaryBaseURL") 拼监控详情链接，Go 侧没有该全局设置，
//     改为读取配置里可选的 primaryBaseURL：有则拼、无则省略，其余行为不变。

func init() {
	registerFunc("bale", sendBale)
	registerFunc("amootsms", sendAmootSMS)
	registerFunc("FreeMobile", sendFreeMobile)
	registerFunc("HeiiOnCall", sendHeiiOnCall)
	registerFunc("Ooredoo", sendOoredoo)
	registerFunc("PagerTree", sendPagerTree)
	registerFunc("PushByTechulus", sendTechulusPush)
	registerFunc("SMSGateway", sendSMSGateway)
	registerFunc("stackfield", sendStackfield)
	registerFunc("Splunk", sendSplunk)
	registerFunc("openwa", sendOpenWA)
	registerFunc("WPush", sendWPush)
	registerFunc("SMSPlanet", sendSMSPlanet)
}

// ---------------- 本文件公用小工具 ----------------

// g4BaseURL 读可选的 primaryBaseURL（官方从全局设置读）。
func g4BaseURL(cfg map[string]any) string {
	return strings.TrimRight(strings.TrimSpace(cfgStr(cfg, "primaryBaseURL")), "/")
}

// g4MonitorURL 返回 {baseURL}/dashboard/{id}，对应官方 getMonitorRelativeURL。
func g4MonitorURL(baseURL string, id int64) string {
	return baseURL + "/dashboard/" + strconv.FormatInt(id, 10)
}

// g4MonitorMap 构造官方 monitorJSON 等价物；测试投递返回 nil（对应 JS 的 null）。
func g4MonitorMap(m Message) map[string]any {
	if m.Monitor == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range m.Monitor.Raw {
		out[k] = v
	}
	out["id"] = m.Monitor.ID
	out["name"] = m.Monitor.Name
	out["type"] = m.Monitor.Type
	return out
}

// g4HeartbeatMap 构造官方 heartbeatJSON 等价物；测试投递返回 nil。
func g4HeartbeatMap(m Message) map[string]any {
	if m.Monitor == nil {
		return nil
	}
	return m.Beat.JSON()
}

// g4JSONObject 把响应体解析成对象；非对象（数组、字符串、空）返回 false。
// 官方对应 `!result || typeof result !== "object"`。
func g4JSONObject(body string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || trimmed[0] != '{' {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// ---------------- Bale ----------------

func sendBale(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := "https://tapi.bale.ai/bot" + cfgStr(cfg, "baleBotToken") + "/sendMessage"
	payload := map[string]any{
		"chat_id": cfgStr(cfg, "baleChatID"),
		"text":    m.Msg,
	}
	return postJSONOK(ctx, "bale", target, payload, map[string]string{
		"content-type": "application/json",
	})
}

// ---------------- Amoot SMS ----------------

// amootStatusMessages 是官方 statusMessages 表的完整移植。
var amootStatusMessages = map[int64]string{
	-300: "System_Disabled",
	-208: "Token_NotAllowMethod",
	-207: "Token_NotAllowIP",
	-204: "Token_Expired",
	-203: "Token_Disabled",
	-202: "Token_Invalid",
	-201: "Token_NotExists",
	-109: "User_NotAllowHttp",
	-108: "User_NotAllowMethod",
	-107: "User_NotAllowIP",
	-106: "User_NotAllowWebService",
	-105: "User_WebServiceBanned",
	-104: "User_Expired",
	-103: "User_Disabled",
	-102: "User_MobileNotVerified",
	-1:   "User_NotExists",
	0:    "Failed",
	1:    "Success",
	2:    "AccountIsDemo",
	4:    "CreditNotEnough",
	5:    "LineNumber_NotExist",
	6:    "BackupLineNumber_NotExist",
	7:    "Avanak_NotAvailable",
	10:   "UserName_Empty",
	11:   "Password_Empty",
	12:   "LineNumber_Empty",
	13:   "BackupLineNumber_Empty",
	14:   "SMSMessageText_Empty",
	15:   "AvanakMessageText_Empty",
	16:   "Mobile_Empty",
	17:   "Mobiles_Empty",
	18:   "Title_Empty",
	19:   "FirstNameOrLastName_Empty",
	20:   "URLAddress_Empty",
	100:  "UserNameOrPassword_Invalid",
	101:  "Mobile_Invalid",
	102:  "Mobiles_Invalid",
	103:  "Count_Invalid",
	104:  "FromRow_Invalid",
	105:  "FromDate_Invalid",
	106:  "FromDateTime_Invalid",
	107:  "ToDate_Invalid",
	108:  "ToDateTime_Invalid",
	109:  "FromDateIsAfterThanToDate",
	110:  "FromDateTimeIsAfterThanToDateTime",
	111:  "MessageID_Invalid",
	112:  "BulkID_Invalid",
	113:  "ContactID_Invalid",
	114:  "ContactGroupID_Invalid",
	115:  "CourseID_Invalid",
	116:  "CourseGroupID_Invalid",
	117:  "URLAddress_Duplicate",
	118:  "RelayMessageDeliveryID_Invalid",
	119:  "RelayRecieveMessageID_Invalid",
	120:  "Length_Invalid",
	121:  "Length_Exceeded",
	500:  "ServerError",
}

// g4AmootStatusString 把 API 里的 Status（数字或字符串）转成字符串。
func g4AmootStatusString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(t)
	}
}

// g4AmootTopStatus 校验顶层 Status，对应官方
// `if (result.Status && result.Status !== "Success")`。
func g4AmootTopStatus(obj map[string]any) error {
	status := g4AmootStatusString(obj["Status"])
	if status == "" || status == "Success" {
		return nil
	}
	if em, ok := obj["ErrorMessage"].(string); ok && em != "" {
		return fmt.Errorf("Amoot SMS API error: %s: %s", status, em)
	}
	return fmt.Errorf("Amoot SMS API error: %s", status)
}

func sendAmootSMS(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	token := strings.TrimSpace(cfgStr(cfg, "amootApiToken"))
	mobiles := cfgStringList(cfg, "amootMobiles")
	if token == "" {
		return "", fmt.Errorf("Amoot SMS API token is required.")
	}
	if len(mobiles) == 0 {
		return "", fmt.Errorf("At least one Amoot SMS recipient number is required.")
	}

	usePattern := cfgBool(cfg, "amootUsePattern")
	useOwnLine := cfgBool(cfg, "amootUseOwnLine")
	patternCode := strings.TrimSpace(cfgStr(cfg, "amootPatternCodeId"))
	lineNumber := strings.TrimSpace(cfgStr(cfg, "amootLineNumber"))

	if usePattern && patternCode == "" {
		return "", fmt.Errorf("Amoot SMS Pattern Code ID is required.")
	}
	if (!usePattern || useOwnLine) && lineNumber == "" {
		return "", fmt.Errorf("Amoot SMS line number is required.")
	}

	if usePattern {
		if useOwnLine {
			return g4AmootSendWithPatternOwn(ctx, token, lineNumber, patternCode, mobiles, m.Msg)
		}
		return g4AmootSendWithPattern(ctx, token, patternCode, mobiles, m.Msg)
	}

	// 简单发送：SendSimple
	form := url.Values{}
	form.Set("SendDateTime", g4IranDateTime())
	form.Set("SMSMessageText", m.Msg)
	form.Set("LineNumber", lineNumber)
	form.Set("Mobiles", strings.Join(mobiles, ","))

	code, body, err := postFormValues(ctx, "https://portal.amootsms.com/rest/SendSimple", form, map[string]string{
		"Authorization": token,
	})
	if err != nil {
		return "", err
	}
	if err := expectOK("Amoot SMS", code, body); err != nil {
		return "", err
	}

	obj, ok := g4JSONObject(body)
	if !ok {
		return "", fmt.Errorf("Amoot SMS API returned an unexpected response.")
	}
	if err := g4AmootTopStatus(obj); err != nil {
		return "", err
	}

	rawData, ok := obj["Data"].([]any)
	if !ok || len(rawData) == 0 {
		return "", fmt.Errorf("Amoot SMS API returned an unexpected response.")
	}
	for _, item := range rawData {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status := entry["Status"]
		statusStr := g4AmootStatusString(status)
		if statusStr == "1" || statusStr == "Success" {
			continue
		}
		if n, perr := strconv.ParseInt(statusStr, 10, 64); perr == nil {
			title, found := amootStatusMessages[n]
			if !found {
				title = "UnknownError"
			}
			return "", fmt.Errorf("Amoot SMS API error: %s (%d)", title, n)
		}
		return "", fmt.Errorf("Amoot SMS API error: %s", statusStr)
	}

	return "Sent Successfully.", nil
}

// g4AmootSendWithPattern 对应官方 sendWithPattern：Token 放在表单里、Authorization 不带 Bearer。
func g4AmootSendWithPattern(ctx context.Context, token, patternCode string, mobiles []string, msg string) (string, error) {
	for _, mobile := range mobiles {
		form := url.Values{}
		form.Set("Token", token)
		form.Set("Mobile", mobile)
		form.Set("PatternCodeID", patternCode)
		form.Set("PatternValues", msg)

		code, body, err := postFormValues(ctx, "https://portal.amootsms.com/rest/SendWithPattern", form, map[string]string{
			"Authorization": token,
		})
		if err != nil {
			return "", err
		}
		if err := expectOK("Amoot SMS", code, body); err != nil {
			return "", err
		}
		obj, ok := g4JSONObject(body)
		if !ok {
			return "", fmt.Errorf("Amoot SMS API returned an unexpected response.")
		}
		if err := g4AmootTopStatus(obj); err != nil {
			return "", err
		}
	}
	return "Sent Successfully.", nil
}

// g4AmootSendWithPatternOwn 对应官方 sendWithPatternOwn：JSON + Bearer。
func g4AmootSendWithPatternOwn(ctx context.Context, token, lineNumber, patternCode string, mobiles []string, msg string) (string, error) {
	patternID, _ := strconv.ParseInt(patternCode, 10, 64)
	for _, mobile := range mobiles {
		payload := map[string]any{
			"Token":         token,
			"LineNumber":    lineNumber,
			"Mobile":        mobile,
			"PatternCodeID": patternID,
			"PatternValues": msg,
		}
		code, body, err := postJSONBody(ctx, "https://portal.amootsms.com/rest/SendWithPatternOWN", payload, map[string]string{
			"Authorization": "Bearer " + token,
		})
		if err != nil {
			return "", err
		}
		if err := expectOK("Amoot SMS", code, body); err != nil {
			return "", err
		}
		obj, ok := g4JSONObject(body)
		if !ok {
			return "", fmt.Errorf("Amoot SMS API returned an unexpected response.")
		}
		if err := g4AmootTopStatus(obj); err != nil {
			return "", err
		}
	}
	return "Sent Successfully.", nil
}

// g4IranDateTime 返回 Asia/Tehran 的 "YYYY-MM-DD HH:mm:ss"。
// 伊朗自 2022 年起不再使用夏令时，固定 +03:30 即可，避免依赖系统 tzdata。
func g4IranDateTime() string {
	return time.Now().In(time.FixedZone("+0330", 3*3600+1800)).Format("2006-01-02 15:04:05")
}

// ---------------- Free Mobile ----------------

func sendFreeMobile(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	// 官方只替换第一个 🔴 为 ⛔️
	text := strings.Replace(m.Msg, "🔴", "⛔️", 1)
	target := "https://smsapi.free-mobile.fr/sendmsg?msg=" + g4EncodeURIComponent(text)
	payload := map[string]any{
		"user": cfgStr(cfg, "freemobileUser"),
		"pass": cfgStr(cfg, "freemobilePass"),
	}
	return postJSONOK(ctx, "Free Mobile", target, payload, nil)
}

// g4EncodeURIComponent 与 JS 的 encodeURIComponent 对齐。
func g4EncodeURIComponent(s string) string {
	out := strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	r := strings.NewReplacer("%21", "!", "%2A", "*", "%27", "'", "%28", "(", "%29", ")")
	return r.Replace(out)
}

// ---------------- Heii On Call ----------------

func sendHeiiOnCall(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	triggerID := strings.TrimSpace(cfgStr(cfg, "heiiOnCallTriggerId"))
	if triggerID == "" {
		return "", fmt.Errorf("Heii On Call Trigger ID is required")
	}

	payload := map[string]any{}
	for k, v := range g4HeartbeatMap(m) {
		payload[k] = v
	}

	baseURL := g4BaseURL(cfg)
	if baseURL != "" && m.Monitor != nil {
		payload["url"] = g4MonitorURL(baseURL, m.Monitor.ID)
	}

	headers := map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + cfgStr(cfg, "heiiOnCallApiKey"),
	}
	base := "https://heiioncall.com/triggers/" + triggerID + "/"

	// 测试 / 证书过期等通用消息：走 alert
	if m.Monitor == nil {
		payload["msg"] = m.Msg
		return postJSONOK(ctx, "Heii On Call", base+"alert", payload, headers)
	}

	switch m.Status() {
	case 0: // DOWN
		return postJSONOK(ctx, "Heii On Call", base+"alert", payload, headers)
	case 1: // UP
		return postJSONOK(ctx, "Heii On Call", base+"resolve", payload, headers)
	}
	// 其它状态官方什么都不发，由引擎兜底成成功文案
	return "", nil
}

// ---------------- Nostr ----------------

// sendNostr 明确报告不支持：官方实现依赖 nostr-tools（BIP-340 schnorr 签名、
// NIP-44 加密、NIP-59 gift wrap）以及 WebSocket 中继客户端，标准库都不具备。
// 按约定宁可不实现，也不返回假的成功结果。

// ---------------- Ooredoo ----------------

const g4OoredooMaxRecipients = 20

func sendOoredoo(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimSpace(cfgStr(cfg, "ooredooServerUrl"))
	if target == "" {
		target = "https://o-papi1-lb01.ooredoo.mv/bulk_sms/v2"
	}

	var recipients []string
	for _, raw := range strings.FieldsFunc(cfgStr(cfg, "ooredooToNumber"), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
	}) {
		if n := g4OoredooNormalize(raw); n != "" {
			recipients = append(recipients, n)
		}
	}
	if len(recipients) == 0 {
		return "", fmt.Errorf("No valid recipient phone number was provided.")
	}

	headers := map[string]string{
		"Content-Type":  "application/x-www-form-urlencoded",
		"Authorization": "Bearer " + cfgStr(cfg, "ooredooBearerToken"),
	}
	accessKey := base64.StdEncoding.EncodeToString([]byte(cfgStr(cfg, "ooredooAccessKey")))
	username := cfgStr(cfg, "ooredooUsername")

	for i := 0; i < len(recipients); i += g4OoredooMaxRecipients {
		end := i + g4OoredooMaxRecipients
		if end > len(recipients) {
			end = len(recipients)
		}
		form := url.Values{}
		form.Set("username", username)
		form.Set("access_key", accessKey)
		form.Set("message", m.Msg)
		form.Set("batch", strings.Join(recipients[i:end], " "))

		code, body, err := postFormValues(ctx, target, form, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("Ooredoo", code, body); err != nil {
			return "", err
		}

		// 网关即使失败也回 200，真实结果在 response_code 里
		obj, ok := g4JSONObject(body)
		if !ok {
			return "", fmt.Errorf("Ooredoo rejected the message: response_code=unknown")
		}
		if g4AmootStatusString(obj["response_code"]) != "0" {
			reason := ""
			if rs, ok := obj["response_message"].(string); ok && rs != "" {
				reason = rs
			} else {
				reason = "response_code=" + g4AmootStatusString(obj["response_code"])
			}
			return "", fmt.Errorf("Ooredoo rejected the message: %s", reason)
		}
	}

	return "Sent Successfully.", nil
}

// g4OoredooNormalize 对应官方 normalizePhoneNumber：
// 去掉空白与 +，7 位本地号码补 960 国家码。
func g4OoredooNormalize(number string) string {
	n := strings.Map(func(r rune) rune {
		if r == ' ' || r == '+' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			return -1
		}
		return r
	}, number)
	if n == "" {
		return ""
	}
	if len(n) == 7 {
		allDigits := true
		for _, r := range n {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return "960" + n
		}
	}
	return n
}

// ---------------- PagerTree ----------------

func sendPagerTree(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	if m.Monitor == nil {
		// 通用消息 / 测试
		return g4PagerTreePost(ctx, cfg, m, m.Msg, "create")
	}
	if isUp(m) && cfgStr(cfg, "pagertreeAutoResolve") == "resolve" {
		return g4PagerTreePost(ctx, cfg, m, "", "resolve")
	}
	if m.Status() == 0 {
		title := fmt.Sprintf("Uptime Kuma Monitor %q is DOWN", m.Monitor.Name)
		return g4PagerTreePost(ctx, cfg, m, title, "create")
	}
	// UP 但未开启自动恢复：官方不发任何请求
	return "", nil
}

func g4PagerTreePost(ctx context.Context, cfg map[string]any, m Message, title, eventAction string) (string, error) {
	if eventAction == "" {
		return "No action required", nil
	}

	beat := g4HeartbeatMap(m)
	eventID := any("uptime-kuma")
	if beat != nil && m.Beat.MonitorID != 0 {
		eventID = m.Beat.MonitorID
	}

	var titleVal any
	if title != "" {
		titleVal = title
	}

	payload := map[string]any{
		"event_type": eventAction,
		"id":         eventID,
		"title":      titleVal,
		"urgency":    cfgStr(cfg, "pagertreeUrgency"),
		"heartbeat":  beat,
		"monitor":    g4MonitorMap(m),
	}

	code, _, err := postJSONBody(ctx, cfgStr(cfg, "pagertreeIntegrationUrl"), payload, map[string]string{
		"Content-Type": "application/json",
	})
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("PagerTree notification failed with status code %d", code)
	}
	if text := http.StatusText(code); text != "" {
		return "PagerTree notification succeed: " + text, nil
	}
	return "Sent Successfully.", nil
}

// ---------------- Push by Techulus ----------------

func sendTechulusPush(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	title := cfgStr(cfg, "pushTitle")
	if title == "" {
		title = "Uptime-Kuma"
	}

	payload := map[string]any{
		"title": title,
		"body":  m.Msg,
		// 官方是 `?? true`：只有显式 false 才关闭
		"timeSensitive": true,
	}
	if v, ok := cfgRaw(cfg, "pushTimeSensitive").(bool); ok {
		payload["timeSensitive"] = v
	}
	if ch := cfgStr(cfg, "pushChannel"); ch != "" {
		payload["channel"] = ch
	}
	if snd := cfgStr(cfg, "pushSound"); snd != "" {
		payload["sound"] = snd
	}

	target := "https://push.techulus.com/api/v1/notify/" + cfgStr(cfg, "pushAPIKey")
	return postJSONOK(ctx, "PushByTechulus", target, payload, nil)
}

// ---------------- SMS Gateway ----------------

func sendSMSGateway(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimRight(cfgStr(cfg, "smsgatewayUrl"), "/") + "/api/v1/sms/send"
	headers := map[string]string{"X-API-Key": cfgStr(cfg, "smsgatewayApiKey")}

	var failures []string
	for _, to := range cfgStringList(cfg, "smsgatewayTo") {
		payload := map[string]any{"to": to, "body": m.Msg}
		code, body, err := postJSONBody(ctx, target, payload, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("SMS Gateway", code, body); err != nil {
			return "", err
		}
		// 网关用 HTTP 200 掩盖发送失败，必须看 body 里的 status
		if obj, ok := g4JSONObject(body); ok {
			if status, _ := obj["status"].(string); status == "failed" {
				reason, _ := obj["message"].(string)
				if reason == "" {
					reason = "unknown error"
				}
				failures = append(failures, to+": "+reason)
			}
		}
	}
	if len(failures) > 0 {
		return "", fmt.Errorf("Failed to send SMS to %s", strings.Join(failures, "; "))
	}
	return "Sent Successfully.", nil
}

// ---------------- Stackfield ----------------

func sendStackfield(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	textMsg := "+Uptime Kuma Alert+"
	if m.Monitor != nil && m.Monitor.Name != "" {
		textMsg += "\n*" + m.Monitor.Name + "*"
	}
	textMsg += "\n" + m.Msg

	if baseURL := g4BaseURL(cfg); baseURL != "" {
		if m.Monitor != nil {
			textMsg += "\n" + g4MonitorURL(baseURL, m.Monitor.ID)
		} else {
			textMsg += "\n" + baseURL + "/"
		}
	}

	payload := map[string]any{"Title": textMsg}
	return postJSONOK(ctx, "Stackfield", cfgStr(cfg, "stackfieldwebhookURL"), payload, nil)
}

// ---------------- Splunk ----------------

func sendSplunk(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	if m.Monitor == nil {
		// 测试按钮：官方伪造一个 ping 监控
		monitor := map[string]any{"type": "ping", "url": "Uptime Kuma Test Button"}
		return g4SplunkPost(ctx, cfg, "Uptime Kuma Alert", m.Msg, monitor, "trigger")
	}

	monitor := g4SplunkMonitor(m)
	switch m.Status() {
	case 1: // UP
		return g4SplunkPost(ctx, cfg, "Uptime Kuma Monitor ✅ Up", m.Beat.Msg, monitor, "recovery")
	case 0: // DOWN
		return g4SplunkPost(ctx, cfg, "Uptime Kuma Monitor 🔴 Down", m.Beat.Msg, monitor, "trigger")
	}
	return "", nil
}

func g4SplunkMonitor(m Message) map[string]any {
	return map[string]any{
		"type":     m.Monitor.Type,
		"name":     m.Monitor.Name,
		"id":       m.Monitor.ID,
		"hostname": m.Monitor.Get("hostname"),
		"port":     m.Monitor.Get("port"),
		"url":      m.Monitor.Get("url"),
	}
}

func g4SplunkPost(ctx context.Context, cfg map[string]any, title, body string, monitor map[string]any, eventAction string) (string, error) {
	monitorType, _ := monitor["type"].(string)
	hostname, _ := monitor["hostname"].(string)
	port, _ := monitor["port"].(string)
	monitorURL, _ := monitor["url"].(string)

	switch {
	case monitorType == "port":
		monitorURL = hostname
		if port != "" {
			monitorURL += ":" + port
		}
	case hostname != "":
		monitorURL = hostname
	}

	if eventAction == "recovery" {
		autoResolve := cfgStr(cfg, "splunkAutoResolve")
		if autoResolve == "0" {
			return "No action required", nil
		}
		eventAction = autoResolve
	} else {
		eventAction = cfgStr(cfg, "splunkSeverity")
	}

	name, _ := monitor["name"].(string)

	payload := map[string]any{
		"message_type":        eventAction,
		"state_message":       fmt.Sprintf("[%s] [%s] %s", title, monitorURL, body),
		"entity_display_name": "Uptime Kuma Alert: " + name,
		"routing_key":         cfgStr(cfg, "pagerdutyIntegrationKey"),
		"entity_id":           "Uptime Kuma/" + g4AnyID(monitor["id"]),
	}

	code, _, err := postJSONBody(ctx, cfgStr(cfg, "splunkRestURL"), payload, map[string]string{
		"Content-Type": "application/json",
	})
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("Splunk notification failed with status code %d", code)
	}
	if text := http.StatusText(code); text != "" {
		return "Splunk notification succeed: " + text, nil
	}
	return "Sent Successfully.", nil
}

func g4AnyID(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// ---------------- OpenWA (WhatsApp) ----------------

var (
	g4OpenWATrailingSlash = regexp.MustCompile(`([^/])/+$`)
	g4TemplateVarRe       = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
)

func sendOpenWA(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	text := m.Msg
	if custom := cfgStr(cfg, "openwaCustomMessage"); cfgBool(cfg, "openwaUseCustomMessage") && strings.TrimSpace(custom) != "" {
		text = g4RenderTemplate(custom, m)
	}

	var chatIDs []string
	for _, id := range strings.Split(cfgStr(cfg, "openwaChatId"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			chatIDs = append(chatIDs, id)
		}
	}
	if len(chatIDs) == 0 {
		return "", fmt.Errorf("No valid OpenWA chat ID found.")
	}

	baseURL := g4OpenWATrailingSlash.ReplaceAllString(cfgStr(cfg, "openwaApiUrl"), "$1")
	session := url.PathEscape(cfgStr(cfg, "openwaSession"))
	target := baseURL + "/api/sessions/" + session + "/messages/send-text"

	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"X-Api-Key":    cfgStr(cfg, "openwaApiKey"),
	}
	for _, chatID := range chatIDs {
		payload := map[string]any{"chatId": chatID, "text": text}
		code, body, err := postJSONBody(ctx, target, payload, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("OpenWA", code, body); err != nil {
			return "", err
		}
	}
	return "Sent Successfully.", nil
}

// g4RenderTemplate 是官方 Liquid 模板的简化实现：
// 只替换官方文档列出的变量，其它 Liquid 语法原样保留。
// 官方支持：{{ msg }} {{ name }} {{ status }} {{ hostnameOrURL }}
// 以及 v1 兼容别名 {{ NAME }} {{ STATUS }} {{ HOSTNAME_OR_URL }}。
func g4RenderTemplate(tpl string, m Message) string {
	monitorName := "Monitor Name not available"
	hostnameOrURL := "testing.hostname"
	if m.Monitor != nil {
		monitorName = m.Monitor.Name
		hostnameOrURL = g4ExtractAddress(m.Monitor)
	}

	serviceStatus := "⚠️ Test"
	if m.Monitor != nil {
		if m.Status() == 0 {
			serviceStatus = "🔴 Down"
		} else {
			serviceStatus = "✅ Up"
		}
	}

	vals := map[string]string{
		"STATUS":          serviceStatus,
		"NAME":            monitorName,
		"HOSTNAME_OR_URL": hostnameOrURL,
		"status":          serviceStatus,
		"name":            monitorName,
		"hostnameOrURL":   hostnameOrURL,
		"msg":             m.Msg,
	}

	return g4TemplateVarRe.ReplaceAllStringFunc(tpl, func(match string) string {
		sub := g4TemplateVarRe.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		if v, ok := vals[sub[1]]; ok {
			return v
		}
		return match
	})
}

// g4ExtractAddress 对应官方 extractAddress：按监控类型取展示用地址。
func g4ExtractAddress(mo *model.Monitor) string {
	hostname := mo.Get("hostname")
	port := mo.Get("port")
	url := mo.Get("url")

	switch mo.Type {
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
		switch mo.Get("subtype") {
		case "ping", "dns":
			return hostname
		case "http":
			return url
		default:
			return ""
		}
	default:
		if url != "https://" && url != "http://" && url != "" {
			return url
		}
		return ""
	}
}

// ---------------- WPush ----------------

func sendWPush(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	title := "UptimeKuma Message"
	switch m.Status() {
	case 1:
		title = "UptimeKuma Monitor Up " + m.MonitorName()
	case 0:
		title = "UptimeKuma Monitor Down " + m.MonitorName()
	}

	payload := map[string]any{
		"title":   title,
		"content": m.Msg,
		"apikey":  cfgStr(cfg, "wpushAPIkey"),
		"channel": cfgStr(cfg, "wpushChannel"),
	}

	code, body, err := postJSONBody(ctx, "https://api.wpush.cn/api/v1/send", payload, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("WPush", code, body); err != nil {
		return "", err
	}

	var res struct {
		Code    *float64 `json:"code"`
		Message string   `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", fmt.Errorf("WPush: invalid response: %v", err)
	}
	if res.Code == nil || *res.Code != 0 {
		if res.Message != "" {
			return "", fmt.Errorf("WPush: %s", res.Message)
		}
		return "", fmt.Errorf("WPush: 响应缺少 code 字段")
	}
	return "Sent Successfully.", nil
}

// ---------------- SMS Planet ----------------

func sendSMSPlanet(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	payload := map[string]any{
		"from": cfgStr(cfg, "smsplanetSenderName"),
		"to":   cfgStr(cfg, "smsplanetPhoneNumbers"),
		"msg":  strings.Replace(m.Msg, "🔴", "❌", 1),
	}
	// 官方用 multipart/form-data 的 Content-Type 发 JSON 对象，这里保持一致
	code, body, err := postJSONBody(ctx, "https://api2.smsplanet.pl/sms", payload, map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "smsplanetApiToken"),
		"content-type":  "multipart/form-data",
	})
	if err != nil {
		return "", err
	}
	if err := expectOK("SMSPlanet", code, body); err != nil {
		return "", err
	}

	var res struct {
		MessageID any    `json:"messageId"`
		ErrorMsg  string `json:"errorMsg"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", fmt.Errorf("SMSPlanet server did not respond with the expected result")
	}
	if g4Falsy(res.MessageID) {
		if res.ErrorMsg != "" {
			return "", fmt.Errorf("SMSPlanet: %s", res.ErrorMsg)
		}
		return "", fmt.Errorf("SMSPlanet server did not respond with the expected result")
	}
	return "Sent Successfully.", nil
}

// g4Falsy 判断 JS 里为 false 的值（null/""/0/false），用于 `!value` 这类判断。
func g4Falsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	default:
		return false
	}
}
