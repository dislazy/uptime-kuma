package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/store"
)

// handleStatusPageData 是状态页的公开配置接口。
// 对应官方 GET /api/status-page/:slug。
func (api *API) handleStatusPageData(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.PathValue("slug")))
	if slug == "" {
		writeError(w, http.StatusBadRequest, "Status Page Not Found")
		return
	}

	ctx := r.Context()

	sp, err := api.StatusPage.GetBySlug(ctx, slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sp == nil {
		writeError(w, http.StatusNotFound, "Status Page Not Found")
		return
	}

	writeJSON(w, http.StatusOK, api.buildStatusPageData(ctx, sp))
}

// handleStatusPageHeartbeat 返回状态页上所有监控的近期心跳与 24 小时可用率。
// 对应官方 GET /api/status-page/heartbeat/:slug。
func (api *API) handleStatusPageHeartbeat(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.PathValue("slug")))

	ctx := r.Context()

	sp, err := api.StatusPage.GetBySlug(ctx, slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 未发布的状态页不给数据（官方 slugToID 默认要求 published）
	if sp == nil || !sp.Published {
		writeError(w, http.StatusNotFound, "Status Page Not Found")
		return
	}

	monitorIDs, err := api.StatusPage.MonitorIDsForPage(ctx, sp.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	heartbeatList := make(map[string]any, len(monitorIDs))
	uptimeList := make(map[string]any, len(monitorIDs))

	since := time.Now().Add(-24 * time.Hour).Unix()

	for _, mid := range monitorIDs {
		beats, err := api.Beat.Recent(ctx, mid, 100)
		if err != nil {
			continue
		}

		// 官方按时间倒序取 100 条后 reverse 成正序
		list := make([]map[string]any, 0, len(beats))
		for i := len(beats) - 1; i >= 0; i-- {
			b := beats[i]
			list = append(list, map[string]any{
				"status": b.Status,
				"time":   b.Time,
				"msg":    "", // 公开页面不暴露错误详情
				"ping":   b.Ping,
			})
		}
		heartbeatList[strconv.FormatInt(mid, 10)] = list

		uptimeList[strconv.FormatInt(mid, 10)+"_24"] = api.uptime24(ctx, mid, since)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"heartbeatList": heartbeatList,
		"uptimeList":    uptimeList,
	})
}

// uptime24 用分钟级聚合算 24 小时可用率，缺失时回落到心跳表。
func (api *API) uptime24(ctx context.Context, monitorID int64, since int64) float64 {
	win, err := api.Stat.Query(ctx, "stat_minutely", monitorID, since, time.Now().Unix())
	if err == nil && win.Count > 0 {
		return round2(win.Percent())
	}

	counts, err := api.Beat.CountByStatus(ctx, monitorID, since)
	if err != nil || len(counts) == 0 {
		return 0
	}

	var up, total int64
	for _, c := range counts {
		switch c.Status {
		case 1: // UP
			up += c.Count
			total += c.Count
		case 0: // DOWN
			total += c.Count
		}
	}
	if total == 0 {
		return 0
	}
	return round2(float64(up) / float64(total) * 100)
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// handleStatusPageIncidentHistory 是公开的历史公告接口（游标分页）。
func (api *API) handleStatusPageIncidentHistory(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.PathValue("slug")))

	ctx := r.Context()

	sp, err := api.StatusPage.GetBySlug(ctx, slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sp == nil || !sp.Published {
		writeError(w, http.StatusNotFound, "Status Page Not Found")
		return
	}

	cursor := r.URL.Query().Get("cursor")

	list, err := api.StatusPage.IncidentsPaged(ctx, sp.ID, cursor, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]map[string]any, 0, len(list))
	for _, in := range list {
		out = append(out, map[string]any{
			"id":              in.ID,
			"title":           in.Title,
			"content":         in.Content,
			"style":           in.Style,
			"pin":             in.Pin,
			"active":          in.Active,
			"createdDate":     in.CreatedDate,
			"lastUpdatedDate": in.LastUpdatedDate,
			"status_page_id":  in.StatusPageID,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// handleStatusPageManifest 生成 PWA manifest。
func (api *API) handleStatusPageManifest(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.PathValue("slug")))
	ctx := r.Context()

	sp, err := api.StatusPage.GetBySlug(ctx, slug)
	if err != nil || sp == nil {
		writeError(w, http.StatusNotFound, "Status Page Not Found")
		return
	}

	icon := sp.Icon
	if icon == "" {
		icon = "/icon.svg"
	}

	base := strings.TrimSuffix(api.Cfg.BaseURL(), "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":             sp.Title,
		"short_name":       sp.Title,
		"start_url":        base + "/status/" + sp.Slug,
		"display":          "standalone",
		"background_color": "#18181b",
		"theme_color":      "#18181b",
		"icons": []map[string]any{
			{"src": base + icon, "sizes": "any", "type": "image/svg+xml"},
		},
	})
}

// buildStatusPageData 组装公开页面所需的全部数据。
func (api *API) buildStatusPageData(ctx context.Context, sp *store.StatusPage) map[string]any {
	config := map[string]any{
		"slug":                  sp.Slug,
		"title":                 sp.Title,
		"description":           sp.Description,
		"icon":                  fallback(sp.Icon, "/icon.svg"),
		"autoRefreshInterval":   sp.AutoRefreshInterval,
		"theme":                 sp.Theme,
		"published":             sp.Published,
		"showTags":              sp.ShowTags,
		"customCSS":             sp.CustomCSS,
		"footerText":            sp.FooterText,
		"showPoweredBy":         sp.ShowPoweredBy,
		"analyticsId":           sp.AnalyticsID,
		"analyticsScriptUrl":    sp.AnalyticsScriptURL,
		"analyticsType":         nullIfEmpty(sp.AnalyticsType),
		"showCertificateExpiry": sp.ShowCertificateExpiry,
		"showOnlyLastHeartbeat": sp.ShowOnlyLastHeartbeat,
		"rssTitle":              sp.RSSTitle,
	}

	// 仅展示置顶且未解决的公告
	incidents, err := api.StatusPage.Incidents(ctx, sp.ID, true)
	if err != nil {
		incidents = nil
	}
	incidentList := make([]map[string]any, 0, len(incidents))
	for _, in := range incidents {
		if !in.Pin {
			continue
		}
		incidentList = append(incidentList, map[string]any{
			"id":              in.ID,
			"title":           in.Title,
			"content":         in.Content,
			"style":           in.Style,
			"pin":             in.Pin,
			"active":          in.Active,
			"createdDate":     in.CreatedDate,
			"lastUpdatedDate": in.LastUpdatedDate,
			"status_page_id":  in.StatusPageID,
		})
	}

	groups, err := api.StatusPage.PublicGroups(ctx, sp.ID, sp.ShowTags)
	if err != nil {
		groups = nil
	}

	return map[string]any{
		"config":          config,
		"incidents":       incidentList,
		"publicGroupList": groups,
		"maintenanceList": api.maintenanceListForPage(ctx, sp.ID),
	}
}

// maintenanceListForPage 返回该状态页绑定且正在进行的维护窗口。
func (api *API) maintenanceListForPage(ctx context.Context, statusPageID int64) []any {
	out := []any{}
	if api.MaintMgr == nil {
		return out
	}

	if api.Maint == nil {
		return out
	}

	ids, err := api.Maint.MaintenanceIDsForStatusPage(ctx, statusPageID)
	if err != nil {
		return out
	}

	for _, mid := range ids {
		if api.MaintMgr.Status(mid) != store.MaintUnderMaintenance {
			continue
		}
		if data := api.MaintMgr.JSONOf(mid); data != nil {
			out = append(out, data)
		}
	}
	return out
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// handleStatusPageRoute 分发 /api/status-page/ 下的所有公开接口。
//
// 官方路径：
//
//	/api/status-page/:slug                      状态页配置 + 分组 + 公告
//	/api/status-page/heartbeat/:slug            心跳与可用率
//	/api/status-page/:slug/incident-history     历史公告
//	/api/status-page/:slug/manifest.json        PWA manifest
func (api *API) handleStatusPageRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/status-page/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "Status Page Not Found")
		return
	}

	parts := strings.Split(rest, "/")

	// /api/status-page/heartbeat/:slug
	if len(parts) == 2 && parts[0] == "heartbeat" {
		r.SetPathValue("slug", parts[1])
		api.handleStatusPageHeartbeat(w, r)
		return
	}

	if len(parts) == 1 {
		r.SetPathValue("slug", parts[0])
		api.handleStatusPageData(w, r)
		return
	}

	if len(parts) == 2 {
		switch parts[1] {
		case "incident-history":
			r.SetPathValue("slug", parts[0])
			api.handleStatusPageIncidentHistory(w, r)
			return
		case "manifest.json":
			r.SetPathValue("slug", parts[0])
			api.handleStatusPageManifest(w, r)
			return
		}
	}

	writeError(w, http.StatusNotFound, "Status Page Not Found")
}
