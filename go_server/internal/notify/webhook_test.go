package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

func testMessage() Message {
	ping := int64(123)
	return Message{
		Monitor: &model.Monitor{
			ID:   7,
			Name: "站点",
			Type: "http",
			Raw:  map[string]any{"url": "https://example.com"},
		},
		Beat: model.Beat{
			MonitorID: 7,
			Status:    model.StatusDown,
			Msg:       "500 - Internal Server Error",
			Time:      "2026-11-21 13:00:00.000",
			Ping:      &ping,
			Important: true,
		},
		Msg: "[站点] [🔴 Down] 500 - Internal Server Error",
	}
}

// TestWebhookDefaultPayload 对齐官方 webhook.js：POST JSON 形如
// `{heartbeat, monitor, msg}`，而不是只有 msg。
//
// 线上症状：接收方按官方结构取 heartbeat/monitor，收到 {"msg": ...} 后
// 直接回 400 "Failed to parse JSON"。
func TestWebhookDefaultPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type 期望 application/json，实际 %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("请求体不是 JSON: %v (%s)", err, body)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	if _, err := (webhookProvider{}).SendMessage(context.Background(),
		map[string]any{"webhookURL": srv.URL}, testMessage()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	for _, k := range []string{"heartbeat", "monitor", "msg"} {
		if _, ok := got[k]; !ok {
			t.Errorf("请求体缺少 %q 字段: %v", k, got)
		}
	}
	hb, _ := got["heartbeat"].(map[string]any)
	if hb == nil || hb["monitorID"] == nil {
		t.Errorf("heartbeat 内容不对: %v", got["heartbeat"])
	}
	mon, _ := got["monitor"].(map[string]any)
	if mon == nil || mon["name"] != "站点" {
		t.Errorf("monitor 内容不对: %v", got["monitor"])
	}
}

// TestWebhookTestMessageHasNullContext 测试投递时 heartbeat/monitor 必须是 null。
func TestWebhookTestMessageHasNullContext(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	if _, err := (webhookProvider{}).Send(context.Background(),
		map[string]any{"webhookURL": srv.URL}, "测试消息"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got["heartbeat"] != nil || got["monitor"] != nil {
		t.Fatalf("测试消息的 heartbeat/monitor 应为 null，实际 %v / %v", got["heartbeat"], got["monitor"])
	}
	if got["msg"] != "测试消息" {
		t.Fatalf("msg 不对: %v", got["msg"])
	}
}

// TestWebhookGetQuery 对齐官方 GET 形态：msg / heartbeat / monitor 放查询参数。
func TestWebhookGetQuery(t *testing.T) {
	var q map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
	}))
	defer srv.Close()

	cfg := map[string]any{"webhookURL": srv.URL, "httpMethod": "GET"}
	if _, err := (webhookProvider{}).SendMessage(context.Background(), cfg, testMessage()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	if len(q["msg"]) == 0 || q["msg"][0] == "" {
		t.Errorf("缺少 msg 查询参数: %v", q)
	}
	for _, k := range []string{"heartbeat", "monitor"} {
		if len(q[k]) == 0 {
			t.Fatalf("缺少 %s 查询参数: %v", k, q)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(q[k][0]), &obj); err != nil {
			t.Errorf("%s 不是 JSON: %v (%s)", k, err, q[k][0])
		}
	}
}

// TestWebhookFormData 对齐 webhookContentType=form-data：data 字段放整份 JSON。
func TestWebhookFormData(t *testing.T) {
	var data string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("不是 multipart: %v", err)
			return
		}
		data = r.FormValue("data")
	}))
	defer srv.Close()

	cfg := map[string]any{"webhookURL": srv.URL, "webhookContentType": "form-data"}
	if _, err := (webhookProvider{}).SendMessage(context.Background(), cfg, testMessage()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		t.Fatalf("data 字段不是 JSON: %v (%q)", err, data)
	}
	if obj["msg"] == nil {
		t.Fatalf("data 里缺少 msg: %v", obj)
	}
}

// TestWebhookCustomBody 自定义模板要能渲染监控名与状态。
func TestWebhookCustomBody(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
	}))
	defer srv.Close()

	cfg := map[string]any{
		"webhookURL":         srv.URL,
		"webhookContentType": "custom",
		"webhookCustomBody":  `{"text":"{{ name }}","msg":"{{ msg }}"}`,
	}
	if _, err := (webhookProvider{}).SendMessage(context.Background(), cfg, testMessage()); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if !strings.Contains(body, "站点") {
		t.Fatalf("模板没有渲染出监控名: %s", body)
	}
}
