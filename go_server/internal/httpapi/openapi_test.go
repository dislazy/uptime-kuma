package httpapi

import (
	"encoding/json"
	"testing"
)

// TestOpenAPIDocumentShape 校验文档结构与官方 zod-openapi 产物一致：
// 3.1.0、bearerAuth、两个 operation、Error/Info/Tag/Monitor 组件。
func TestOpenAPIDocumentShape(t *testing.T) {
	raw, err := json.Marshal(openAPIDocument())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Components struct {
			SecuritySchemes map[string]struct {
				Type   string `json:"type"`
				Scheme string `json:"scheme"`
			} `json:"securitySchemes"`
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			OperationID string                     `json:"operationId"`
			Security    []map[string][]any         `json:"security"`
			Responses   map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q", doc.OpenAPI)
	}
	if doc.Info.Title != "Uptime Kuma API" || doc.Info.Version == "" {
		t.Fatalf("info = %+v", doc.Info)
	}
	if doc.Components.SecuritySchemes["bearerAuth"].Scheme != "bearer" ||
		doc.Components.SecuritySchemes["bearerAuth"].Type != "http" {
		t.Fatalf("securitySchemes = %+v", doc.Components.SecuritySchemes)
	}
	for _, name := range []string{"Error", "Info", "Tag", "Monitor"} {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Fatalf("缺少 schema %s", name)
		}
	}

	cases := map[string]string{"/api/info": "get_info", "/api/monitors": "get_monitors"}
	for path, opID := range cases {
		get, ok := doc.Paths[path]["get"]
		if !ok {
			t.Fatalf("缺少 path %s", path)
		}
		if get.OperationID != opID {
			t.Fatalf("%s operationId = %q, want %q", path, get.OperationID, opID)
		}
		if len(get.Security) == 0 {
			t.Fatalf("%s 缺少 security", path)
		}
		// 官方每个操作都带 200 + 401/403/429
		for _, code := range []string{"200", "401", "403", "429"} {
			if _, ok := get.Responses[code]; !ok {
				t.Fatalf("%s 缺少响应 %s", path, code)
			}
		}
	}
}

// TestTimezoneOffset 对齐 dayjs().format("Z") 的形态。
func TestTimezoneOffset(t *testing.T) {
	got := timezoneOffset()
	if len(got) != 6 || (got[0] != '+' && got[0] != '-') || got[3] != ':' {
		t.Fatalf("offset 形态 = %q", got)
	}
}
