package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// handleBadge 生成 SVG 状态徽章。
//
// 覆盖官方的全部徽章路径：
//
//	/api/badge/:id/status
//	/api/badge/:id/uptime[/:duration]
//	/api/badge/:id/ping[/:duration]
//	/api/badge/:id/avg-response[/:duration]
//	/api/badge/:id/response
//	/api/badge/:id/cert-exp
func (a *API) handleBadge(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	metric := r.PathValue("metric")
	duration := r.PathValue("duration")

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid monitor id", http.StatusBadRequest)
		return
	}

	m, err := a.Mon.Get(r.Context(), id)
	if err != nil || m == nil {
		http.Error(w, "monitor not found", http.StatusNotFound)
		return
	}

	q := r.URL.Query()
	isPublic := a.monitorIsPublic(r, id)

	label := strings.TrimSpace(q.Get("label"))
	var (
		value string
		color string
	)

	switch metric {
	case "status":
		label = orDefault(label, "Status")
		if !isPublic {
			// 非公开监控（或监控不存在）在官方的行为是灰色 N/A，
			// 不能把私有监控的状态泄漏给任何人
			value, color = "N/A", naColor
			break
		}
		status := a.lastStatus(r, id)
		if v := q.Get("value"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				status = n
			}
		}
		value, color = statusBadge(status, q)

	case "uptime":
		durationLabel := "24h"
		if d := strings.TrimSpace(duration); d != "" {
			durationLabel = d
		}
		label = orDefault(label, fmt.Sprintf("Uptime (%s)", durationLabel))
		if !isPublic {
			value, color = "N/A", naColor
			break
		}
		w2 := a.windowFor(r, id, duration)
		value = fmt.Sprintf("%.2f%%", w2.Percent)
		color = orDefault(q.Get("color"), percentColor(w2.Percent))

	case "ping", "response", "avg-response":
		if label == "" {
			label = strings.TrimSpace(m.Name)
			if label == "" {
				label = "Response"
			}
		}
		if !isPublic {
			value, color = "N/A", naColor
			break
		}
		w2 := a.windowFor(r, id, duration)
		if w2.Count == 0 {
			value, color = "N/A", naColor
		} else {
			value = fmt.Sprintf("%.0f ms", w2.Ping)
			color = orDefault(q.Get("color"), pingColor(w2.Ping))
		}

	case "cert-exp":
		label = orDefault(label, "Cert Expiry")
		// 证书到期天数依赖 TLS 信息采集，采集不到时按官方返回 N/A
		value, color = "N/A", naColor

	default:
		http.Error(w, "unknown badge metric", http.StatusNotFound)
		return
	}

	svg := renderBadge(label, value, color)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}

// lastStatus 取最近一次心跳状态。
func (a *API) lastStatus(r *http.Request, monitorID int64) int {
	beats, err := a.Beat.Recent(r.Context(), monitorID, 1)
	if err != nil || len(beats) == 0 {
		return model.StatusPending
	}
	return beats[len(beats)-1].Status
}

// windowFor 按 duration 选择聚合表并返回统计窗口。
func (a *API) windowFor(r *http.Request, monitorID int64, duration string) (out struct {
	Up, Down, Count int64
	Ping            float64
	Percent         float64
}) {
	now := time.Now().Unix()

	table := "stat_minutely"
	from := now - 24*3600

	switch strings.TrimSpace(duration) {
	case "720", "30d":
		table, from = "stat_hourly", now-30*86400
	case "1y", "365":
		table, from = "stat_daily", now-365*86400
	case "", "24", "24h":
		// 默认 24 小时
	}

	w, err := a.Stat.Query(r.Context(), table, monitorID, from, now)
	if err != nil {
		return
	}
	out.Up, out.Down, out.Count, out.Ping = w.Up, w.Down, w.Count, w.Ping
	out.Percent = w.Percent()
	return
}

const (
	greenColor  = "#4c1"
	redColor    = "#e05d44"
	yellowColor = "#dfb317"
	blueColor   = "#007ec6"
	grayColor   = "#9f9f9f"
)

func statusText(status int) (string, string) {
	switch status {
	case model.StatusUp:
		return "up", greenColor
	case model.StatusDown:
		return "down", redColor
	case model.StatusMaintenance:
		return "maintenance", blueColor
	default:
		return "pending", yellowColor
	}
}

func percentColor(p float64) string {
	switch {
	case p >= 99:
		return greenColor
	case p >= 95:
		return yellowColor
	default:
		return redColor
	}
}

func pingColor(ms float64) string {
	switch {
	case ms < 300:
		return greenColor
	case ms < 1000:
		return yellowColor
	default:
		return redColor
	}
}

// renderBadge 生成 shields 风格的徽章 SVG。
//
// 宽度按字符数估算：shields 用 Verdana 11px，平均每个字符约 6.5px。
func renderBadge(label, value, color string) string {
	const (
		height   = 20
		padding  = 10
		charW    = 6.6
		labelPad = 2 * padding
	)

	labelW := int(float64(len(label))*charW) + labelPad
	valueW := int(float64(len(value))*charW) + labelPad
	totalW := labelW + valueW

	labelMid := labelW * 10 / 2
	valueMid := labelW*10 + valueW*10/2

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" role="img" aria-label="%s: %s">
  <title>%s: %s</title>
  <linearGradient id="s" x2="0" y2="100%%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r">
    <rect width="%d" height="%d" rx="3" fill="#fff"/>
  </clipPath>
  <g clip-path="url(#r)">
    <rect width="%d" height="%d" fill="#555"/>
    <rect x="%d" width="%d" height="%d" fill="%s"/>
    <rect width="%d" height="%d" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%d" y="14" fill="#010101" fill-opacity=".3">%s</text>
    <text x="%d" y="13">%s</text>
    <text x="%d" y="14" fill="#010101" fill-opacity=".3">%s</text>
    <text x="%d" y="13">%s</text>
  </g>
</svg>`,
		totalW, height, label, value,
		label, value,
		totalW, height,
		labelW, height,
		labelW, valueW, height, color,
		totalW, height,
		labelMid, label,
		labelMid, label,
		valueMid, value,
		valueMid, value,
	)
}

// 官方 badge 的默认色板（src/util.ts badgeConstants）
const (
	naColor                 = "#999"
	defaultUpColor          = "#66c20a"
	defaultDownColor        = "#c2290a"
	defaultPendingColor     = "#f8a306"
	defaultMaintenanceColor = "#1747f5"
)

// statusBadge 按状态取文案与颜色，支持 query 覆盖：
// upLabel / downLabel / pendingLabel / maintenanceLabel 与对应的 *Color。
func statusBadge(status int, q url.Values) (string, string) {
	switch status {
	case model.StatusUp:
		return orDefault(q.Get("upLabel"), "Up"), orDefault(q.Get("upColor"), defaultUpColor)
	case model.StatusDown:
		return orDefault(q.Get("downLabel"), "Down"), orDefault(q.Get("downColor"), defaultDownColor)
	case model.StatusPending:
		return orDefault(q.Get("pendingLabel"), "Pending"), orDefault(q.Get("pendingColor"), defaultPendingColor)
	case model.StatusMaintenance:
		return orDefault(q.Get("maintenanceLabel"), "Maintenance"), orDefault(q.Get("maintenanceColor"), defaultMaintenanceColor)
	default:
		return "N/A", naColor
	}
}

// monitorIsPublic 判断监控是否出现在某个已发布状态页的公开分组里。
func (a *API) monitorIsPublic(r *http.Request, monitorID int64) bool {
	if a.StatusPage == nil {
		return false
	}
	ids, err := a.StatusPage.PublicMonitorIDs(r.Context())
	if err != nil {
		return false
	}
	for _, id := range ids {
		if id == monitorID {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
