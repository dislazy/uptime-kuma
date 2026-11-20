package handlers

import (
	"encoding/json"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// TestMonitorJSONConfigIsObject 守住 monitor.config 的往返形态。
//
// 官方 src/pages/EditMonitor.vue 读的是 monitor.config.xxx（对象），
// server/model/monitor.js 的 toJSON 里也是 config: this.getConfig()。
// 如果这里吐字符串，前端编辑表单会静默丢字段（2026-10 起 systemServiceName
// 等 per-type 字段都收进了 config，丢了就等于改不动监控）。
func TestMonitorJSONConfigIsObject(t *testing.T) {
	m := &model.Monitor{
		ID:   7,
		Name: "svc",
		Type: "system-service",
		Raw: map[string]any{
			"id":                        int64(7),
			"name":                      "svc",
			"type":                      "system-service",
			"config":                    `{"systemServiceName":"nginx","customThing":3}`,
			"accepted_statuscodes_json": `["200-299"]`,
			"system_service_name":       "nginx",
		},
	}

	out := monitorJSON(m, nil)

	cfg, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("config 应为对象，实际 %T (%v)", out["config"], out["config"])
	}
	if cfg["systemServiceName"] != "nginx" {
		t.Fatalf("systemServiceName = %v", cfg["systemServiceName"])
	}
	if cfg["customThing"] != float64(3) {
		t.Fatalf("customThing = %v (%T)", cfg["customThing"], cfg["customThing"])
	}

	// accepted_statuscodes 仍然要还原成数组
	if codes, ok := out["accepted_statuscodes"].([]string); !ok || len(codes) != 1 {
		t.Fatalf("accepted_statuscodes = %#v", out["accepted_statuscodes"])
	}
}

// TestMonitorColumnsConfigSerialized 守住写库方向：对象要落成 JSON 字符串。
func TestMonitorColumnsConfigSerialized(t *testing.T) {
	cols, err := monitorColumns(map[string]any{
		"name": "svc",
		"type": "system-service",
		"config": map[string]any{
			"systemServiceName": "nginx",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, ok := cols["config"].(string)
	if !ok {
		t.Fatalf("config 应序列化为 string，实际 %T", cols["config"])
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("config 不是合法 JSON: %v (%s)", err, raw)
	}
	if back["systemServiceName"] != "nginx" {
		t.Fatalf("回读 systemServiceName = %v", back["systemServiceName"])
	}
}
