package httpapi

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// 状态页 RSS。对应官方 GET /status/:slug/rss。
//
// 内容与官方一致：置顶且未解决的公告 + 当前处于 DOWN 的监控，
// channel 描述是"当前整体状态"（All Systems Operational 之类）。
const (
	statusPageAllDown     = 0
	statusPageAllUp       = 1
	statusPagePartialDown = 2
	statusPageMaintenance = 3
	statusPageNoServices  = -1
)

// handleStatusPageRSS 输出 RSS 2.0。
func (api *API) handleStatusPageRSS(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	sp, err := api.StatusPage.GetBySlug(r.Context(), slug)
	if err != nil || sp == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Status Page Not Found"))
		return
	}

	feedURL := api.statusPageFeedURL(r, slug)
	heartbeats := api.statusPageHeartbeats(r.Context(), sp.ID, sp.ShowTags)

	overall := overallStatus(heartbeats)
	description := statusDescription(overall)

	title := "Uptime Kuma RSS Feed"
	if strings.TrimSpace(sp.RSSTitle) != "" {
		title = sp.RSSTitle
	} else if strings.TrimSpace(sp.Title) != "" {
		title = sp.Title + " RSS Feed"
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom">` + "\n")
	b.WriteString("  <channel>\n")
	b.WriteString("    <title>" + xmlEscape(title) + "</title>\n")
	b.WriteString("    <description>" + xmlEscape("Current status: "+description) + "</description>\n")
	b.WriteString("    <link>" + xmlEscape(feedURL) + "</link>\n")
	b.WriteString(`    <atom:link href="` + xmlEscape(feedURL) + `" rel="self" type="application/rss+xml"/>` + "\n")
	b.WriteString("    <language>en</language>\n")
	b.WriteString("    <lastBuildDate>" + rssDate(time.Now()) + "</lastBuildDate>\n")
	b.WriteString("    <generator>Uptime Kuma Go</generator>\n")

	// 公告
	incidents, err := api.StatusPage.Incidents(r.Context(), sp.ID, true)
	if err == nil {
		for _, in := range incidents {
			if !in.Pin {
				continue
			}
			date := in.LastUpdatedDate
			if date == "" {
				date = in.CreatedDate
			}
			b.WriteString("    <item>\n")
			b.WriteString("      <title>" + xmlEscape(in.Title) + "</title>\n")
			b.WriteString("      <description>" + xmlEscape(in.Content) + "</description>\n")
			b.WriteString("      <guid isPermaLink=\"false\">" + xmlEscape(fmt.Sprintf("i%d-%s", in.ID, date)) + "</guid>\n")
			b.WriteString("      <link>" + xmlEscape(feedURL) + "</link>\n")
			b.WriteString("      <pubDate>" + rssDate(parseServerTime(date)) + "</pubDate>\n")
			b.WriteString("    </item>\n")
		}
	}

	// 当前 DOWN 的监控
	for _, hb := range heartbeats {
		if hb.Status != model.StatusDown {
			continue
		}
		b.WriteString("    <item>\n")
		b.WriteString("      <title>" + xmlEscape(hb.Name+" is down") + "</title>\n")
		b.WriteString("      <description>" + xmlEscape(hb.Name+" has been down since "+hb.Time+" UTC") + "</description>\n")
		b.WriteString("      <guid isPermaLink=\"false\">" + xmlEscape(fmt.Sprintf("%d-%s", hb.ID, hb.Time)) + "</guid>\n")
		b.WriteString("      <link>" + xmlEscape(feedURL) + "</link>\n")
		b.WriteString("      <pubDate>" + rssDate(parseServerTime(hb.Time)) + "</pubDate>\n")
		b.WriteString("    </item>\n")
	}

	b.WriteString("  </channel>\n</rss>\n")

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// pageHeartbeat 是 RSS 需要的最小心跳视图。
type pageHeartbeat struct {
	ID     int64
	Name   string
	Status int
	Time   string
}

// statusPageHeartbeats 取状态页上每个监控的最新一条心跳。
func (api *API) statusPageHeartbeats(ctx context.Context, statusPageID int64, showTags bool) []pageHeartbeat {
	groups, err := api.StatusPage.PublicGroups(ctx, statusPageID, showTags)
	if err != nil {
		return nil
	}

	out := make([]pageHeartbeat, 0, len(groups))
	seen := map[int64]bool{}
	for _, g := range groups {
		for _, m := range g.MonitorList {
			// 同一个监控可能被挂在多个公开分组里，RSS 只保留一条
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			beats, err := api.Beat.Recent(ctx, m.ID, 1)
			if err != nil || len(beats) == 0 {
				continue
			}
			last := beats[len(beats)-1]
			out = append(out, pageHeartbeat{
				ID:     m.ID,
				Name:   m.Name,
				Status: last.Status,
				Time:   last.Time,
			})
		}
	}
	return out
}

// overallStatus 与官方 StatusPage.overallStatus 完全一致。
func overallStatus(beats []pageHeartbeat) int {
	if len(beats) == 0 {
		return statusPageNoServices
	}
	status := statusPageAllUp
	hasUp := false
	for _, b := range beats {
		switch b.Status {
		case model.StatusMaintenance:
			return statusPageMaintenance
		case model.StatusUp:
			hasUp = true
		default:
			status = statusPagePartialDown
		}
	}
	if !hasUp {
		status = statusPageAllDown
	}
	return status
}

func statusDescription(status int) string {
	switch status {
	case statusPageNoServices:
		return "No Services"
	case statusPageAllUp:
		return "All Systems Operational"
	case statusPagePartialDown:
		return "Partially Degraded Service"
	case statusPageAllDown:
		return "Degraded Service"
	case statusPageMaintenance:
		return "Under maintenance"
	default:
		return "?"
	}
}

// statusPageFeedURL 还原对外可访问的 RSS 地址（支持反向代理头）。
func (api *API) statusPageFeedURL(r *http.Request, slug string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host

	if api.Cfg.TrustProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = strings.TrimSpace(strings.Split(p, ",")[0])
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return fmt.Sprintf("%s://%s/status/%s/rss", scheme, host, slug)
}

// parseServerTime 解析库里存的时间（统一 "2006-01-02 15:04:05"，按 UTC 解释）。
func parseServerTime(s string) time.Time {
	// 库里可能是 "2006-01-02 15:04:05"，也可能带毫秒（redbean 的格式）
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04:05.000", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t
		}
	}
	return time.Now().UTC()
}

func rssDate(t time.Time) string {
	return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// xmlEscape 转义 XML 文本节点。
func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
