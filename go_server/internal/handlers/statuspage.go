package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// slugPattern 只允许官方前端能生成的字符，避免路径穿越或非法 URL。
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// statusPagePayload 是前端 EditStatusPage.vue 提交的配置。
type statusPagePayload struct {
	Slug                  string   `json:"slug"`
	Title                 string   `json:"title"`
	Description           string   `json:"description"`
	Theme                 string   `json:"theme"`
	AutoRefreshInterval   int      `json:"autoRefreshInterval"`
	ShowTags              bool     `json:"showTags"`
	ShowPoweredBy         bool     `json:"showPoweredBy"`
	ShowCertificateExpiry bool     `json:"showCertificateExpiry"`
	ShowOnlyLastHeartbeat bool     `json:"showOnlyLastHeartbeat"`
	FooterText            string   `json:"footerText"`
	CustomCSS             string   `json:"customCSS"`
	AnalyticsID           string   `json:"analyticsId"`
	AnalyticsScriptURL    string   `json:"analyticsScriptUrl"`
	AnalyticsType         *string  `json:"analyticsType"`
	RSSTitle              string   `json:"rssTitle"`
	DomainNameList        []string `json:"domainNameList"`
}

var validAnalyticsTypes = map[string]bool{
	"google": true, "umami": true, "plausible": true, "matomo": true, "rybbit": true,
}

// statusPageJSON 生成前端 getStatusPage 期望的 config（对应官方 toJSON）。
func statusPageJSON(ctx context.Context, st *store.StatusPageStore, sp *store.StatusPage) map[string]any {
	icon := sp.Icon
	if icon == "" {
		icon = "/icon.svg"
	}

	domains, err := st.CNAMEs(ctx, sp.ID)
	if err != nil {
		logx.Warnf("statuspage", "读取域名列表失败(id=%d): %v", sp.ID, err)
		domains = []string{}
	}

	return map[string]any{
		"id":                    sp.ID,
		"slug":                  sp.Slug,
		"title":                 sp.Title,
		"description":           sp.Description,
		"icon":                  icon,
		"theme":                 sp.Theme,
		"autoRefreshInterval":   sp.AutoRefreshInterval,
		"published":             sp.Published,
		"showTags":              sp.ShowTags,
		"domainNameList":        domains,
		"customCSS":             sp.CustomCSS,
		"footerText":            sp.FooterText,
		"showPoweredBy":         sp.ShowPoweredBy,
		"analyticsId":           sp.AnalyticsID,
		"analyticsScriptUrl":    sp.AnalyticsScriptURL,
		"analyticsType":         nullableString(sp.AnalyticsType),
		"showCertificateExpiry": sp.ShowCertificateExpiry,
		"showOnlyLastHeartbeat": sp.ShowOnlyLastHeartbeat,
		"rssTitle":              sp.RSSTitle,
	}
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// addStatusPage 新建状态页。
func addStatusPage(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var title, slug string
	if err := c.Arg(0, &title); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &slug); err != nil {
		return nil, err
	}

	slug = strings.ToLower(strings.TrimSpace(slug))
	if !slugPattern.MatchString(slug) {
		return nil, errors.New("slug 只能包含小写字母、数字、下划线和短横线")
	}
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("请输入状态页标题")
	}

	exists, err := c.StatusPage.SlugExists(c.Ctx, slug, 0)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("slug %q 已被占用", slug)
	}

	if _, err := c.StatusPage.Create(c.Ctx, title, slug); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "msg": "successAdded", "msgi18n": true, "slug": slug}, nil
}

// saveStatusPage 保存状态页配置与公开分组。
func saveStatusPage(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}

	var p statusPagePayload
	if err := c.Arg(1, &p); err != nil {
		return nil, err
	}

	var imgDataURL string
	_ = c.OptArg(2, &imgDataURL)

	var groups []store.PublicGroup
	_ = c.OptArg(3, &groups)

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("No slug?")
	}

	newSlug := strings.ToLower(strings.TrimSpace(p.Slug))
	if !slugPattern.MatchString(newSlug) {
		return nil, errors.New("slug 只能包含小写字母、数字、下划线和短横线")
	}
	taken, err := c.StatusPage.SlugExists(c.Ctx, newSlug, sp.ID)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, fmt.Errorf("slug %q 已被占用", newSlug)
	}

	if p.AnalyticsType != nil && *p.AnalyticsType != "" && !validAnalyticsTypes[*p.AnalyticsType] {
		return nil, errors.New("Invalid analytics type")
	}

	sp.Slug = newSlug
	sp.Title = p.Title
	sp.Description = p.Description
	sp.Theme = p.Theme
	if p.AutoRefreshInterval > 0 {
		sp.AutoRefreshInterval = p.AutoRefreshInterval
	}
	sp.ShowTags = p.ShowTags
	sp.ShowPoweredBy = p.ShowPoweredBy
	sp.ShowCertificateExpiry = p.ShowCertificateExpiry
	sp.ShowOnlyLastHeartbeat = p.ShowOnlyLastHeartbeat
	sp.FooterText = p.FooterText
	sp.CustomCSS = p.CustomCSS
	sp.AnalyticsID = p.AnalyticsID
	sp.AnalyticsScriptURL = p.AnalyticsScriptURL
	if p.AnalyticsType != nil {
		sp.AnalyticsType = *p.AnalyticsType
	}
	sp.RSSTitle = p.RSSTitle

	// 图标：data URI 落盘，其它当普通 URL 处理
	if strings.HasPrefix(imgDataURL, "data:") {
		icon, err := saveStatusPageIcon(c, sp.ID, imgDataURL)
		if err != nil {
			return nil, err
		}
		sp.Icon = icon
	} else if imgDataURL != "" {
		sp.Icon = imgDataURL
	}

	if err := c.StatusPage.Save(c.Ctx, sp); err != nil {
		return nil, err
	}

	if err := c.StatusPage.SetCNAMEs(c.Ctx, sp.ID, p.DomainNameList); err != nil {
		return nil, err
	}

	if err := c.StatusPage.SetGroups(c.Ctx, sp.ID, groups); err != nil {
		return nil, err
	}

	// slug 变了要同步入口页设置，否则默认入口会指向旧地址
	if newSlug != slug {
		if cur := c.Set.GetString(c.Ctx, "entryPage", ""); cur == "statusPage-"+slug {
			if err := c.Set.Set(c.Ctx, "entryPage", "statusPage-"+newSlug, "general"); err != nil {
				logx.Warnf("statuspage", "更新入口页失败: %v", err)
			}
		}
	}

	return map[string]any{"ok": true, "publicGroupList": groups}, nil
}

// saveStatusPageIcon 把 data URI 写进 upload 目录，返回可访问路径。
func saveStatusPageIcon(c *Ctx, id int64, dataURL string) (string, error) {
	const pngPrefix = "data:image/png;base64,"
	if !strings.HasPrefix(dataURL, pngPrefix) {
		return "", errors.New("Only allowed PNG logo.")
	}

	raw, err := base64.StdEncoding.DecodeString(dataURL[len(pngPrefix):])
	if err != nil {
		return "", fmt.Errorf("logo 解析失败: %w", err)
	}
	if len(raw) > 2<<20 {
		return "", errors.New("logo 不能超过 2MB")
	}

	dir := c.Cfg.UploadDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	name := fmt.Sprintf("logo%d.png", id)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		return "", err
	}

	return fmt.Sprintf("/upload/%s?t=%d", name, time.Now().UnixMilli()), nil
}

// deleteStatusPage 删除状态页。
func deleteStatusPage(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("No slug?")
	}

	if err := c.StatusPage.Delete(c.Ctx, sp.ID); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// getStatusPage 取状态页配置。
func getStatusPage(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("No slug?")
	}

	return map[string]any{"ok": true, "config": statusPageJSON(c.Ctx, c.StatusPage, sp)}, nil
}

// ---------------- 事件公告 ----------------

type incidentPayload struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Style   string `json:"style"`
	Pin     bool   `json:"pin"`
	Active  bool   `json:"active"`
}

func incidentJSON(in *store.Incident) map[string]any {
	return map[string]any{
		"id":              in.ID,
		"title":           in.Title,
		"content":         in.Content,
		"style":           in.Style,
		"pin":             in.Pin,
		"active":          in.Active,
		"createdDate":     in.CreatedDate,
		"lastUpdatedDate": in.LastUpdatedDate,
		"status_page_id":  in.StatusPageID,
	}
}

// postIncident 新建或更新公告。
func postIncident(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}

	var p incidentPayload
	if err := c.Arg(1, &p); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("slug is not found")
	}

	if strings.TrimSpace(p.Title) == "" {
		return nil, errors.New("请输入公告标题")
	}
	if p.Style == "" {
		p.Style = "warning"
	}

	in := &store.Incident{
		ID:           p.ID,
		Title:        p.Title,
		Content:      p.Content,
		Style:        p.Style,
		Pin:          true,
		Active:       true,
		StatusPageID: sp.ID,
	}

	// 带 id 且属于本状态页 → 更新；否则新建
	if p.ID > 0 {
		existing, err := c.StatusPage.GetIncident(c.Ctx, p.ID)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.StatusPageID == sp.ID {
			if err := c.StatusPage.SaveIncident(c.Ctx, in); err != nil {
				return nil, err
			}
			updated, _ := c.StatusPage.GetIncident(c.Ctx, p.ID)
			if updated != nil {
				in = updated
			}
			return map[string]any{"ok": true, "incident": incidentJSON(in)}, nil
		}
	}

	id, err := c.StatusPage.CreateIncident(c.Ctx, in)
	if err != nil {
		return nil, err
	}
	in.ID = id
	if created, _ := c.StatusPage.GetIncident(c.Ctx, id); created != nil {
		in = created
	}

	return map[string]any{"ok": true, "incident": incidentJSON(in)}, nil
}

// editIncident 修改公告正文。
func editIncident(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	var incidentID int64
	var p incidentPayload
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &incidentID); err != nil {
		return nil, err
	}
	if err := c.Arg(2, &p); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("slug is not found")
	}

	in, err := c.StatusPage.GetIncident(c.Ctx, incidentID)
	if err != nil {
		return nil, err
	}
	if in == nil || in.StatusPageID != sp.ID {
		return nil, errors.New("公告不存在")
	}

	if strings.TrimSpace(p.Title) != "" {
		in.Title = p.Title
	}
	in.Content = p.Content
	if p.Style != "" {
		in.Style = p.Style
	}

	if err := c.StatusPage.SaveIncident(c.Ctx, in); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "incident": incidentJSON(in)}, nil
}

// deleteIncident 删除公告。
func deleteIncident(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	var incidentID int64
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &incidentID); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("slug is not found")
	}

	in, err := c.StatusPage.GetIncident(c.Ctx, incidentID)
	if err != nil {
		return nil, err
	}
	if in == nil || in.StatusPageID != sp.ID {
		return nil, errors.New("公告不存在")
	}

	if err := c.StatusPage.DeleteIncident(c.Ctx, incidentID); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// resolveIncident 把公告标记为已解决（active=0，同时取消置顶）。
func resolveIncident(c *Ctx) (any, error) {
	return setIncidentFlags(c, false, false, "successResolved")
}

// unpinIncident 取消置顶。
func unpinIncident(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("slug is not found")
	}

	// 官方未指定公告 id，语义是「把本状态页所有置顶公告取消置顶」
	if _, err := c.DB.Exec(c.Ctx,
		"UPDATE incident SET pin = 0, last_updated_date = ? WHERE status_page_id = ? AND pin = 1",
		store.Now(), sp.ID); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "msg": "successUnpinned", "msgi18n": true}, nil
}

func setIncidentFlags(c *Ctx, active, pin bool, msg string) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	var incidentID int64
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &incidentID); err != nil {
		return nil, err
	}

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("slug is not found")
	}

	in, err := c.StatusPage.GetIncident(c.Ctx, incidentID)
	if err != nil {
		return nil, err
	}
	if in == nil || in.StatusPageID != sp.ID {
		return nil, errors.New("公告不存在")
	}

	if err := c.DB.Update(c.Ctx, "incident", incidentID, map[string]any{
		"active":            active,
		"pin":               pin,
		"last_updated_date": store.Now(),
	}); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "msg": msg, "msgi18n": true}, nil
}

// getIncidentHistory 分页取历史公告。
func getIncidentHistory(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var slug string
	var cursor string
	if err := c.Arg(0, &slug); err != nil {
		return nil, err
	}
	_ = c.OptArg(1, &cursor)

	sp, err := c.StatusPage.GetBySlug(c.Ctx, slug)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, errors.New("slug is not found")
	}

	const pageSize = 20
	// 多取一条用于判断是否还有下一页
	list, err := c.StatusPage.IncidentsPaged(c.Ctx, sp.ID, cursor, pageSize+1)
	if err != nil {
		return nil, err
	}

	hasMore := false
	if len(list) > pageSize {
		hasMore = true
		list = list[:pageSize]
	}

	out := make([]map[string]any, 0, len(list))
	for _, in := range list {
		out = append(out, incidentJSON(in))
	}

	return map[string]any{"ok": true, "incidents": out, "hasMore": hasMore}, nil
}

// buildStatusPageList 对应官方 sendStatusPageList：{id: toJSON}，按 title 排序。
func buildStatusPageList(ctx context.Context, d *Deps) map[string]any {
	out := map[string]any{}
	if d.StatusPage == nil {
		return out
	}

	list, err := d.StatusPage.List(ctx)
	if err != nil {
		logx.Errorf("statuspage", "读取状态页列表失败: %v", err)
		return out
	}

	// 官方按 title 升序
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Title) < strings.ToLower(list[j].Title)
	})

	for _, sp := range list {
		out[strconv.FormatInt(sp.ID, 10)] = statusPageJSON(ctx, d.StatusPage, sp)
	}
	return out
}

// publishStatusPageList 配置变更后主动推一次列表。
func publishStatusPageList(d *Deps) {
	if d.Hub == nil {
		return
	}
	d.Hub.Broadcast("statusPageList", buildStatusPageList(context.Background(), d))
}
