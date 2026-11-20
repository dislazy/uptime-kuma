package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// 本文件移植 15 个官方通知渠道：
// apprise / CallMeBot / egosms / Indigo / nextcloudtalk / Onesender / Flowtriq /
// pushbullet / SIGNL4 / MicrosoftGraph / twilio / evolution / Cellsynt / YZJ / Milky
//
// 字段名、URL、请求体、header、成功文案严格按官方 notification-providers/*.js。
// 所有包级标识符统一带 g3 前缀，避免与其它文件重名。

func init() {
	registerFunc("apprise", sendApprise)
	registerFunc("CallMeBot", sendCallMeBot)
	registerFunc("egosms", sendEgoSMS)
	registerFunc("Indigo", sendIndigo)
	registerFunc("nextcloudtalk", sendNextcloudTalk)
	registerFunc("Onesender", sendOnesender)
	registerFunc("Flowtriq", sendFlowtriq)
	registerFunc("pushbullet", sendPushbullet)
	registerFunc("SIGNL4", sendSignl4)
	registerFunc("MicrosoftGraph", sendMicrosoftGraph)
	registerFunc("twilio", sendTwilio)
	registerFunc("evolution", sendEvolution)
	registerFunc("Cellsynt", sendCellsynt)
	registerFunc("YZJ", sendYZJ)
	registerFunc("Milky", sendMilky)
}

// ---------------- 公共小工具 ----------------

// g3Timezone 官方用 setting 里的 serverTimezone 渲染 "Time (...)"。
// notify 包拿不到 settings，Beat.Time 本身已按 UTC 存储，这里固定 UTC。
const g3Timezone = "UTC"

// g3LocalDateTime 对应官方 heartbeatJSON.localDateTime。
func g3LocalDateTime(m Message) string {
	if m.Beat.Time != "" {
		return m.Beat.Time
	}
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

// g3HeartbeatJSON 把 Beat 还原成官方 heartbeatJSON 的形状。
// 测试消息没有心跳，返回 nil（序列化成 JSON null，与官方一致）。
func g3HeartbeatJSON(m Message) map[string]any {
	if m.Monitor == nil {
		return nil
	}
	hb := map[string]any{
		"monitorID":     m.Beat.MonitorID,
		"status":        m.Beat.Status,
		"msg":           m.Beat.Msg,
		"time":          m.Beat.Time,
		"timezone":      g3Timezone,
		"localDateTime": g3LocalDateTime(m),
	}
	if m.Beat.Ping != nil {
		hb["ping"] = *m.Beat.Ping
	}
	return hb
}

// g3MonitorJSON 把 model.Monitor 还原成官方 monitorJSON 的常用字段。
func g3MonitorJSON(m Message) map[string]any {
	if m.Monitor == nil {
		return nil
	}
	out := map[string]any{
		"id":       m.Monitor.ID,
		"name":     m.Monitor.Name,
		"type":     m.Monitor.Type,
		"url":      m.Monitor.URL(),
		"hostname": m.Monitor.Hostname(),
	}
	if p := m.Monitor.Port(); p != 0 {
		out["port"] = p
	}
	if sub := m.Monitor.Get("subtype"); sub != "" {
		out["subtype"] = sub
	}
	return out
}

// g3ExtractAddress 对应基类的 extractAddress()。
func g3ExtractAddress(m Message) string {
	if m.Monitor == nil {
		return ""
	}
	switch m.Monitor.Type {
	case "push":
		return "Heartbeat"
	case "ping", "dns":
		return m.Monitor.Hostname()
	case "port", "gamedig", "steam":
		if p := m.Monitor.Port(); p != 0 {
			return fmt.Sprintf("%s:%d", m.Monitor.Hostname(), p)
		}
		return m.Monitor.Hostname()
	case "globalping":
		switch m.Monitor.Get("subtype") {
		case "ping", "dns":
			return m.Monitor.Hostname()
		case "http":
			return m.Monitor.URL()
		default:
			return ""
		}
	default:
		u := m.Monitor.URL()
		if u != "" && u != "https://" && u != "http://" {
			return u
		}
		return ""
	}
}

// g3TemplateVar 匹配 {{ name }} 这类占位符。
var g3TemplateVar = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// g3RenderTemplate 是官方 Liquid renderTemplate 的降级版本：
// 只支持 {{msg}} / {{name}} / {{NAME}} / {{status}} / {{STATUS}} /
// {{hostnameOrURL}} / {{HOSTNAME_OR_URL}} 这些顶层变量。
// 官方支持的 {{monitorJSON.xxx}} 之类对象取值无法在不引入模板引擎的情况下复刻，
// 遇到未知占位符时原样保留（比官方渲染成空串更不容易丢信息）。
func g3RenderTemplate(tpl string, m Message) string {
	status := "⚠️ Test"
	name := "Monitor Name not available"
	addr := "testing.hostname"
	if m.Monitor != nil {
		if m.Status() == model.StatusDown {
			status = "🔴 Down"
		} else {
			status = "✅ Up"
		}
		name = m.Monitor.Name
		addr = g3ExtractAddress(m)
	}

	vars := map[string]string{
		"STATUS":          status,
		"status":          status,
		"NAME":            name,
		"name":            name,
		"HOSTNAME_OR_URL": addr,
		"hostnameOrURL":   addr,
		"msg":             m.Msg,
	}

	return g3TemplateVar.ReplaceAllStringFunc(tpl, func(tok string) string {
		sub := g3TemplateVar.FindStringSubmatch(tok)
		if len(sub) < 2 {
			return tok
		}
		if v, ok := vars[sub[1]]; ok {
			return v
		}
		return tok
	})
}

// g3PostJSONTLS 与 postJSONBody 相同，但允许跳过 TLS 校验（Indigo 局域网自签证书）。
func g3PostJSONTLS(ctx context.Context, insecure bool, target string, payload any, headers map[string]string) (int, string, error) {
	if !insecure {
		return postJSONBody(ctx, target, payload, headers)
	}

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

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 官方 indigoIgnoreTlsError 语义
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()

	var body bytes.Buffer
	_, _ = body.ReadFrom(res.Body)
	text := body.String()
	if len(text) > 4096 {
		text = text[:4096]
	}
	return res.StatusCode, text, nil
}

// ---------------- apprise ----------------

// sendApprise 调用本机 apprise CLI（官方同样是 execFile("apprise", ...)）。
func sendApprise(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	args := []string{"-vv", "-b", m.Msg}
	if title := cfgStr(cfg, "title"); title != "" {
		args = append(args, "-t", title)
	}
	args = append(args, "--", cfgStr(cfg, "appriseURL"))

	cmd := exec.CommandContext(ctx, "apprise", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// 找不到二进制 / 非零退出：官方 execFileAsync 会直接 reject
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s", detail)
	}

	// 退出码 0 时输出可能为空，不能当成失败（官方 #5547）
	if out := stdout.String(); strings.Contains(out, "ERROR") {
		return "", fmt.Errorf("%s", out)
	}
	return "Sent Successfully.", nil
}

// ---------------- CallMeBot ----------------

func sendCallMeBot(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	u, err := url.Parse(cfgStr(cfg, "callMeBotEndpoint"))
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("text", m.Msg)
	u.RawQuery = q.Encode()

	return getOK(ctx, "CallMeBot", u.String(), nil)
}

// ---------------- egosms ----------------

func sendEgoSMS(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	sender := cfgStr(cfg, "egosmsSender")
	if sender == "" {
		sender = "EGOSMS"
	}

	form := url.Values{}
	form.Set("number", cfgStr(cfg, "egosmsPhoneNumber"))
	form.Set("message", m.Msg)
	form.Set("username", cfgStr(cfg, "egosmsUsername"))
	form.Set("password", cfgStr(cfg, "egosmsPassword"))
	form.Set("sender", sender)
	form.Set("priority", "0")

	target := "https://www.egosms.co/api/v1/plain/?" + form.Encode()
	code, body, err := getURL(ctx, target, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("egosms", code, body); err != nil {
		return "", err
	}
	// 官方 return response.data || okMsg
	if text := strings.TrimSpace(body); text != "" {
		return text, nil
	}
	return "Sent Successfully.", nil
}

// ---------------- Indigo ----------------

func sendIndigo(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(cfgStr(cfg, "indigoUrl")), "/")
	target := base + "/v2/api/command"

	variableID := cfgInt(cfg, "indigoVariableId")
	actionGroupID := cfgInt(cfg, "indigoActionGroupId")
	if variableID == 0 && actionGroupID == 0 {
		return "", fmt.Errorf("Set an Indigo variable ID, an action group ID, or both.")
	}

	// 变量先写，动作组才能读到
	type indigoCommand struct {
		message    string
		objectID   int64
		parameters map[string]any
	}
	commands := make([]indigoCommand, 0, 2)
	if variableID != 0 {
		commands = append(commands, indigoCommand{
			message:    "indigo.variable.updateValue",
			objectID:   variableID,
			parameters: map[string]any{"value": m.Msg},
		})
	}
	if actionGroupID != 0 {
		commands = append(commands, indigoCommand{
			message:  "indigo.actionGroup.execute",
			objectID: actionGroupID,
		})
	}

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "indigoApiKey"),
		"Content-Type":  "application/json",
	}
	insecure := cfgBool(cfg, "indigoIgnoreTlsError")

	for _, c := range commands {
		payload := map[string]any{
			"id":       "uptime-kuma",
			"message":  c.message,
			"objectId": c.objectID,
		}
		if c.parameters != nil {
			payload["parameters"] = c.parameters
		}

		code, body, err := g3PostJSONTLS(ctx, insecure, target, payload, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("Indigo", code, body); err != nil {
			return "", err
		}

		var res struct {
			Error            string `json:"error"`
			ValidationErrors any    `json:"validationErrors"`
		}
		_ = json.Unmarshal([]byte(body), &res)
		if res.Error != "" || res.ValidationErrors != nil {
			detail := res.Error
			if detail == "" {
				b, _ := json.Marshal(res.ValidationErrors)
				detail = string(b)
			}
			return "", fmt.Errorf("Indigo rejected %s: %s", c.message, detail)
		}
	}

	return "Sent Successfully.", nil
}

// ---------------- nextcloudtalk ----------------

func sendNextcloudTalk(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	// 官方：randomBytes(64).toString("hex").slice(0, 64)，即 32 字节的 hex
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	talkRandom := hex.EncodeToString(buf)

	mac := hmac.New(sha256.New, []byte(cfgStr(cfg, "botSecret")))
	mac.Write([]byte(talkRandom + m.Msg))
	talkSignature := hex.EncodeToString(mac.Sum(nil))

	silentUp := m.Status() == model.StatusUp && cfgBool(cfg, "sendSilentUp")
	silentDown := m.Status() == model.StatusDown && cfgBool(cfg, "sendSilentDown")

	target := strings.TrimRight(cfgStr(cfg, "host"), "/") +
		"/ocs/v2.php/apps/spreed/api/v1/bot/" +
		url.PathEscape(cfgStr(cfg, "conversationToken")) + "/message"

	payload := map[string]any{
		"message": m.Msg,
		"silent":  silentUp || silentDown,
	}
	headers := map[string]string{
		"X-Nextcloud-Talk-Bot-Random":    talkRandom,
		"X-Nextcloud-Talk-Bot-Signature": talkSignature,
		"OCS-APIRequest":                 "true",
	}

	code, body, err := postJSONBody(ctx, target, payload, headers)
	if err != nil {
		return "", err
	}
	if code == http.StatusCreated {
		return "Sent Successfully.", nil
	}
	if snippet := strings.TrimSpace(body); snippet != "" {
		return "", fmt.Errorf("Nextcloud Talk Error %d: %s", code, snippet)
	}
	return "", fmt.Errorf("Nextcloud Talk Error %d", code)
}

// ---------------- Onesender ----------------

func sendOnesender(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	recipient := cfgStr(cfg, "onesenderReceiver")

	payload := map[string]any{
		"heartbeat":      g3HeartbeatJSON(m),
		"monitor":        g3MonitorJSON(m),
		"msg":            m.Msg,
		"to":             recipient,
		"type":           "text",
		"recipient_type": "individual",
		"text":           map[string]any{"body": m.Msg},
	}
	if cfgStr(cfg, "onesenderTypeReceiver") == "private" {
		payload["to"] = recipient + "@s.whatsapp.net"
	} else {
		payload["recipient_type"] = "group"
		payload["to"] = recipient + "@g.us"
	}

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "onesenderToken"),
	}
	return postJSONOK(ctx, "Onesender", cfgStr(cfg, "onesenderURL"), payload, headers)
}

// ---------------- Flowtriq ----------------

func sendFlowtriq(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	status := "info"
	monitorName := "Unknown"
	if m.Monitor != nil {
		monitorName = m.Monitor.Name
	}
	switch m.Status() {
	case model.StatusDown:
		status = "down"
	case model.StatusUp:
		status = "up"
	}

	payload := map[string]any{
		"source":  "uptime-kuma",
		"status":  status,
		"monitor": monitorName,
		"msg":     m.Msg,
	}
	if m.Monitor != nil {
		beatInfo := map[string]any{
			"status":   m.Beat.Status,
			"time":     m.Beat.Time,
			"msg":      m.Beat.Msg,
			"timezone": g3Timezone,
		}
		if m.Beat.Ping != nil {
			beatInfo["ping"] = *m.Beat.Ping
		}
		payload["heartbeat"] = beatInfo
	}
	if mon := g3MonitorJSON(m); mon != nil {
		payload["monitorInfo"] = map[string]any{
			"id":       mon["id"],
			"name":     mon["name"],
			"type":     mon["type"],
			"url":      mon["url"],
			"hostname": mon["hostname"],
			"port":     m.Monitor.Port(),
		}
	}

	headers := map[string]string{}
	if key := cfgStr(cfg, "flowtriqApiKey"); key != "" {
		headers["X-API-Key"] = key
	}
	return postJSONOK(ctx, "Flowtriq", cfgStr(cfg, "flowtriqWebhookUrl"), payload, headers)
}

// ---------------- pushbullet ----------------

func sendPushbullet(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	const target = "https://api.pushbullet.com/v2/pushes"
	headers := map[string]string{
		"Access-Token": cfgStr(cfg, "pushbulletAccessToken"),
	}

	if m.Monitor == nil {
		payload := map[string]any{
			"type":  "note",
			"title": "Uptime Kuma Alert",
			"body":  m.Msg,
		}
		return postJSONOK(ctx, "Pushbullet", target, payload, headers)
	}

	var mark string
	switch m.Status() {
	case model.StatusDown:
		mark = "[🔴 Down] "
	case model.StatusUp:
		mark = "[✅ Up] "
	default:
		// 官方只有 UP/DOWN 两个分支，其它状态直接返回成功
		return "Sent Successfully.", nil
	}
	payload := map[string]any{
		"type":  "note",
		"title": "UptimeKuma Alert: " + m.Monitor.Name,
		"body": mark + m.Beat.Msg +
			fmt.Sprintf("\nTime (%s): %s", g3Timezone, g3LocalDateTime(m)),
	}
	return postJSONOK(ctx, "Pushbullet", target, payload, headers)
}

// ---------------- SIGNL4 ----------------

func sendSignl4(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	payload := map[string]any{
		"heartbeat":         g3HeartbeatJSON(m),
		"monitor":           g3MonitorJSON(m),
		"msg":               m.Msg,
		"X-S4-SourceSystem": "UptimeKuma",
		"monitorUrl":        g3ExtractAddress(m),
	}

	if m.Monitor == nil {
		// 测试告警
		payload["title"] = "Uptime Kuma Alert"
		payload["message"] = m.Msg
	} else if m.Status() == model.StatusUp {
		payload["title"] = "Uptime Kuma Monitor ✅ Up"
		payload["X-S4-ExternalID"] = "UptimeKuma-" + strconv.FormatInt(m.Monitor.ID, 10)
		payload["X-S4-Status"] = "resolved"
	} else if m.Status() == model.StatusDown {
		payload["title"] = "Uptime Kuma Monitor 🔴 Down"
		payload["X-S4-ExternalID"] = "UptimeKuma-" + strconv.FormatInt(m.Monitor.ID, 10)
		payload["X-S4-Status"] = "new"
	}

	headers := map[string]string{"Content-Type": "application/json"}
	return postJSONOK(ctx, "SIGNL4", cfgStr(cfg, "webhookURL"), payload, headers)
}

// ---------------- MicrosoftGraph ----------------

// g3ParseGraphRecipients 对应 parseRecipients()：按逗号/分号切分。
func g3ParseGraphRecipients(value string) []map[string]any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var out []map[string]any
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';'
	}) {
		if addr := strings.TrimSpace(part); addr != "" {
			out = append(out, map[string]any{
				"emailAddress": map[string]any{"address": addr},
			})
		}
	}
	return out
}

func sendMicrosoftGraph(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	to := g3ParseGraphRecipients(cfgStr(cfg, "msGraphTo"))
	cc := g3ParseGraphRecipients(cfgStr(cfg, "msGraphCC"))
	bcc := g3ParseGraphRecipients(cfgStr(cfg, "msGraphBCC"))
	if len(to)+len(cc)+len(bcc) == 0 {
		return "", fmt.Errorf("At least one recipient (To, CC or BCC) is required")
	}

	mailbox := strings.TrimSpace(cfgStr(cfg, "msGraphMailbox"))
	if mailbox == "" {
		return "", fmt.Errorf("Sender mailbox is required")
	}

	subject := m.Msg
	body := m.Msg
	useHTML := false
	if m.Monitor != nil {
		body = m.Msg + fmt.Sprintf("\nTime (%s): %s", g3Timezone, g3LocalDateTime(m))
	}
	if (m.Monitor != nil) || strings.HasSuffix(m.Msg, "Testing") {
		if customSubject := strings.TrimSpace(cfgStr(cfg, "customSubject")); customSubject != "" {
			subject = g3RenderTemplate(customSubject, m)
		}
		if customBody := strings.TrimSpace(cfgStr(cfg, "customBody")); customBody != "" {
			useHTML = cfgBool(cfg, "htmlBody")
			body = g3RenderTemplate(customBody, m)
		}
	}

	token, err := g3GraphAccessToken(ctx, cfg)
	if err != nil {
		return "", err
	}

	contentType := "Text"
	if useHTML {
		contentType = "HTML"
	}
	payload := map[string]any{
		"message": map[string]any{
			"subject": subject,
			"body": map[string]any{
				"contentType": contentType,
				"content":     body,
			},
			"toRecipients":  g3GraphRecipientList(to),
			"ccRecipients":  g3GraphRecipientList(cc),
			"bccRecipients": g3GraphRecipientList(bcc),
		},
		"saveToSentItems": false,
	}

	target := "https://graph.microsoft.com/v1.0/users/" + url.PathEscape(mailbox) + "/sendMail"
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}
	return postJSONOK(ctx, "MicrosoftGraph", target, payload, headers)
}

func g3GraphRecipientList(in []map[string]any) []map[string]any {
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// g3GraphAccessToken 用 client_credentials 取 token。
func g3GraphAccessToken(ctx context.Context, cfg map[string]any) (string, error) {
	tenantID := strings.TrimSpace(cfgStr(cfg, "msGraphTenantId"))
	form := url.Values{}
	form.Set("client_id", strings.TrimSpace(cfgStr(cfg, "msGraphClientId")))
	form.Set("client_secret", cfgStr(cfg, "msGraphClientSecret"))
	form.Set("scope", "https://graph.microsoft.com/.default")
	form.Set("grant_type", "client_credentials")

	target := "https://login.microsoftonline.com/" + url.PathEscape(tenantID) + "/oauth2/v2.0/token"
	code, body, err := postFormValues(ctx, target, form, nil)
	if err != nil {
		return "", err
	}
	if err := expectOK("MicrosoftGraph", code, body); err != nil {
		return "", err
	}

	var res struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("Failed to obtain access token from Microsoft Entra ID")
	}
	return res.AccessToken, nil
}

// ---------------- twilio ----------------

func sendTwilio(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	apiKey := cfgStr(cfg, "twilioApiKey")
	if apiKey == "" {
		apiKey = cfgStr(cfg, "twilioAccountSID")
	}

	auth := base64.StdEncoding.EncodeToString([]byte(apiKey + ":" + cfgStr(cfg, "twilioAuthToken")))
	headers := map[string]string{
		"Authorization": "Basic " + auth,
		"Content-Type":  "application/x-www-form-urlencoded;charset=utf-8",
	}

	form := url.Values{}
	form.Set("To", cfgStr(cfg, "twilioToNumber"))
	form.Set("From", cfgStr(cfg, "twilioFromNumber"))
	form.Set("Body", m.Msg)
	if sid := cfgStr(cfg, "twilioMessagingServiceSID"); sid != "" {
		form.Set("MessagingServiceSid", sid)
	}

	target := "https://api.twilio.com/2010-04-01/Accounts/" +
		cfgStr(cfg, "twilioAccountSID") + "/Messages.json"
	return postFormOK(ctx, "twilio", target, form, headers)
}

// ---------------- evolution ----------------

// g3TrailingSlash 对应官方 evolution 的 replace(/([^/])\/+$/, "$1")：只剥末尾一个斜杠。
var g3TrailingSlash = regexp.MustCompile(`([^/])/+$`)

func sendEvolution(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	text := m.Msg
	if cfgBool(cfg, "evolutionUseCustomMessage") {
		if custom := cfgStr(cfg, "evolutionCustomMessage"); strings.TrimSpace(custom) != "" {
			text = g3RenderTemplate(custom, m)
		}
	}

	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"apikey":       cfgStr(cfg, "evolutionAuthToken"),
	}
	payload := map[string]any{
		"number": cfgStr(cfg, "evolutionRecipient"),
		"text":   text,
	}

	base := cfgStr(cfg, "evolutionApiUrl")
	if base == "" {
		base = "https://evolapicloud.com/"
	}
	// 官方只剥掉末尾一个斜杠（正则 ([^/])\/+$ ）
	base = g3TrailingSlash.ReplaceAllString(base, "$1")

	target := base + "/message/sendText/" + url.PathEscape(cfgStr(cfg, "evolutionInstanceName"))
	return postJSONOK(ctx, "evolution", target, payload, headers)
}

// ---------------- Cellsynt ----------------

// g3StripNonASCII 对应官方 msg.replace(/[^\x00-\x7F]/g, "")。
func g3StripNonASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] < 0x80 {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func sendCellsynt(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	allowConcat := "1"
	if cfgBool(cfg, "cellsyntAllowLongSMS") {
		allowConcat = "6"
	}

	// 官方用 axios 的 params（查询串）而不是请求体
	form := url.Values{}
	form.Set("username", cfgStr(cfg, "cellsyntLogin"))
	form.Set("password", cfgStr(cfg, "cellsyntPassword"))
	form.Set("destination", cfgStr(cfg, "cellsyntDestination"))
	form.Set("text", g3StripNonASCII(m.Msg))
	form.Set("originatortype", cfgStr(cfg, "cellsyntOriginatortype"))
	form.Set("originator", cfgStr(cfg, "cellsyntOriginator"))
	form.Set("allowconcat", allowConcat)

	target := "https://se-1.cellsynt.net/sms.php?" + form.Encode()
	code, body, err := httpDoRaw(ctx, http.MethodPost, target, "", nil, nil)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", expectOK("Cellsynt", code, body)
	}
	if strings.Contains(body, "Error:") {
		return "", fmt.Errorf("%s", strings.ReplaceAll(body, "Error:", ""))
	}
	return "Sent Successfully.", nil
}

// ---------------- YZJ ----------------

func sendYZJ(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	content := m.Msg
	if m.Monitor != nil {
		status := "❌"
		if m.Status() == model.StatusUp {
			status = "✅"
		}
		content = fmt.Sprintf("%s %s \n> %s\n> Time (%s): %s",
			status, m.Monitor.Name, m.Beat.Msg, g3Timezone, g3LocalDateTime(m))
	}

	target := fmt.Sprintf("%s?yzjtype=0&yzjtoken=%s",
		cfgStr(cfg, "yzjWebHookUrl"), cfgStr(cfg, "yzjToken"))
	headers := map[string]string{"Content-Type": "application/json"}

	code, body, err := postJSONBody(ctx, target, map[string]any{"content": content}, headers)
	if err != nil {
		return "", err
	}
	if err := expectOK("YZJ", code, body); err != nil {
		return "", err
	}

	var res struct {
		Success bool   `json:"success"`
		ErrMsg  string `json:"errmsg"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	if !res.Success {
		if res.ErrMsg != "" {
			return "", fmt.Errorf("%s", res.ErrMsg)
		}
		return "", fmt.Errorf("yzj's server did not respond with the expected result")
	}
	return "Sent Successfully.", nil
}

// ---------------- Milky ----------------

func sendMilky(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := cfgStr(cfg, "httpAddr")
	if !strings.HasPrefix(target, "http") {
		target = "http://" + target
	}
	// 官方的行为：已经以 "/" 结尾时不再补 /api/
	if !strings.HasSuffix(target, "/") {
		target += "/api/"
	}
	isGroup := cfgStr(cfg, "msgType") == "group"
	if isGroup {
		target += "send_group_message"
	} else {
		target += "send_private_message"
	}

	payload := map[string]any{
		"message": []any{
			map[string]any{
				"type": "text",
				"data": map[string]any{"text": "UptimeKuma Alert: " + m.Msg},
			},
		},
	}
	if isGroup {
		payload["group_id"] = cfgStr(cfg, "recieverId")
	} else {
		payload["user_id"] = cfgStr(cfg, "recieverId")
	}

	headers := map[string]string{
		"Authorization": "Bearer " + cfgStr(cfg, "accessToken"),
	}
	return postJSONOK(ctx, "Milky", target, payload, headers)
}
