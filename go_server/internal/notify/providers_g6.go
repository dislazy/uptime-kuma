package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// 本文件移植 g6 批次官方渠道：Bitrix24 / smsir / gorush / Kook / octopush /
// OpsiMate / plivo / serwersms / SMSPartner / telnyx / ZohoCliq / waha /
// TurboSMTP / max。
//
// 所有包级标识符都带渠道前缀（或 g6 前缀），避免与其它 provider 文件重名。

func init() {
	registerFunc("Bitrix24", sendBitrix24)
	registerFunc("smsir", sendSMSIR)
	registerFunc("gorush", sendGorush)
	registerFunc("Kook", sendKook)
	registerFunc("octopush", sendOctopush)
	registerFunc("OpsiMate", sendOpsiMate)
	registerFunc("plivo", sendPlivo)
	registerFunc("serwersms", sendSerwerSMS)
	registerFunc("SMSPartner", sendSMSPartner)
	registerFunc("telnyx", sendTelnyx)
	registerFunc("ZohoCliq", sendZohoCliq)
	registerFunc("waha", sendWaha)
	registerFunc("TurboSMTP", sendTurboSMTP)
	registerFunc("max", sendMax)
}

// ---------------- 通用小工具 ----------------

// g6StripNonASCII 对应官方多个渠道里的 msg.replace(/[^\x00-\x7F]/g, "")。
func g6StripNonASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r > 0x7F {
			return -1
		}
		return r
	}, s)
}

// g6RemoveSpaces 对应 msg.replace(/\s/g, "")。
func g6RemoveSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// g6TruncateRunes 按字符截断，等价于 JS 的 substring(0, n)（够用即可）。
func g6TruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// g6ExtractAddress 复刻基类的 extractAddress(monitorJSON)。
func g6ExtractAddress(mon *model.Monitor) string {
	if mon == nil {
		return ""
	}
	switch mon.Get("type") {
	case "push":
		return "Heartbeat"
	case "ping", "dns":
		return mon.Hostname()
	case "port", "gamedig", "steam":
		if p := mon.Port(); p > 0 {
			return fmt.Sprintf("%s:%d", mon.Hostname(), p)
		}
		return mon.Hostname()
	case "globalping":
		switch mon.Get("subtype") {
		case "ping", "dns":
			return mon.Hostname()
		case "http":
			return mon.URL()
		default:
			return ""
		}
	default:
		if u := mon.URL(); u != "https://" && u != "http://" && u != "" {
			return u
		}
		return ""
	}
}

// g6MonitorJSON 把 model.Monitor 还原成官方 monitorJSON 的近似结构。
func g6MonitorJSON(mon *model.Monitor) any {
	if mon == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range mon.Raw {
		out[k] = v
	}
	out["id"] = mon.ID
	out["name"] = mon.Name
	out["type"] = mon.Type
	out["url"] = mon.URL()
	out["hostname"] = mon.Hostname()
	return out
}

// ---------------- Bitrix24 ----------------

func sendBitrix24(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	webhook, err := requireCfg(cfg, "bitrix24WebhookURL", "Bitrix24 Webhook URL")
	if err != nil {
		return "", err
	}
	userID, err := requireCfg(cfg, "bitrix24UserID", "User ID")
	if err != nil {
		return "", err
	}

	// 官方把颜色写反了（UP 用红色），这里保持一致以免用户既有习惯被破坏。
	color := "#67b518"
	if m.Status() == model.StatusUp {
		color = "#b73419"
	}

	q := url.Values{}
	q.Set("user_id", userID)
	q.Set("message", "[B]Uptime Kuma[/B]")
	q.Set("ATTACH[COLOR]", color)
	q.Set("ATTACH[BLOCKS][0][MESSAGE]", m.Msg)

	target := webhook + "/im.notify.system.add.json?" + q.Encode()
	return getOK(ctx, "Bitrix24", target, nil)
}

// ---------------- smsir ----------------

// smsirNormalizeMobile 把 09xxxxxxxxx 转成 9xxxxxxxxx（SMSIR 只收不带前导 0 的号码）。
func smsirNormalizeMobile(mobile string) string {
	if len(mobile) != 11 || !strings.HasPrefix(mobile, "09") {
		return mobile
	}
	for _, r := range mobile {
		if r < '0' || r > '9' {
			return mobile
		}
	}
	return mobile[1:]
}

// smsirShorten 复刻官方的 20 字符压缩逻辑：先删空白，再硬截断。
func smsirShorten(msg string) string {
	const maxLen = 20
	if len([]rune(msg)) > maxLen {
		msg = g6RemoveSpaces(msg)
	}
	if len([]rune(msg)) > maxLen {
		// 官方 substring(0, MAX-1-3) 后接 "..."
		msg = g6TruncateRunes(msg, maxLen-1-3) + "..."
	}
	return msg
}

func sendSMSIR(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "smsirApiKey", "API Key")
	if err != nil {
		return "", err
	}
	numbers, err := requireCfg(cfg, "smsirNumber", "Recipient Numbers")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(cfgStr(cfg, "smsirTemplate")) == "" {
		return "", fmt.Errorf("Template ID is required")
	}
	templateID := cfgInt(cfg, "smsirTemplate")

	text := smsirShorten(m.Msg)
	headers := map[string]string{
		"Accept":    "application/json",
		"X-API-Key": apiKey,
	}
	// 官方用 Promise.all 并发发送，失败任一即失败；这里顺序发送，语义一致。
	for _, mobile := range strings.Split(numbers, ",") {
		payload := map[string]any{
			"mobile":     smsirNormalizeMobile(mobile),
			"templateId": templateID,
			"parameters": []map[string]any{
				{"name": "uptkumaalert", "value": text},
			},
		}
		code, body, err := postJSONBody(ctx, "https://api.sms.ir/v1/send/verify", payload, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("smsir", code, body); err != nil {
			return "", err
		}
	}
	return "Sent Successfully.", nil
}

// ---------------- gorush ----------------

func sendGorush(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	serverURL, err := requireCfg(cfg, "gorushServerURL", "Server URL")
	if err != nil {
		return "", err
	}
	token, err := requireCfg(cfg, "gorushDeviceToken", "Device Token")
	if err != nil {
		return "", err
	}

	platformMapping := map[string]int64{
		"ios":     1,
		"android": 2,
		"huawei":  3,
	}

	note := map[string]any{
		"tokens":  []string{token},
		"message": m.Msg,
		"retry":   cfgInt(cfg, "gorushRetry"),
	}
	// 官方直接写 platform: platformMapping[x]，未命中时值为 undefined，JSON 序列化会丢掉该键。
	if platform, ok := platformMapping[cfgStr(cfg, "gorushPlatform")]; ok {
		note["platform"] = platform
	}
	if v := cfgStr(cfg, "gorushTitle"); v != "" {
		note["title"] = v
	}
	if v := cfgStr(cfg, "gorushPriority"); v != "" {
		note["priority"] = v
	}
	if v := cfgStr(cfg, "gorushTopic"); v != "" {
		note["topic"] = v
	}

	payload := map[string]any{"notifications": []any{note}}
	return postJSONOK(ctx, "gorush", serverURL+"/api/push", payload, nil)
}

// ---------------- Kook ----------------

func sendKook(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	token, err := requireCfg(cfg, "kookBotToken", "Bot Token")
	if err != nil {
		return "", err
	}
	guildID, err := requireCfg(cfg, "kookGuildID", "Guild ID")
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"target_id": guildID,
		"content":   m.Msg,
	}
	headers := map[string]string{
		"Authorization": "Bot " + token,
	}
	return postJSONOK(ctx, "Kook", "https://www.kookapp.cn/api/v3/message/create", payload, headers)
}

// ---------------- octopush ----------------

func sendOctopush(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	version := strings.TrimSpace(cfgStr(cfg, "octopushVersion"))
	switch version {
	case "", "2":
		return sendOctopushV2(ctx, cfg, m)
	case "1":
		return sendOctopushV1(ctx, cfg, m)
	default:
		return "", fmt.Errorf("Unknown Octopush version!")
	}
}

func sendOctopushV2(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "octopushAPIKey", "API Key")
	if err != nil {
		return "", err
	}
	login, err := requireCfg(cfg, "octopushLogin", "Login")
	if err != nil {
		return "", err
	}
	phone, err := requireCfg(cfg, "octopushPhoneNumber", "Phone Number")
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"recipients": []map[string]any{
			{"phone_number": phone},
		},
		// octopush 不支持非 ASCII 字符
		"text":    g6StripNonASCII(m.Msg),
		"type":    cfgStr(cfg, "octopushSMSType"),
		"purpose": "alert",
		"sender":  cfgStr(cfg, "octopushSenderName"),
	}
	headers := map[string]string{
		"api-key":       apiKey,
		"api-login":     login,
		"cache-control": "no-cache",
	}
	return postJSONOK(ctx, "octopush", "https://api.octopush.com/v1/public/sms-campaign/send", payload, headers)
}

func sendOctopushV1(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	login, err := requireCfg(cfg, "octopushDMLogin", "Login")
	if err != nil {
		return "", err
	}
	apiKey, err := requireCfg(cfg, "octopushDMAPIKey", "API Key")
	if err != nil {
		return "", err
	}
	phone, err := requireCfg(cfg, "octopushDMPhoneNumber", "Phone Number")
	if err != nil {
		return "", err
	}

	smsType := "XXX"
	if cfgStr(cfg, "octopushDMSMSType") == "sms_premium" {
		smsType = "FR"
	}

	params := url.Values{}
	params.Set("user_login", login)
	params.Set("api_key", apiKey)
	params.Set("sms_recipients", phone)
	params.Set("sms_sender", cfgStr(cfg, "octopushDMSenderName"))
	params.Set("sms_type", smsType)
	params.Set("transactional", "1")
	params.Set("sms_text", g6StripNonASCII(m.Msg))

	target := "https://www.octopush-dm.com/api/sms/json?" + params.Encode()
	code, body, err := postJSONBody(ctx, target, map[string]any{}, map[string]string{"cache-control": "no-cache"})
	if err != nil {
		return "", err
	}
	if err := expectOK("octopush", code, body); err != nil {
		return "", err
	}

	// V1 接口即使出错也返回 200，必须看响应体里的 error_code。
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("Octopush error %s", strings.TrimSpace(body))
	}
	if codeVal, ok := resp["error_code"]; ok {
		if cfgValueString(codeVal) != "000" {
			raw, _ := json.Marshal(resp)
			return "", fmt.Errorf("Octopush error %s", string(raw))
		}
	}
	return "Sent Successfully.", nil
}

// ---------------- OpsiMate ----------------

func sendOpsiMate(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	base, err := requireCfg(cfg, "opsimateURL", "OpsiMate URL")
	if err != nil {
		return "", err
	}
	token, err := requireCfg(cfg, "opsimateAPIToken", "API Token")
	if err != nil {
		return "", err
	}

	// 官方只去掉一个结尾的 "/"
	if strings.HasSuffix(base, "/") {
		base = base[:len(base)-1]
	}

	payload := map[string]any{
		"heartbeat": opsimateHeartbeat(m),
		"monitor":   g6MonitorJSON(m.Monitor),
		"msg":       m.Msg,
	}
	headers := map[string]string{"x-api-token": token}

	return postJSONOK(ctx, "OpsiMate", base+"/api/v1/alerts/custom/uptimekuma", payload, headers)
}

// opsimateHeartbeat 对应官方透传的 heartbeatJSON；测试消息为 null。
func opsimateHeartbeat(m Message) any {
	if m.Monitor == nil {
		return nil
	}
	return m.Beat.JSON()
}

// ---------------- plivo ----------------

func sendPlivo(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	authID, err := requireCfg(cfg, "plivoAuthID", "plivoAuthID")
	if err != nil {
		return "", err
	}
	authToken, err := requireCfg(cfg, "plivoAuthToken", "plivoAuthToken")
	if err != nil {
		return "", err
	}
	from, err := requireCfg(cfg, "plivoFromNumber", "plivoFromNumber")
	if err != nil {
		return "", err
	}
	to, err := requireCfg(cfg, "plivoToNumber", "plivoToNumber")
	if err != nil {
		return "", err
	}

	credentials := base64.StdEncoding.EncodeToString([]byte(authID + ":" + authToken))
	headers := map[string]string{"Authorization": "Basic " + credentials}
	baseURL := "https://api.plivo.com/v1/Account/" + authID

	if cfgStr(cfg, "plivoMessageType") == "call" {
		answerURL, err := requireCfg(cfg, "plivoAnswerUrl", "plivoAnswerUrl")
		if err != nil {
			return "", err
		}
		u, err := url.Parse(answerURL)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("message", m.Msg)
		u.RawQuery = q.Encode()

		payload := map[string]any{
			"from":          from,
			"to":            to,
			"answer_url":    u.String(),
			"answer_method": "GET",
		}
		return postJSONOK(ctx, "plivo", baseURL+"/Call/", payload, headers)
	}

	payload := map[string]any{
		"src":  from,
		"dst":  to,
		"text": m.Msg,
	}
	return postJSONOK(ctx, "plivo", baseURL+"/Message/", payload, headers)
}

// ---------------- SerwerSMS ----------------

func sendSerwerSMS(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	username, err := requireCfg(cfg, "serwersmsUsername", "Username")
	if err != nil {
		return "", err
	}
	password, err := requireCfg(cfg, "serwersmsPassword", "Password")
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"username": username,
		"password": password,
		"text":     g6StripNonASCII(m.Msg),
		"sender":   cfgStr(cfg, "serwersmsSenderName"),
	}

	if cfgStr(cfg, "serwersmsRecipientType") == "group" {
		groupID, err := requireCfg(cfg, "serwersmsGroupId", "Group ID")
		if err != nil {
			return "", err
		}
		data["group_id"] = groupID
	} else {
		phone, err := requireCfg(cfg, "serwersmsPhoneNumber", "Phone Number")
		if err != nil {
			return "", err
		}
		data["phone"] = phone
	}

	code, body, err := postJSONBody(ctx, "https://api2.serwersms.pl/messages/send_sms", data, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("SerwerSMS", code, body); err != nil {
		return "", err
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", err
	}
	if !cfgBool(resp, "success") {
		if errObj, ok := resp["error"].(map[string]any); ok {
			return "", fmt.Errorf(
				"SerwerSMS.pl API returned error code %s (%s) with error message: %s",
				cfgValueString(errObj["code"]),
				cfgValueString(errObj["type"]),
				cfgValueString(errObj["message"]),
			)
		}
		return "", fmt.Errorf("SerwerSMS.pl API returned an unexpected response")
	}
	return "Sent Successfully.", nil
}

// ---------------- SMSPartner ----------------

func sendSMSPartner(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "smspartnerApikey", "API Key")
	if err != nil {
		return "", err
	}
	phoneNumbers, err := requireCfg(cfg, "smspartnerPhoneNumber", "Phone Numbers")
	if err != nil {
		return "", err
	}

	// smspartner 不支持非 ASCII，且最长 639 字符
	cleanMsg := g6TruncateRunes(g6StripNonASCII(m.Msg), 639)
	sender := g6TruncateRunes(cfgStr(cfg, "smspartnerSenderName"), 11)

	payload := map[string]any{
		"apiKey":       apiKey,
		"sender":       sender,
		"phoneNumbers": phoneNumbers,
		"message":      cleanMsg,
	}
	headers := map[string]string{
		"cache-control": "no-cache",
		"Accept":        "application/json",
	}

	code, body, err := postJSONBody(ctx, "https://api.smspartner.fr/v1/send", payload, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("SMSPartner", code, body); err != nil {
		return "", err
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", err
	}
	if success, ok := resp["success"].(bool); !ok || !success {
		// 官方文案：`Api returned ${resp.data.response.status}.`
		status := "undefined"
		if respObj, ok := resp["response"].(map[string]any); ok {
			if v, ok := respObj["status"]; ok {
				status = cfgValueString(v)
			}
		}
		return "", fmt.Errorf("Api returned %s.", status)
	}
	return "Sent Successfully.", nil
}

// ---------------- Telnyx ----------------

func sendTelnyx(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey, err := requireCfg(cfg, "telnyxApiKey", "telnyxApiKey")
	if err != nil {
		return "", err
	}
	from, err := requireCfg(cfg, "telnyxPhoneNumber", "telnyxPhoneNumber")
	if err != nil {
		return "", err
	}
	to, err := requireCfg(cfg, "telnyxToNumber", "telnyxToNumber")
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"from": from,
		"to":   to,
		"text": m.Msg,
	}
	if profileID := cfgStr(cfg, "telnyxMessagingProfileId"); profileID != "" {
		payload["messaging_profile_id"] = profileID
	}

	headers := map[string]string{"Authorization": "Bearer " + apiKey}
	return postJSONOK(ctx, "telnyx", "https://api.telnyx.com/v2/messages", payload, headers)
}

// ---------------- ZohoCliq ----------------

// zohoCliqStatusMessage 复刻 _statusMessageFactory。
func zohoCliqStatusMessage(status int, monitorName string) string {
	switch status {
	case model.StatusDown:
		return fmt.Sprintf("🔴 [%s] went down\n", monitorName)
	case model.StatusUp:
		return fmt.Sprintf("### ✅ [%s] is back online\n", monitorName)
	default:
		return "Notification\n"
	}
}

func sendZohoCliq(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	webhookURL, err := requireCfg(cfg, "webhookUrl", "Webhook URL")
	if err != nil {
		return "", err
	}

	payload := make([]string, 0, 3)
	if m.Monitor == nil {
		// 测试消息：无状态、无 URL
		payload = append(payload, zohoCliqStatusMessage(m.Status(), ""), "*Description:* "+m.Msg)
	} else {
		payload = append(payload, zohoCliqStatusMessage(m.Status(), m.MonitorName()), "*Description:* "+m.Beat.Msg)
		if monitorURL := g6ExtractAddress(m.Monitor); monitorURL != "" && monitorURL != "https://" {
			payload = append(payload, "*URL:* "+monitorURL)
		}
	}

	body := map[string]any{"text": strings.Join(payload, "\n")}
	return postJSONOK(ctx, "ZohoCliq", webhookURL, body, nil)
}

// ---------------- waha ----------------

func sendWaha(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiURL, err := requireCfg(cfg, "wahaApiUrl", "API URL")
	if err != nil {
		return "", err
	}
	apiKey, err := requireCfg(cfg, "wahaApiKey", "API Key")
	if err != nil {
		return "", err
	}
	chatID, err := requireCfg(cfg, "wahaChatId", "wahaChatId")
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"session": cfgStr(cfg, "wahaSession"),
		"chatId":  chatID,
		"text":    m.Msg,
	}
	headers := map[string]string{
		"Accept":    "application/json",
		"X-Api-Key": apiKey,
	}

	// 官方用 /([^/])\/+$/ 去掉结尾多余斜杠
	target := strings.TrimRight(apiURL, "/") + "/api/sendText"
	return postJSONOK(ctx, "waha", target, payload, headers)
}

// ---------------- TurboSMTP ----------------

func sendTurboSMTP(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	consumerKey, err := requireCfg(cfg, "turbosmtpConsumerKey", "turbosmtpConsumerKey")
	if err != nil {
		return "", err
	}
	consumerSecret, err := requireCfg(cfg, "turbosmtpConsumerSecret", "turbosmtpConsumerSecret")
	if err != nil {
		return "", err
	}
	from, err := requireCfg(cfg, "turbosmtpFromEmail", "From Email")
	if err != nil {
		return "", err
	}
	to, err := requireCfg(cfg, "turbosmtpToEmail", "To Email")
	if err != nil {
		return "", err
	}

	host := "api.turbo-smtp.com"
	if cfgStr(cfg, "turbosmtpRegion") == "eu" {
		host = "api.eu.turbo-smtp.com"
	}

	subject := cfgStr(cfg, "turbosmtpSubject")
	if subject == "" {
		subject = "Notification from Your Uptime Kuma"
	}

	payload := map[string]any{
		"from":    strings.TrimSpace(from),
		"to":      to,
		"subject": subject,
		"content": m.Msg,
	}

	// 逗号分隔列表，逐个 trim 后重新拼接
	if cc := cfgStr(cfg, "turbosmtpCcEmail"); cc != "" {
		parts := strings.Split(cc, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		payload["cc"] = strings.Join(parts, ",")
	}
	if bcc := cfgStr(cfg, "turbosmtpBccEmail"); bcc != "" {
		parts := strings.Split(bcc, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		payload["bcc"] = strings.Join(parts, ",")
	}

	headers := map[string]string{
		"consumerKey":    consumerKey,
		"consumerSecret": consumerSecret,
	}
	return postJSONOK(ctx, "TurboSMTP", "https://"+host+"/api/v2/mail/send", payload, headers)
}

// ---------------- max ----------------

// maxTemplateVarRe 匹配 Liquid 的 {{ ... }} 输出标签。
var maxTemplateVarRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// maxRenderTemplate 是官方 Liquid 渲染的精简替代：
// 支持 {{ status }} / {{ name }} / {{ hostnameOrURL }} / {{ msg }}、
// 大写兼容别名，以及 monitorJSON.xxx / heartbeatJSON.xxx 的简单路径取值。
// 不支持 Liquid 的过滤器与条件语句（超出本包能力，按原样降级）。
func maxRenderTemplate(tpl string, m Message) string {
	monitorName := "Monitor Name not available"
	hostOrURL := "testing.hostname"
	status := "⚠️ Test"

	if m.Monitor != nil {
		monitorName = m.MonitorName()
		hostOrURL = g6ExtractAddress(m.Monitor)
		if m.Status() == model.StatusDown {
			status = "🔴 Down"
		} else {
			status = "✅ Up"
		}
	}

	ctx := map[string]any{
		"STATUS":          status,
		"NAME":            monitorName,
		"HOSTNAME_OR_URL": hostOrURL,
		"status":          status,
		"name":            monitorName,
		"hostnameOrURL":   hostOrURL,
		"monitorJSON":     g6MonitorJSON(m.Monitor),
		"msg":             m.Msg,
	}
	if m.Monitor != nil {
		ctx["heartbeatJSON"] = m.Beat.JSON()
	} else {
		ctx["heartbeatJSON"] = nil
	}

	return maxTemplateVarRe.ReplaceAllStringFunc(tpl, func(token string) string {
		sub := maxTemplateVarRe.FindStringSubmatch(token)
		if len(sub) < 2 {
			return ""
		}
		expr := strings.TrimSpace(sub[1])
		// 过滤器语法暂不支持，取管道前的变量名
		if i := strings.Index(expr, "|"); i >= 0 {
			expr = strings.TrimSpace(expr[:i])
		}
		return maxTemplateLookup(expr, ctx)
	})
}

// maxTemplateLookup 按点号路径从渲染上下文取值，未命中返回空串（与 Liquid 一致）。
func maxTemplateLookup(expr string, root map[string]any) string {
	var cur any = root
	for _, part := range strings.Split(expr, ".") {
		part = strings.TrimSpace(part)
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[part]
		if !ok {
			return ""
		}
	}
	if cur == nil {
		return ""
	}
	return cfgValueString(cur)
}

func sendMax(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	token, err := requireCfg(cfg, "maxBotToken", "Bot Token")
	if err != nil {
		return "", err
	}
	chatID, err := requireCfg(cfg, "maxChatID", "Chat ID")
	if err != nil {
		return "", err
	}

	baseURL := strings.TrimSpace(cfgStr(cfg, "maxApiUrl"))
	if baseURL == "" {
		baseURL = "https://platform-api.max.ru"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	body := map[string]any{"text": m.Msg}
	if cfgBool(cfg, "maxUseTemplate") && cfgStr(cfg, "maxTemplate") != "" {
		body["text"] = maxRenderTemplate(cfgStr(cfg, "maxTemplate"), m)
		if format := cfgStr(cfg, "maxTemplateFormat"); format != "" && format != "plain" {
			body["format"] = format
		}
	}

	// 对应 JS 的 encodeURIComponent(chatId)
	target := baseURL + "/messages?chat_id=" + strings.ReplaceAll(url.QueryEscape(chatID), "+", "%20")
	headers := map[string]string{"Authorization": token}

	return postJSONOK(ctx, "max", target, body, headers)
}
