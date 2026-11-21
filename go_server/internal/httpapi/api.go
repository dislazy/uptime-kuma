package httpapi

// API Key 鉴权的 REST 接口，对齐官方 server/routers/api-router.js：
//
//	GET /api/info     → {ok:true, info:{...}}        与 socket info 事件同构
//	GET /api/monitors → {ok:true, monitors:[...]}    紧凑监控列表，不带敏感字段
//
// 鉴权语义对齐官方 requireApiKey：
//   - `apiEnabled` 设置未开 → 403 "The API is disabled"
//   - 全局限流 60 次/分钟                    → 429 "Too frequently, try again later."
//   - Authorization 头缺 key                → 401 "Missing API key"
//   - key 无效                              → 401 "Invalid API key"
//
// key 的读取与官方 readApiKey 一致：取 Authorization 头最后一个空格后的 token，
// 所以 "Bearer <key>" 与裸 key 两种写法都支持。

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/version"
)

// apiRateLimiter 官方 apiRateLimiter 为每分钟 60 个 token。
var apiRateLimiter = newWindowLimiter(60, 60)

// MountAPIRoutes 挂需要 API Key 的 REST 路由（与公开路由分开，便于阅读）。
func MountAPIRoutes(mux *http.ServeMux, api *API) {
	mux.HandleFunc("GET /api/info", api.handleAPIInfo)
	mux.HandleFunc("GET /api/monitors", api.handleAPIMonitors)
}

// requireAPIKey 鉴权中间件，通过返回 true。
func (a *API) requireAPIKey(w http.ResponseWriter, r *http.Request) bool {
	if !a.Set.GetBool(r.Context(), "apiEnabled", false) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "msg": "The API is disabled"})
		return false
	}
	// 官方语义：被拒绝的请求也消耗一个 token
	if !apiRateLimiter.allow("") {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "msg": "Too frequently, try again later."})
		return false
	}

	key := readAPIKey(r.Header.Get("Authorization"))
	if key == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "msg": "Missing API key"})
		return false
	}
	if _, err := a.Auth.VerifyAPIKey(r.Context(), key); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "msg": "Invalid API key"})
		return false
	}
	return true
}

// readAPIKey 对齐官方 readApiKey：取头里最后一个空格分隔的 token。
func readAPIKey(authorization string) string {
	authorization = strings.TrimSpace(authorization)
	if authorization == "" {
		return ""
	}
	parts := strings.Split(authorization, " ")
	return parts[len(parts)-1]
}

// handleAPIInfo 对应 GET /api/info，与官方 getInfo() 同构。
func (a *API) handleAPIInfo(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPIKey(w, r) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"info": map[string]any{
			"version":              version.Version,
			"latestVersion":        version.Version,
			"primaryBaseURL":       a.Set.GetString(r.Context(), "primaryBaseURL", ""),
			"serverTimezone":       a.Set.GetString(r.Context(), "serverTimezone", "UTC"),
			"serverTimezoneOffset": timezoneOffset(),
			"isContainer":          true,
			"dbType":               "sqlite",
			"runtime": map[string]any{
				"platform": runtime.GOOS,
				"arch":     runtime.GOARCH,
			},
		},
	})
}

// timezoneOffset 对齐官方 dayjs().format("Z")：+08:00 形式。
func timezoneOffset() string {
	_, secs := time.Now().Zone()
	sign := "+"
	if secs < 0 {
		sign = "-"
		secs = -secs
	}
	return fmt.Sprintf("%s%02d:%02d", sign, secs/3600, (secs%3600)/60)
}

// handleAPIMonitors 对应 GET /api/monitors，返回官方 toCompactMonitor 形状：
// 只带 id/name/type/active/parent/childrenIDs/tags/path/pathName/interval/weight，
// 不含 url、密码、通知参数等敏感字段。
func (a *API) handleAPIMonitors(w http.ResponseWriter, r *http.Request) {
	if !a.requireAPIKey(w, r) {
		return
	}

	list, err := a.Mon.List(r.Context(), true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "msg": "Failed to get the monitors"})
		return
	}

	children := map[int64][]int64{}
	names := map[int64]string{}
	for _, m := range list {
		names[m.ID] = m.Name
		if m.Parent != nil {
			children[*m.Parent] = append(children[*m.Parent], m.ID)
		}
	}
	byID := map[int64]int{}
	for i, m := range list {
		byID[m.ID] = i
	}

	monitors := make([]map[string]any, 0, len(list))
	for _, m := range list {
		// path：从根到父级的名字列表（官方 compact-monitor 同构）
		var path []string
		var parent any
		if m.Parent != nil {
			parent = *m.Parent
			chain := []int64{}
			cur := m.Parent
			for cur != nil && len(chain) <= 64 { // 深度上限，防脏数据成环死循环
				chain = append(chain, *cur)
				if idx, ok := byID[*cur]; ok {
					cur = list[idx].Parent
				} else {
					cur = nil
				}
			}
			for i := len(chain) - 1; i >= 0; i-- {
				path = append(path, names[chain[i]])
			}
		}
		if path == nil {
			path = []string{}
		}
		ids := children[m.ID]
		if ids == nil {
			ids = []int64{}
		}

		tags := make([]map[string]any, 0, len(m.Tags))
		for _, t := range m.Tags {
			tags = append(tags, map[string]any{
				"tag_id":     t.TagID,
				"monitor_id": m.ID,
				"name":       t.Name,
				"color":      t.Color,
				"value":      t.Value,
			})
		}

		monitors = append(monitors, map[string]any{
			"id":          m.ID,
			"name":        m.Name,
			"type":        m.Type,
			"active":      m.Active,
			"parent":      parent,
			"childrenIDs": ids,
			"tags":        tags,
			"path":        path,
			"pathName":    strings.Join(path, " / "),
			"interval":    m.Interval,
			"weight":      m.Weight,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "monitors": monitors})
}
