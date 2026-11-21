package httpapi

// /api/openapi.json 与 /api/docs，对齐官方 server/routers/api-router.js:347-351：
//
//	GET /api/openapi.json → requireApiAccess（apiEnabled 设置 + 会话或 API Key）
//	GET /api/docs         → requireDocsAccess（同样的门禁，错误用纯文本说明）
//
// 文档内容对应官方 zod-openapi 生成的 OpenAPI 3.1.0 文档：
// 目前只桥接 /api/info 与 /api/monitors 两个操作（与官方一致）。
// /api/docs 用 @scalar/express-api-reference 同款页面：CDN 脚本 + data-url。

import (
	"net/http"

	"github.com/dislazy/uptime-kuma/go_server/internal/version"
)

// scalarDocsHTML 是 Scalar API Reference 的最小挂载页（与官方 apiReference 默认输出同构）。
const scalarDocsHTML = `<!doctype html>
<html>
  <head>
    <title>API Reference</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/api/openapi.json"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>`

// handleOpenAPISpec 对应 GET /api/openapi.json。
func (a *API) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if !a.Set.GetBool(r.Context(), "apiEnabled", false) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "msg": "The API is disabled"})
		return
	}
	if !a.hasAPIAccess(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "msg": "Missing or invalid API key"})
		return
	}
	writeJSON(w, http.StatusOK, openAPIDocument())
}

// handleAPIDocs 对应 GET /api/docs：门禁同 openapi.json，但错误用纯文本。
func (a *API) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	if !a.Set.GetBool(r.Context(), "apiEnabled", false) {
		writeText(w, http.StatusForbidden,
			`The API is disabled. Enable "API and MCP" in Settings -> General to use the API and this documentation.`)
		return
	}
	if !a.hasAPIAccess(r) {
		writeText(w, http.StatusUnauthorized, "Log in to Uptime Kuma to view the API documentation.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(scalarDocsHTML))
}

// hasAPIAccess 对齐官方 hasApiAccess：dashboard 会话（cookie）或有效 API Key。
func (a *API) hasAPIAccess(r *http.Request) bool {
	user, err := a.Auth.UserFromHeader(r.Context(), r.Header.Get("Cookie"))
	if err == nil && user != nil {
		return true
	}
	key := readAPIKey(r.Header.Get("Authorization"))
	if key == "" {
		return false
	}
	_, err = a.Auth.VerifyAPIKey(r.Context(), key)
	return err == nil
}

// writeText 输出纯文本响应（官方 response.type("text/plain").send(msg)）。
func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

// openAPIDocument 构建与官方 getOpenApiDocument 同构的 OpenAPI 3.1.0 文档。
//
// 官方用 zod-openapi 从路由声明生成；Go 侧静态声明，字段与官方
// infoSchema / monitorSchema / tagSchema / errorSchema 一一对应。
func openAPIDocument() map[string]any {
	errorSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"ok": map[string]any{"type": "boolean", "const": false}, "msg": map[string]any{"type": "string"}},
		"required":             []string{"ok", "msg"},
		"additionalProperties": false,
	}
	infoSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"primaryBaseURL":       map[string]any{"type": []string{"string", "null"}},
			"serverTimezone":       map[string]any{"type": "string"},
			"serverTimezoneOffset": map[string]any{"type": "string"},
			"version":              map[string]any{"type": "string"},
			"latestVersion":        map[string]any{"type": []string{"string", "null"}},
			"isContainer":          map[string]any{"type": "boolean"},
			"dbType":               map[string]any{"type": "string"},
			"runtime": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"platform": map[string]any{"type": "string"},
					"arch":     map[string]any{"type": "string"},
				},
				"required": []string{"platform", "arch"},
			},
		},
		"required": []string{
			"primaryBaseURL", "serverTimezone", "serverTimezoneOffset", "version",
			"latestVersion", "isContainer", "dbType", "runtime",
		},
	}
	tagSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tag_id":     map[string]any{"type": "number"},
			"monitor_id": map[string]any{"type": "number"},
			"value":      map[string]any{"type": []string{"string", "null"}},
			"name":       map[string]any{"type": "string"},
			"color":      map[string]any{"type": "string"},
		},
		"required": []string{"tag_id", "monitor_id", "value", "name", "color"},
	}
	monitorSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":          map[string]any{"type": "number"},
			"name":        map[string]any{"type": "string"},
			"type":        map[string]any{"type": "string"},
			"active":      map[string]any{"type": "boolean"},
			"parent":      map[string]any{"type": []string{"number", "null"}},
			"childrenIDs": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
			"tags":        map[string]any{"type": "array", "items": ref("#/components/schemas/Tag")},
			"path":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"pathName":    map[string]any{"type": "string"},
			"interval":    map[string]any{"type": "number"},
			"weight":      map[string]any{"type": []string{"number", "null"}},
		},
		"required": []string{
			"id", "name", "type", "active", "parent", "childrenIDs",
			"tags", "path", "pathName", "interval", "weight",
		},
	}

	errorResponse := func(desc string) map[string]any {
		return map[string]any{
			"description": desc,
			"content": map[string]any{
				"application/json": map[string]any{"schema": ref("#/components/schemas/Error")},
			},
		}
	}
	okResponse := func(desc string, props map[string]any) map[string]any {
		schema := map[string]any{
			"type":                 "object",
			"properties":           mergeMaps(map[string]any{"ok": map[string]any{"type": "boolean", "const": true}}, props),
			"required":             append([]string{"ok"}, keysOf(props)...),
			"additionalProperties": false,
		}
		return map[string]any{
			"description": desc,
			"content":     map[string]any{"application/json": map[string]any{"schema": schema}},
		}
	}
	standardErrors := map[string]any{
		"401": errorResponse("Missing or invalid API key"),
		"403": errorResponse("The API is disabled"),
		"429": errorResponse("Rate limit exceeded"),
	}

	operation := func(id, summary, desc string, responses map[string]any) map[string]any {
		return map[string]any{
			"operationId": id,
			"summary":     summary,
			"description": desc,
			"security":    []any{map[string]any{"bearerAuth": []any{}}},
			"responses":   mergeMaps(standardErrors, responses),
		}
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Uptime Kuma API",
			"version":     version.Version,
			"description": "Alpha. Requires an API key and the `API and MCP` setting to be enabled.",
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
			"schemas": map[string]any{
				"Error":   errorSchema,
				"Info":    infoSchema,
				"Tag":     tagSchema,
				"Monitor": monitorSchema,
			},
		},
		"paths": map[string]any{
			"/api/info": map[string]any{
				"get": operation("get_info", "Get the server information",
					"The same payload the dashboard receives from the `info` socket event.",
					map[string]any{
						"200": okResponse("Server information", map[string]any{
							"info": ref("#/components/schemas/Info"),
						}),
					}),
			},
			"/api/monitors": map[string]any{
				"get": operation("get_monitors", "Get all monitors",
					"The compact monitor list the dashboard receives after logging in, without secrets.",
					map[string]any{
						"200": okResponse("Monitor list", map[string]any{
							"monitors": map[string]any{"type": "array", "items": ref("#/components/schemas/Monitor")},
						}),
					}),
			},
		},
	}
}

func ref(path string) map[string]any { return map[string]any{"$ref": path} }

func mergeMaps(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
