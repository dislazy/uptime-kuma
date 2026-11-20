package notify

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// ---------------- 阿里云短信 ----------------

// aliyunSMSProvider 走阿里云 dysmsapi 的 SendSms 接口。
//
// 阿里云的签名规则比较特殊：除了 A-Za-z0-9~-._ 之外的字符全部要做
// RFC3986 编码（比 encodeURIComponent 多编 ! * ' ( ) 五个），
// 而且待签串本身还要再编码一次。这里手写是为了不引入一个只为签名的库。
type aliyunSMSProvider struct{}

func (aliyunSMSProvider) Type() string { return "AliyunSMS" }

func (aliyunSMSProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return aliyunSMSProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (aliyunSMSProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	phone := strings.TrimSpace(cfgStr(cfg, "phonenumber"))
	template := strings.TrimSpace(cfgStr(cfg, "templateCode"))
	sign := strings.TrimSpace(cfgStr(cfg, "signName"))
	keyID := strings.TrimSpace(cfgStr(cfg, "accessKeyId"))
	keySecret := strings.TrimSpace(cfgStr(cfg, "secretAccessKey"))

	if phone == "" {
		return "", fmt.Errorf("Phone Number is required")
	}
	if template == "" {
		return "", fmt.Errorf("Template Code is required")
	}
	if sign == "" {
		return "", fmt.Errorf("Sign Name is required")
	}
	if keyID == "" || keySecret == "" {
		return "", fmt.Errorf("AccessKey ID and Secret are required")
	}

	// 模板参数：监控名 / 时间 / 状态（测试投递时留空）
	templateParam := map[string]any{"name": "", "time": "", "status": ""}

	if m.Monitor != nil {
		templateParam["name"] = m.Monitor.Name
		templateParam["time"] = time.Now().Format("2006-01-02 15:04:05")
		templateParam["status"] = aliyunStatusText(m.Status())

		if cfgBool(cfg, "optionalParameters") {
			// 阿里云短信审核不允许出现 IP 和域名，这里按官方规则替换掉
			templateParam["msg"] = removeIPAndDomain(m.Beat.Msg)
		}
	} else if cfgBool(cfg, "optionalParameters") {
		templateParam["msg"] = removeIPAndDomain(m.Msg)
	}

	paramJSON, err := json.Marshal(templateParam)
	if err != nil {
		return "", err
	}

	params := map[string]string{
		"PhoneNumbers":     phone,
		"TemplateCode":     template,
		"SignName":         sign,
		"TemplateParam":    string(paramJSON),
		"AccessKeyId":      keyID,
		"Format":           "JSON",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"SignatureNonce":   aliyunNonce(),
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"Action":           "SendSms",
		"Version":          "2017-05-25",
	}
	params["Signature"] = aliyunSign(params, keySecret)

	// 表单体：所有参数按编码后拼接
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, aliyunEncode(k)+"="+aliyunEncode(params[k]))
	}
	form := strings.Join(pairs, "&")

	status, body, err := doRequest(ctx, http.MethodPost, "https://dysmsapi.aliyuncs.com/",
		"application/x-www-form-urlencoded", []byte(form), nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("Aliyun SMS returned %d: %s", status, truncateStr(body, 200))
	}

	var res struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal([]byte(body), &res); err == nil && res.Code != "OK" {
		return "", fmt.Errorf("Aliyun SMS error %s: %s", res.Code, res.Message)
	}

	return "Sent Successfully.", nil
}

func aliyunStatusText(status int) string {
	switch status {
	case model.StatusDown:
		return "DOWN"
	case model.StatusUp:
		return "UP"
	default:
		return fmt.Sprintf("%d", status)
	}
}

// aliyunEncode 按阿里云要求做 RFC3986 编码。
func aliyunEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '~' || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
			continue
		}
		b.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return b.String()
}

// aliyunSign 生成 Signature 参数。
func aliyunSign(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, aliyunEncode(k)+"="+aliyunEncode(params[k]))
	}

	stringToSign := "POST&" + aliyunEncode("/") + "&" + aliyunEncode(strings.Join(pairs, "&"))

	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func aliyunNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// 阿里云短信内容审核不允许出现具体地址
var (
	reURL    = regexp.MustCompile(`(?i)(?:https?|ftp|ws|wss)://[^\s]+`)
	reIPv4   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)
	reIPv6   = regexp.MustCompile(`\[?(?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}\]?(?::\d+)?`)
	reDomain = regexp.MustCompile(`\b(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}(?::\d+)?\b`)
	reCIDR   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}/\d{1,2}\b`)
)

func removeIPAndDomain(msg string) string {
	if msg == "" {
		return msg
	}
	msg = reURL.ReplaceAllString(msg, "[URL]")
	msg = reIPv4.ReplaceAllString(msg, "[IP]")
	msg = reIPv6.ReplaceAllString(msg, "[IP]")
	msg = reDomain.ReplaceAllString(msg, "[Domain]")
	msg = reCIDR.ReplaceAllString(msg, "[CIDR]")
	return msg
}

// ---------------- Microsoft Teams ----------------

// teamsProvider 发 Adaptive Card 到 Teams 的 Incoming Webhook。
type teamsProvider struct{}

func (teamsProvider) Type() string { return "teams" }

func (teamsProvider) Send(ctx context.Context, cfg map[string]any, msg string) (string, error) {
	return teamsProvider{}.SendMessage(ctx, cfg, Message{Msg: msg, Test: true})
}

func (teamsProvider) SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	target := strings.TrimSpace(cfgStr(cfg, "webhookUrl"))
	if target == "" {
		return "", fmt.Errorf("Webhook URL is required")
	}
	if _, err := url.Parse(target); err != nil {
		return "", fmt.Errorf("Invalid Webhook URL: %w", err)
	}

	status := m.Status()
	heading := teamsHeading(status, m.MonitorName())
	style := "emphasis"
	switch status {
	case model.StatusDown:
		style = "attention"
	case model.StatusUp:
		style = "good"
	}

	facts := []map[string]any{}
	if m.Monitor != nil {
		facts = append(facts, map[string]any{"title": "Error", "value": m.Beat.Msg})
		facts = append(facts, map[string]any{"title": "Monitor", "value": m.MonitorName()})
		if addr := monitorSource(m); addr != "" {
			facts = append(facts, map[string]any{"title": "URL", "value": addr})
		}
		facts = append(facts, map[string]any{
			"title": "Time",
			"value": time.Now().Format("2006-01-02 15:04:05"),
		})
	}

	body := []map[string]any{
		{
			"type": "Container",
			"items": []map[string]any{
				{
					"type":   "TextBlock",
					"size":   "Medium",
					"weight": "Bolder",
					"text":   "**" + heading + "**",
					"color":  teamsColor(status),
				},
				{
					"type":     "TextBlock",
					"size":     "Small",
					"text":     "Uptime Kuma Alert",
					"isSubtle": true,
					"spacing":  "None",
				},
			},
			"style": style,
		},
	}

	if len(facts) > 0 {
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	} else {
		// 测试消息没有监控信息，直接展示文案
		body = append(body, map[string]any{"type": "TextBlock", "text": m.Msg, "wrap": true})
	}

	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.5",
		"body":    body,
	}

	payload := map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}

	statusCode, respBody, err := postJSON(ctx, target, payload, nil)
	if err != nil {
		return "", err
	}
	if statusCode < 200 || statusCode >= 300 {
		return "", fmt.Errorf("Teams returned %d: %s", statusCode, truncateStr(respBody, 200))
	}
	return "Sent Successfully.", nil
}

func teamsHeading(status int, name string) string {
	switch status {
	case model.StatusDown:
		return "🔴 [" + name + "] went down"
	case model.StatusUp:
		return "✅ [" + name + "] is back online"
	}
	return "Notification"
}

func teamsColor(status int) string {
	switch status {
	case model.StatusDown:
		return "attention"
	case model.StatusUp:
		return "good"
	}
	return "default"
}

func init() {
	Register(aliyunSMSProvider{})
	Register(teamsProvider{})
}
