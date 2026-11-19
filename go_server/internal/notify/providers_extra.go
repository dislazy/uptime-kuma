package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// 本文件实现国内常用渠道与国际主流 IM 渠道。
//
// 它们都需要区分 UP/DOWN 来生成标题，所以实现的是 ContextProvider（SendMessage），
// 而不是只拿纯文本的 Send。

// ---------------- 企业微信（群机器人） ----------------

type weComProvider struct{}

func (weComProvider) Type() string { return "WeCom" }

func (weComProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return weComProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (weComProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	key := strings.TrimSpace(cfgStr(cfg, "weComBotKey"))
	if key == "" {
		return "", fmt.Errorf("WeCom Bot Key is required")
	}

	title := "UptimeKuma Message"
	switch m.Status() {
	case model.StatusUp:
		title = "UptimeKuma Monitor Up"
	case model.StatusDown:
		title = "UptimeKuma Monitor Down"
	}

	text := map[string]any{
		"content": title + "\n" + m.Msg,
	}

	// 支持 @手机号，多个用逗号分隔
	if raw := strings.TrimSpace(cfgStr(cfg, "weComMentionedMobileList")); raw != "" {
		var mobiles []string
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				mobiles = append(mobiles, p)
			}
		}
		if len(mobiles) > 0 {
			text["mentioned_mobile_list"] = mobiles
		}
	}

	payload := map[string]any{"msgtype": "text", "text": text}

	target := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + url.QueryEscape(key)
	status, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("WeCom returned %d: %s", status, truncateStr(body, 200))
	}

	// 企业微信在 HTTP 200 里用 errcode 表达业务失败
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal([]byte(body), &res); err == nil && res.ErrCode != 0 {
		return "", fmt.Errorf("WeCom error %d: %s", res.ErrCode, res.ErrMsg)
	}

	return "Sent Successfully.", nil
}

// ---------------- WxPusher ----------------

type wxPusherProvider struct{}

func (wxPusherProvider) Type() string { return "WxPusher" }

// wxPusherBatch 是单次请求最多的 SPT 数（官方限制）
const wxPusherBatch = 10

func (wxPusherProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return wxPusherProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (wxPusherProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	var spts []string
	for _, p := range strings.Split(cfgStr(cfg, "wxpusherSPT"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			spts = append(spts, p)
		}
	}
	if len(spts) == 0 {
		return "", fmt.Errorf("No WxPusher SPT is configured")
	}

	summary := wxPusherTitle(m)
	if len([]rune(summary)) > 100 {
		summary = string([]rune(summary)[:100])
	}

	// 超过 10 个就分批，避免后面的 SPT 被静默丢弃
	for i := 0; i < len(spts); i += wxPusherBatch {
		end := i + wxPusherBatch
		if end > len(spts) {
			end = len(spts)
		}

		payload := map[string]any{
			"content":     m.Msg,
			"summary":     summary,
			"contentType": 1,
			"sptList":     spts[i:end],
		}

		status, body, err := postJSON(ctx,
			"https://wxpusher.zjiecode.com/api/send/message/simple-push", payload, nil)
		if err != nil {
			return "", err
		}
		if status < 200 || status >= 300 {
			return "", fmt.Errorf("WxPusher returned %d: %s", status, truncateStr(body, 200))
		}

		var res struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(body), &res); err == nil && res.Code != 1000 {
			return "", fmt.Errorf("WxPusher error %d: %s", res.Code, res.Msg)
		}
	}

	return "Sent Successfully.", nil
}

func wxPusherTitle(m Message) string {
	switch m.Status() {
	case model.StatusUp:
		return "UptimeKuma Monitor Up " + m.MonitorName()
	case model.StatusDown:
		return "UptimeKuma Monitor Down " + m.MonitorName()
	}
	return "UptimeKuma Message"
}

// ---------------- Spug 推送 ----------------

type spugPushProvider struct{}

func (spugPushProvider) Type() string { return "SpugPush" }

func (spugPushProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return spugPushProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (spugPushProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	key := strings.TrimSpace(cfgStr(cfg, "templateKey"))
	if key == "" {
		return "", fmt.Errorf("Template Key is required")
	}

	title := "Uptime Kuma Message"
	content := m.Msg

	switch m.Status() {
	case model.StatusUp:
		title = fmt.Sprintf("UptimeKuma 「%s」 is Up", m.MonitorName())
		content = "[✅ Up] " + m.Beat.Msg
	case model.StatusDown:
		title = fmt.Sprintf("UptimeKuma 「%s」 is Down", m.MonitorName())
		content = "[🔴 Down] " + m.Beat.Msg
	}

	payload := map[string]any{"title": title, "content": content}
	target := "https://push.spug.cc/send/" + url.PathEscape(key)

	status, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("SpugPush returned %d: %s", status, truncateStr(body, 200))
	}
	return "Sent Successfully.", nil
}

// ---------------- Google Chat ----------------

type googleChatProvider struct{}

func (googleChatProvider) Type() string { return "GoogleChat" }

func (googleChatProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return googleChatProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (googleChatProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimSpace(cfgStr(cfg, "googleChatWebhookURL"))
	if target == "" {
		return "", fmt.Errorf("Webhook URL is required")
	}

	// 官方支持自定义模板，这里退化为纯文本（模板语法是 JS 表达式，无法直接复用）
	payload := map[string]any{"text": m.Msg}

	retries := cfgInt(cfg, "googleChatMaxRetries")
	if retries <= 0 {
		retries = 1
	}
	if retries > 10 {
		retries = 10
	}

	var lastErr error
	for i := int64(0); i < retries; i++ {
		status, body, err := postJSON(ctx, target, payload, nil)
		if err == nil && status >= 200 && status < 300 {
			return "Sent Successfully.", nil
		}

		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("Google Chat returned %d: %s", status, truncateStr(body, 200))
		}

		// 只有 429 才值得退避重试，其它错误立即失败
		if status != http.StatusTooManyRequests {
			break
		}
		// 官方会等 60~180 秒；这里压缩到 2 秒，避免卡住整轮通知
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return "", lastErr
}

// ---------------- Matrix ----------------

type matrixProvider struct{}

func (matrixProvider) Type() string { return "matrix" }

func (matrixProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return matrixProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (matrixProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	homeserver := strings.TrimRight(strings.TrimSpace(cfgStr(cfg, "homeserverUrl")), "/")
	roomID := strings.TrimSpace(cfgStr(cfg, "internalRoomId"))
	token := strings.TrimSpace(cfgStr(cfg, "accessToken"))

	if homeserver == "" {
		return "", fmt.Errorf("Homeserver URL is required")
	}
	if roomID == "" {
		return "", fmt.Errorf("Internal Room ID is required")
	}
	if token == "" {
		return "", fmt.Errorf("Access Token is required")
	}

	// 每条消息要带唯一的 txnId，否则 Matrix 会当成重复消息丢弃
	target := fmt.Sprintf("%s/_matrix/client/r0/rooms/%s/send/m.room.message/%s",
		homeserver, url.PathEscape(roomID), matrixTxnID())

	payload := map[string]any{"msgtype": "m.text", "body": m.Msg}

	status, body, err := doJSON(ctx, http.MethodPut, target, payload,
		map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("Matrix returned %d: %s", status, truncateStr(body, 200))
	}
	return "Sent Successfully.", nil
}

func matrixTxnID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("gokuma%d", time.Now().UnixNano())
	}
	return "gokuma" + hex.EncodeToString(b)
}

// ---------------- PagerDuty ----------------

type pagerDutyProvider struct{}

func (pagerDutyProvider) Type() string { return "PagerDuty" }

func (pagerDutyProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return pagerDutyProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (pagerDutyProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimSpace(cfgStr(cfg, "pagerdutyIntegrationUrl"))
	key := strings.TrimSpace(cfgStr(cfg, "pagerdutyIntegrationKey"))
	if target == "" {
		return "", fmt.Errorf("Integration URL is required")
	}
	if key == "" {
		return "", fmt.Errorf("Integration Key is required")
	}

	severity := strings.TrimSpace(cfgStr(cfg, "pagerdutyPriority"))
	if severity == "" {
		severity = "warning"
	}

	eventAction := "trigger"
	title := "Uptime Kuma Alert"

	switch m.Status() {
	case model.StatusUp:
		title = "Uptime Kuma Monitor ✅ Up"
		eventAction = "resolve"
		// 自动恢复可以配置成 0（不恢复）或自定义动作
		if v := strings.TrimSpace(cfgStr(cfg, "pagerdutyAutoResolve")); v != "" && v != "0" {
			eventAction = v
		} else if v == "0" {
			return "no action required", nil
		}
	case model.StatusDown:
		title = "Uptime Kuma Monitor 🔴 Down"
		eventAction = "trigger"
	}

	source := monitorSource(m)
	summary := "[" + title + "] " + m.Beat.Msg
	if name := m.MonitorName(); name != "" {
		summary = "[" + title + "] [" + name + "] " + m.Beat.Msg
	}

	dedupKey := "Uptime Kuma/test"
	if m.Monitor != nil {
		dedupKey = fmt.Sprintf("Uptime Kuma/%d", m.Monitor.ID)
	}

	payload := map[string]any{
		"payload": map[string]any{
			"summary":  summary,
			"severity": severity,
			"source":   source,
		},
		"routing_key":  key,
		"event_action": eventAction,
		"dedup_key":    dedupKey,
	}

	status, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("PagerDuty returned %d: %s", status, truncateStr(body, 200))
	}
	return "PagerDuty notification succeed", nil
}

// monitorSource 取一个能标识监控对象的字符串（官方按类型取 hostname / url）。
func monitorSource(m Message) string {
	if m.Monitor == nil {
		return "Uptime Kuma Test Button"
	}
	if strings.EqualFold(m.Monitor.Get("type"), "port") {
		if p := m.Monitor.Port(); p > 0 {
			return fmt.Sprintf("%s:%d", m.Monitor.Hostname(), p)
		}
	}
	if h := m.Monitor.Hostname(); h != "" {
		return h
	}
	return m.Monitor.URL()
}

// ---------------- Mattermost ----------------

type mattermostProvider struct{}

func (mattermostProvider) Type() string { return "mattermost" }

func (mattermostProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return mattermostProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (mattermostProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimSpace(cfgStr(cfg, "mattermostWebhookUrl"))
	if target == "" {
		return "", fmt.Errorf("Webhook URL is required")
	}

	username := strings.TrimSpace(cfgStr(cfg, "mattermostusername"))
	if username == "" {
		username = "Uptime Kuma"
	}

	payload := map[string]any{"username": username, "text": m.Msg}

	if m.Monitor != nil {
		channel := strings.ToLower(strings.TrimSpace(cfgStr(cfg, "mattermostchannel")))
		if channel != "" {
			payload["channel"] = channel
		}

		color := "#32cd32"
		title := "Monitor Up"
		if m.Status() == model.StatusDown {
			color = "#ff0000"
			title = "Monitor Down"
		}

		payload["text"] = "Uptime Kuma Alert"
		payload["attachments"] = []map[string]any{{
			"color": color,
			"title": fmt.Sprintf("%s: %s", title, m.MonitorName()),
			"text":  m.Beat.Msg,
		}}
	}

	status, body, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("Mattermost returned %d: %s", status, truncateStr(body, 200))
	}
	return "Sent Successfully.", nil
}

// ---------------- 注册 ----------------

func init() {
	Register(weComProvider{})
	Register(wxPusherProvider{})
	Register(spugPushProvider{})
	Register(googleChatProvider{})
	Register(matrixProvider{})
	Register(pagerDutyProvider{})
	Register(mattermostProvider{})
}

// doJSON 是带任意 HTTP 方法的 JSON 请求包装（postJSON 只支持 POST）。
func doJSON(ctx context.Context, method, target string, payload any, headers map[string]string) (int, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	return doRequest(ctx, method, target, "application/json", body, headers)
}
