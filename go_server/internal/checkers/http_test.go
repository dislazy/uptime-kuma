package checkers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// httpMonitor 构造一个只带必要列的监控，模拟官方库里的一行。
func httpMonitor(typ, url string, extra map[string]any) *model.Monitor {
	raw := map[string]any{
		"url":  url,
		"type": typ,
	}
	for k, v := range extra {
		raw[k] = v
	}
	return &model.Monitor{Type: typ, Raw: raw, Interval: 60}
}

// TestHTTPTypeIgnoresBodyAssertions 是 2026-11-21 线上事故的回归测试。
//
// 官方 2.x 前端给每个新监控都存 `json_path = "$"`、`expected_value = NULL`，
// 但只有类型为 json-query 时才真正执行 JSON 断言（server/model/monitor.js）。
// 我们曾经写成"json_path 非空就断言"，结果所有普通 http 监控都被拿去
// 比较整份响应和空串，全部误判 DOWN。
func TestHTTPTypeIgnoresBodyAssertions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>星空</body></html>"))
	}))
	defer srv.Close()

	cases := []struct {
		name  string
		extra map[string]any
	}{
		{"json_path=$, expected 为空（线上默认值）", map[string]any{
			"json_path": "$", "json_path_operator": "==",
		}},
		{"带 keyword 列但类型是 http", map[string]any{
			"keyword": "不存在的关键字",
		}},
		{"json_path 与 keyword 同时存在", map[string]any{
			"json_path": "$", "json_path_operator": "==", "keyword": "不存在",
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := httpMonitor("http", srv.URL, c.extra)
			res := HTTPChecker{}.Check(context.Background(), m)
			if res.Status != model.StatusUp {
				t.Fatalf("http 类型只看状态码，应为 UP，实际 %v: %s", res.Status, res.Msg)
			}
		})
	}
}

// TestKeywordTypeChecksBody 保证 keyword 类型仍然做正文断言。
func TestKeywordTypeChecksBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	up := httpMonitor("keyword", srv.URL, map[string]any{"keyword": "hello"})
	if res := (HTTPChecker{}).Check(context.Background(), up); res.Status != model.StatusUp {
		t.Fatalf("关键字命中应为 UP，实际 %v: %s", res.Status, res.Msg)
	}

	down := httpMonitor("keyword", srv.URL, map[string]any{"keyword": "absent"})
	if res := (HTTPChecker{}).Check(context.Background(), down); res.Status != model.StatusDown {
		t.Fatalf("关键字未命中应为 DOWN，实际 %v: %s", res.Status, res.Msg)
	}

	// invert_keyword 取反
	inv := httpMonitor("keyword", srv.URL, map[string]any{"keyword": "hello", "invert_keyword": 1})
	if res := (HTTPChecker{}).Check(context.Background(), inv); res.Status != model.StatusDown {
		t.Fatalf("invert_keyword 下关键字命中应为 DOWN，实际 %v: %s", res.Status, res.Msg)
	}
}

// TestJSONQueryTypeChecksPath 保证 json-query 类型仍然按 path 取值比较。
func TestJSONQueryTypeChecksPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","count":3}`))
	}))
	defer srv.Close()

	up := httpMonitor("json-query", srv.URL, map[string]any{
		"json_path": "$.status", "json_path_operator": "==", "expected_value": "ok",
	})
	res := HTTPChecker{}.Check(context.Background(), up)
	if res.Status != model.StatusUp {
		t.Fatalf("JSON 值匹配应为 UP，实际 %v: %s", res.Status, res.Msg)
	}

	down := httpMonitor("json-query", srv.URL, map[string]any{
		"json_path": "$.status", "json_path_operator": "==", "expected_value": "fail",
	})
	if res := (HTTPChecker{}).Check(context.Background(), down); res.Status != model.StatusDown {
		t.Fatalf("JSON 值不匹配应为 DOWN，实际 %v: %s", res.Status, res.Msg)
	}
}

// TestHTTPTypeStatusCodes 保证状态码断言仍然生效（与断言类型无关）。
func TestHTTPTypeStatusCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := httpMonitor("http", srv.URL, nil)
	if res := (HTTPChecker{}).Check(context.Background(), m); res.Status != model.StatusDown {
		t.Fatalf("500 应为 DOWN，实际 %v: %s", res.Status, res.Msg)
	}

	// 把 500 加进接受范围就应通过
	m2 := httpMonitor("http", srv.URL, map[string]any{"accepted_statuscodes_json": `["200-299","500"]`})
	if res := (HTTPChecker{}).Check(context.Background(), m2); res.Status != model.StatusUp {
		t.Fatalf("500 在可接受范围内应为 UP，实际 %v: %s", res.Status, res.Msg)
	}
}
