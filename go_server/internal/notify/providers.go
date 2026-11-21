package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 本文件实现 P1 阶段的核心渠道。
// 每个渠道都是独立的 Provider，新增渠道只要写一个结构体 + init 里 Register。
// 字段名必须与前端组件里的 v-model 完全一致，否则读到空值。

// ---------------- 通用 HTTP 工具 ----------------

var httpClient = &http.Client{Timeout: 20 * time.Second}

func postJSON(ctx context.Context, target string, payload any, headers map[string]string) (int, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	return doRequest(ctx, http.MethodPost, target, "application/json", body, headers)
}

func doRequest(ctx context.Context, method, target, contentType string, body []byte, headers map[string]string) (int, string, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, string(respBody), fmt.Errorf("HTTP %d: %s", res.StatusCode, truncateStr(string(respBody), 200))
	}
	return res.StatusCode, string(respBody), nil
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------- Telegram ----------------

type telegramProvider struct{}

func (telegramProvider) Type() string { return "telegram" }

func (telegramProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	token := cfgStr(cfg, "telegramBotToken")
	chatID := cfgStr(cfg, "telegramChatID")
	if token == "" || chatID == "" {
		return "", fmt.Errorf("telegramBotToken 与 telegramChatID 不能为空")
	}

	base := cfgStr(cfg, "telegramServerUrl")
	if base == "" {
		base = "https://api.telegram.org"
	}
	target := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(base, "/"), token)

	payload := map[string]any{
		"chat_id":    chatID,
		"text":       msg,
		"parse_mode": "HTML",
	}
	if thread := cfgStr(cfg, "telegramMessageThreadID"); thread != "" {
		payload["message_thread_id"] = thread
	}
	if cfgBool(cfg, "telegramSendSilently") {
		payload["disable_notification"] = true
	}
	if cfgBool(cfg, "telegramProtectContent") {
		payload["protect_content"] = true
	}

	if _, _, err := postJSON(ctx, target, payload, nil); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- Slack ----------------

type slackProvider struct{}

func (slackProvider) Type() string { return "slack" }

func (slackProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	webhook := cfgStr(cfg, "slackwebhookURL")
	if webhook == "" {
		return "", fmt.Errorf("slackwebhookURL 不能为空")
	}

	payload := map[string]any{"text": msg}
	if ch := cfgStr(cfg, "slackchannel"); ch != "" {
		payload["channel"] = ch
	}
	if u := cfgStr(cfg, "slackusername"); u != "" {
		payload["username"] = u
	}
	if icon := firstNonEmpty(cfgStr(cfg, "slackiconemoji"), cfgStr(cfg, "slackiconemo")); icon != "" {
		payload["icon_emoji"] = icon
	}

	_, body, err := postJSON(ctx, webhook, payload, nil)
	if err != nil {
		return "", err
	}
	if body != "ok" && body != "" {
		// Slack webhook 成功时返回纯文本 ok
		if !strings.Contains(body, "ok") {
			return "", fmt.Errorf("%s", truncateStr(body, 200))
		}
	}
	return "Sent Successfully.", nil
}

// ---------------- Discord ----------------

type discordProvider struct{}

func (discordProvider) Type() string { return "discord" }

func (discordProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	webhook := cfgStr(cfg, "discordWebhookUrl")
	if webhook == "" {
		return "", fmt.Errorf("discordWebhookUrl 不能为空")
	}

	// 允许配置 URL 后缀 ?thread_id=
	if thread := cfgStr(cfg, "threadId"); thread != "" && !strings.Contains(webhook, "thread_id=") {
		sep := "?"
		if strings.Contains(webhook, "?") {
			sep = "&"
		}
		webhook += sep + "thread_id=" + url.QueryEscape(thread)
	}

	payload := map[string]any{
		"content":  msg,
		"username": firstNonEmpty(cfgStr(cfg, "discordUsername"), "Uptime Kuma"),
	}
	if cfgBool(cfg, "discordSuppressNotifications") {
		payload["flags"] = 4096
	}

	if _, _, err := postJSON(ctx, webhook, payload, nil); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- 钉钉 ----------------

type dingDingProvider struct{}

func (dingDingProvider) Type() string { return "DingDing" }

func (dingDingProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	target := firstNonEmpty(cfgStr(cfg, "webHookUrl"), cfgStr(cfg, "webhookURL"))
	if target == "" {
		return "", fmt.Errorf("webHookUrl 不能为空")
	}

	// 加签：timestamp + "\n" + secret → HMAC-SHA256 → base64 → url encode
	if secret := cfgStr(cfg, "secretKey"); secret != "" {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(ts + "\n" + secret))
		sign := url.QueryEscape(base64.StdEncoding.EncodeToString(h.Sum(nil)))

		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + "timestamp=" + ts + "&sign=" + sign
	}

	payload := map[string]any{
		"msgtype": "text",
		"text":    map[string]any{"content": msg},
	}

	// @ 指定人
	if cfgBool(cfg, "mentioning") {
		at := map[string]any{"isAtAll": false}
		if mobiles := splitList(cfgStr(cfg, "mobileList")); len(mobiles) > 0 {
			at["atMobiles"] = mobiles
		}
		if users := splitList(cfgStr(cfg, "userList")); len(users) > 0 {
			at["atUserIds"] = users
		}
		payload["at"] = at
	}

	_, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}

	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal([]byte(body), &res) == nil && res.ErrCode != 0 {
		return "", fmt.Errorf("%s", res.ErrMsg)
	}
	return "Sent Successfully.", nil
}

// ---------------- 飞书 ----------------

type feishuProvider struct{}

func (feishuProvider) Type() string { return "Feishu" }

func (feishuProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	target := cfgStr(cfg, "feishuWebHookUrl")
	if target == "" {
		return "", fmt.Errorf("feishuWebHookUrl 不能为空")
	}

	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]any{"text": msg},
	}

	_, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &res) == nil && res.Code != 0 {
		return "", fmt.Errorf("%s", res.Msg)
	}
	return "Sent Successfully.", nil
}

// ---------------- Gotify ----------------

type gotifyProvider struct{}

func (gotifyProvider) Type() string { return "gotify" }

func (gotifyProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	server := strings.TrimRight(cfgStr(cfg, "gotifyserverurl"), "/")
	token := cfgStr(cfg, "gotifyapplicationToken")
	if server == "" || token == "" {
		return "", fmt.Errorf("gotifyserverurl 与 gotifyapplicationToken 不能为空")
	}

	priority := cfgInt(cfg, "gotifyPriority")
	if priority <= 0 {
		priority = 8
	}

	payload := map[string]any{
		"title":    "Uptime Kuma",
		"message":  msg,
		"priority": priority,
	}
	target := fmt.Sprintf("%s/message?token=%s", server, url.QueryEscape(token))

	if _, _, err := postJSON(ctx, target, payload, nil); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- ntfy ----------------

type ntfyProvider struct{}

func (ntfyProvider) Type() string { return "ntfy" }

func (ntfyProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	server := strings.TrimRight(cfgStr(cfg, "ntfyserverurl"), "/")
	topic := cfgStr(cfg, "ntfytopic")
	if server == "" || topic == "" {
		return "", fmt.Errorf("ntfyserverurl 与 ntfytopic 不能为空")
	}

	headers := map[string]string{
		"Title":    firstNonEmpty(cfgStr(cfg, "ntfyCustomTitle"), "Uptime Kuma"),
		"Priority": firstNonEmpty(cfgStr(cfg, "ntfyPriority"), "3"),
	}
	if icon := cfgStr(cfg, "ntfyIcon"); icon != "" {
		headers["Icon"] = icon
	}
	if cfgBool(cfg, "ntfyCall") {
		headers["X-Call"] = "yes"
	}

	if token := cfgStr(cfg, "ntfyaccesstoken"); token != "" {
		headers["Authorization"] = "Bearer " + token
	} else if user := cfgStr(cfg, "ntfyusername"); user != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+cfgStr(cfg, "ntfypassword")))
	}

	target := server + "/" + url.PathEscape(topic)
	if _, _, err := doRequest(ctx, http.MethodPost, target, "text/plain; charset=utf-8", []byte(msg), headers); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- Bark ----------------

type barkProvider struct{}

func (barkProvider) Type() string { return "Bark" }

func (barkProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	endpoint := cfgStr(cfg, "barkEndpoint")
	if endpoint == "" {
		return "", fmt.Errorf("barkEndpoint 不能为空")
	}

	payload := map[string]any{
		"title": "Uptime Kuma",
		"body":  msg,
	}
	if g := cfgStr(cfg, "barkGroup"); g != "" {
		payload["group"] = g
	}
	if s := cfgStr(cfg, "barkSound"); s != "" {
		payload["sound"] = s
	}

	// barkEndpoint 形如 https://api.day.app/DEVICE_KEY
	target := strings.TrimRight(endpoint, "/")
	if _, _, err := postJSON(ctx, target, payload, nil); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- Server酱 ----------------

type serverChanProvider struct{}

func (serverChanProvider) Type() string { return "ServerChan" }

func (serverChanProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	key := cfgStr(cfg, "serverChanSendKey")
	if key == "" {
		return "", fmt.Errorf("serverChanSendKey 不能为空")
	}

	target := fmt.Sprintf("https://sctapi.ftqq.com/%s.send", url.PathEscape(key))
	payload := map[string]any{"title": "Uptime Kuma", "desp": msg}

	_, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &res) == nil && res.Code != 0 {
		return "", fmt.Errorf("%s", res.Msg)
	}
	return "Sent Successfully.", nil
}

// ---------------- PushPlus ----------------

type pushPlusProvider struct{}

func (pushPlusProvider) Type() string { return "PushPlus" }

func (pushPlusProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	key := cfgStr(cfg, "pushPlusSendKey")
	if key == "" {
		return "", fmt.Errorf("pushPlusSendKey 不能为空")
	}

	payload := map[string]any{
		"token":   key,
		"title":   "Uptime Kuma",
		"content": msg,
	}

	_, body, err := postJSON(ctx, "https://www.pushplus.plus/send", payload, nil)
	if err != nil {
		return "", err
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &res) == nil && res.Code != 200 {
		return "", fmt.Errorf("%s", res.Msg)
	}
	return "Sent Successfully.", nil
}

// ---------------- PushDeer ----------------

type pushDeerProvider struct{}

func (pushDeerProvider) Type() string { return "PushDeer" }

func (pushDeerProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	server := firstNonEmpty(cfgStr(cfg, "pushdeerServer"), "https://api2.pushdeer.com")
	key := cfgStr(cfg, "pushdeerKey")
	if key == "" {
		return "", fmt.Errorf("pushdeerKey 不能为空")
	}

	target := strings.TrimRight(server, "/") + "/message/push"
	payload := map[string]any{"pushkey": key, "text": "Uptime Kuma", "desp": msg}

	if _, _, err := postJSON(ctx, target, payload, nil); err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- Pushover ----------------

type pushoverProvider struct{}

func (pushoverProvider) Type() string { return "pushover" }

func (pushoverProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	token := cfgStr(cfg, "pushoverapptoken")
	user := cfgStr(cfg, "pushoveruserkey")
	if token == "" || user == "" {
		return "", fmt.Errorf("pushoverapptoken 与 pushoveruserkey 不能为空")
	}

	form := url.Values{}
	form.Set("token", token)
	form.Set("user", user)
	form.Set("message", msg)
	form.Set("title", firstNonEmpty(cfgStr(cfg, "pushovertitle"), "Uptime Kuma"))
	if d := cfgStr(cfg, "pushoverdevice"); d != "" {
		form.Set("device", d)
	}
	if p := cfgStr(cfg, "pushoverpriority"); p != "" {
		form.Set("priority", p)
	}
	if ttl := cfgStr(cfg, "pushoverttl"); ttl != "" {
		form.Set("ttl", ttl)
	}

	_, _, err := doRequest(ctx, http.MethodPost, "https://api.pushover.net/1/messages.json",
		"application/x-www-form-urlencoded", []byte(form.Encode()), nil)
	if err != nil {
		return "", err
	}
	return "Sent Successfully.", nil
}

// ---------------- 通用 Webhook ----------------

type webhookProvider struct{}

func (webhookProvider) Type() string { return "webhook" }

func (webhookProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return webhookProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

// SendMessage 按官方向通用 webhook 投递。
//
// 官方的请求体是 `{heartbeat, monitor, msg}`（server/notification-providers/webhook.js），
// 并且支持三种形态：
//   - 默认：POST JSON，三者都带（测试投递时 heartbeat/monitor 为 null）
//   - GET：把 msg / heartbeat / monitor 放进查询参数（对象需 JSON 序列化）
//   - webhookContentType=form-data：multipart 的 data 字段放整份 JSON
//   - webhookContentType=custom：用 webhookCustomBody 模板渲染
//
// 之前这里只发 `{"msg": ...}`，接收方按官方结构取字段会直接失败
// （线上表现是 HTTP 400 "Failed to parse JSON"）。
func (webhookProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimSpace(cfgStr(cfg, "webhookURL"))
	if target == "" {
		return "", fmt.Errorf("Webhook URL is required")
	}
	if _, err := url.Parse(target); err != nil {
		return "", fmt.Errorf("Invalid Webhook URL: %w", err)
	}

	method := strings.ToUpper(firstNonEmpty(cfgStr(cfg, "httpMethod"), "POST"))

	headers := map[string]string{}
	if raw := cfgStr(cfg, "webhookAdditionalHeaders"); raw != "" {
		var h map[string]string
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			return "", fmt.Errorf("Additional Headers is not a valid JSON")
		}
		headers = h
	}

	// 测试投递时与官方一致：heartbeat / monitor 都是 null
	var heartbeatJSON any
	if m.Monitor != nil {
		heartbeatJSON = m.Beat.JSON()
	}
	monitorJSON := g4MonitorMap(m)

	if method == http.MethodGet {
		u, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("Invalid Webhook URL: %w", err)
		}
		q := u.Query()
		q.Set("msg", m.Msg)
		if heartbeatJSON != nil {
			if raw, err := json.Marshal(heartbeatJSON); err == nil {
				q.Set("heartbeat", string(raw))
			}
		}
		if monitorJSON != nil {
			if raw, err := json.Marshal(monitorJSON); err == nil {
				q.Set("monitor", string(raw))
			}
		}
		u.RawQuery = q.Encode()

		code, body, err := httpDoRaw(ctx, http.MethodGet, u.String(), "", nil, headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("Webhook", code, body); err != nil {
			return "", err
		}
		return "Sent Successfully.", nil
	}

	payload := map[string]any{
		"heartbeat": heartbeatJSON,
		"monitor":   monitorJSON,
		"msg":       m.Msg,
	}

	switch strings.ToLower(strings.TrimSpace(cfgStr(cfg, "webhookContentType"))) {
	case "form-data":
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		raw, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		if err := w.WriteField("data", string(raw)); err != nil {
			return "", err
		}
		if err := w.Close(); err != nil {
			return "", err
		}
		code, body, err := httpDoRaw(ctx, http.MethodPost, target, w.FormDataContentType(), buf.Bytes(), headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("Webhook", code, body); err != nil {
			return "", err
		}
		return "Sent Successfully.", nil

	case "custom":
		// 官方用 Liquid 渲染 webhookCustomBody；这里复用已有的模板子集
		// （{{ msg }} / {{ name }} / {{ status }} / {{ monitorJSON.x }} ...）。
		// 自定义模板基本都是 JSON，按 application/json 发出，接收方才能直接解析。
		body := g1RenderTemplate(cfgStr(cfg, "webhookCustomBody"), m)
		code, respBody, err := httpDoRaw(ctx, http.MethodPost, target, "application/json", []byte(body), headers)
		if err != nil {
			return "", err
		}
		if err := expectOK("Webhook", code, respBody); err != nil {
			return "", err
		}
		return "Sent Successfully.", nil
	}

	return postJSONOK(ctx, "Webhook", target, payload, headers)
}

// ---------------- SMTP ----------------

type smtpProvider struct{}

func (smtpProvider) Type() string { return "smtp" }

func (smtpProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	host := cfgStr(cfg, "smtpHost")
	if host == "" {
		return "", fmt.Errorf("smtpHost 不能为空")
	}

	port := cfgInt(cfg, "smtpPort")
	if port <= 0 {
		port = 587
	}

	from := cfgStr(cfg, "smtpFrom")
	to := splitList(cfgStr(cfg, "smtpTo"))
	if from == "" || len(to) == 0 {
		return "", fmt.Errorf("smtpFrom 与 smtpTo 不能为空")
	}

	subject := firstNonEmpty(cfgStr(cfg, "customSubject"), "Uptime Kuma Alert")
	body := firstNonEmpty(cfgStr(cfg, "customBody"), msg)
	if cfgBool(cfg, "htmlBody") {
		body = "<pre>" + body + "</pre>"
	}

	headers := map[string]string{
		"From":         from,
		"To":           strings.Join(to, ", "),
		"Subject":      subject,
		"MIME-Version": "1.0",
		"Content-Type": "text/plain; charset=UTF-8",
	}
	if cc := splitList(cfgStr(cfg, "smtpCC")); len(cc) > 0 {
		headers["Cc"] = strings.Join(cc, ", ")
	}

	var sb strings.Builder
	for k, v := range headers {
		sb.WriteString(k + ": " + v + "\r\n")
	}
	sb.WriteString("\r\n" + body)

	return sendSMTP(ctx, host, int(port), cfg, from, to, []byte(sb.String()))
}

func sendSMTP(ctx context.Context, host string, port int, cfg map[string]any, from string, to []string, data []byte) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	secure := cfgBool(cfg, "smtpSecure") || port == 465
	insecure := cfgBool(cfg, "smtpIgnoreTLSError")
	ignoreStartTLS := cfgBool(cfg, "smtpIgnoreSTARTTLS")

	user := cfgStr(cfg, "smtpUsername")
	pass := cfgStr(cfg, "smtpPassword")

	tlsCfg := &tls.Config{ServerName: host, InsecureSkipVerify: insecure} //nolint:gosec // 用户可选项

	var client *smtp.Client
	var err error

	if secure {
		conn, dialErr := tls.Dial("tcp", addr, tlsCfg)
		if dialErr != nil {
			return "", dialErr
		}
		client, err = smtp.NewClient(conn, host)
	} else {
		client, err = smtp.Dial(addr)
	}
	if err != nil {
		return "", err
	}
	defer client.Close()

	if !secure && !ignoreStartTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				return "", err
			}
		}
	}

	if user != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			auth := smtp.PlainAuth("", user, pass, host)
			if err := client.Auth(auth); err != nil {
				return "", err
			}
		}
	}

	if err := client.Mail(from); err != nil {
		return "", err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return "", err
		}
	}

	w, err := client.Data()
	if err != nil {
		return "", err
	}
	if _, err := w.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	_ = client.Quit()
	return "Sent Successfully.", nil
}

// ---------------- 小工具 ----------------

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func init() {
	Register(telegramProvider{})
	Register(slackProvider{})
	Register(discordProvider{})
	Register(dingDingProvider{})
	Register(feishuProvider{})
	Register(gotifyProvider{})
	Register(ntfyProvider{})
	Register(barkProvider{})
	Register(serverChanProvider{})
	Register(pushPlusProvider{})
	Register(pushDeerProvider{})
	Register(pushoverProvider{})
	Register(webhookProvider{})
	Register(smtpProvider{})
}
