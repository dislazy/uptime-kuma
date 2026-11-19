package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// StatusPage 是 status_page 表的一行。
type StatusPage struct {
	ID          int64
	Slug        string
	Title       string
	Description string
	Icon        string
	Theme       string

	Published             bool
	SearchEngineIndex     bool
	ShowTags              bool
	ShowPoweredBy         bool
	ShowCertificateExpiry bool
	ShowOnlyLastHeartbeat bool

	Password           string
	CreatedDate        string
	ModifiedDate       string
	FooterText         string
	CustomCSS          string
	AnalyticsID        string
	AnalyticsScriptURL string
	AnalyticsType      string
	RSSTitle           string

	AutoRefreshInterval int
}

// Incident 是状态页上的事件公告。
type Incident struct {
	ID              int64
	Title           string
	Content         string
	Style           string
	CreatedDate     string
	LastUpdatedDate string
	Pin             bool
	Active          bool
	StatusPageID    int64
}

// PublicGroup 是状态页上的一个分组。
type PublicGroup struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Weight      int             `json:"weight"`
	MonitorList []PublicMonitor `json:"monitorList"`
}

// PublicMonitor 是分组里对外暴露的监控（只含必要字段）。
type PublicMonitor struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	SendURL   bool   `json:"sendUrl"`
	Type      string `json:"type"`
	URL       string `json:"url,omitempty"`
	CustomURL string `json:"-"`
	Tags      []Tag  `json:"tags,omitempty"`
}

// StatusPageStore 负责 status_page / group / monitor_group / incident。
type StatusPageStore struct {
	db *DB
}

// NewStatusPageStore 构造。
func NewStatusPageStore(db *DB) *StatusPageStore { return &StatusPageStore{db: db} }

func toStatusPage(r Row) *StatusPage {
	sp := &StatusPage{
		ID:          r.Int("id"),
		Slug:        r.Str("slug"),
		Title:       r.Str("title"),
		Description: r.Str("description"),
		Icon:        r.Str("icon"),
		Theme:       r.Str("theme"),

		Published:             r.Bool("published"),
		SearchEngineIndex:     r.Bool("search_engine_index"),
		ShowTags:              pickBool(r, "show_tags", "showTags"),
		ShowPoweredBy:         pickBool(r, "show_powered_by", "showPoweredBy"),
		ShowCertificateExpiry: pickBool(r, "show_certificate_expiry", "showCertificateExpiry"),
		ShowOnlyLastHeartbeat: pickBool(r, "show_only_last_heartbeat", "showOnlyLastHeartbeat"),

		Password:     r.Str("password"),
		CreatedDate:  pickStr(r, "created_date", "createdDate"),
		ModifiedDate: pickStr(r, "modified_date", "modifiedDate"),
		FooterText:   pickStr(r, "footer_text", "footerText"),
		CustomCSS:    pickStr(r, "custom_css", "customCSS"),

		// 老库可能还叫 google_analytics_tag_id
		AnalyticsID:        pickStr(r, "analytics_id", "google_analytics_tag_id"),
		AnalyticsScriptURL: pickStr(r, "analytics_script_url", "analyticsScriptUrl"),
		AnalyticsType:      pickStr(r, "analytics_type", "analyticsType"),
		RSSTitle:           pickStr(r, "rss_title", "rssTitle"),

		AutoRefreshInterval: int(pickInt(r, "auto_refresh_interval", "autoRefreshInterval")),
	}
	if sp.AutoRefreshInterval <= 0 {
		sp.AutoRefreshInterval = 300
	}
	return sp
}

// List 返回全部状态页。
func (s *StatusPageStore) List(ctx context.Context) ([]*StatusPage, error) {
	rows, err := s.db.Query(ctx, "SELECT * FROM status_page ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	out := make([]*StatusPage, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStatusPage(r))
	}
	return out, nil
}

// Get 按 id 取。
func (s *StatusPageStore) Get(ctx context.Context, id int64) (*StatusPage, error) {
	row, err := s.db.FindByID(ctx, "status_page", id)
	if err != nil || row == nil {
		return nil, err
	}
	return toStatusPage(row), nil
}

// GetBySlug 按 slug 取（官方一律转小写比较）。
func (s *StatusPageStore) GetBySlug(ctx context.Context, slug string) (*StatusPage, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT * FROM status_page WHERE slug = ?", strings.ToLower(strings.TrimSpace(slug)))
	if err != nil || row == nil {
		return nil, err
	}
	return toStatusPage(row), nil
}

// SlugExists 判断 slug 是否被别的状态页占用。
func (s *StatusPageStore) SlugExists(ctx context.Context, slug string, excludeID int64) (bool, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT id FROM status_page WHERE slug = ? AND id != ?", strings.ToLower(slug), excludeID)
	if err != nil {
		return false, err
	}
	return row != nil, nil
}

// Create 新建状态页。
func (s *StatusPageStore) Create(ctx context.Context, title, slug string) (int64, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return 0, errors.New("slug 不能为空")
	}

	now := Now()
	return s.db.Insert(ctx, "status_page", map[string]any{
		"slug":                     slug,
		"title":                    title,
		"description":              "",
		"icon":                     "",
		"theme":                    "auto",
		"published":                true,
		"search_engine_index":      true,
		"show_tags":                false,
		"show_powered_by":          true,
		"show_certificate_expiry":  false,
		"show_only_last_heartbeat": false,
		"auto_refresh_interval":    300,
		"created_date":             now,
		"modified_date":            now,
	})
}

// Save 更新状态页配置。
func (s *StatusPageStore) Save(ctx context.Context, sp *StatusPage) error {
	if sp.ID <= 0 {
		return errors.New("状态页 ID 缺失")
	}
	sp.ModifiedDate = Now()

	return s.db.Update(ctx, "status_page", sp.ID, map[string]any{
		"slug":                     strings.ToLower(sp.Slug),
		"title":                    sp.Title,
		"description":              sp.Description,
		"icon":                     sp.Icon,
		"theme":                    sp.Theme,
		"show_tags":                sp.ShowTags,
		"footer_text":              sp.FooterText,
		"custom_css":               sp.CustomCSS,
		"show_powered_by":          sp.ShowPoweredBy,
		"rss_title":                sp.UpstreamRSSTitle(),
		"show_only_last_heartbeat": sp.ShowOnlyLastHeartbeat,
		"show_certificate_expiry":  sp.ShowCertificateExpiry,
		"auto_refresh_interval":    sp.AutoRefreshInterval,
		"analytics_id":             sp.AnalyticsID,
		"analytics_script_url":     sp.AnalyticsScriptURL,
		"analytics_type":           sp.AnalyticsType,
		"modified_date":            sp.ModifiedDate,
	})
}

// UpstreamRSSTitle 单独抽出来是为了在调用处显式提醒：
// rss_title 列只在较新的库里存在，filterColumns 会自动跳过缺失的情况。
func (sp *StatusPage) UpstreamRSSTitle() string { return sp.RSSTitle }

// SetPublished 切换公开状态。
func (s *StatusPageStore) SetPublished(ctx context.Context, id int64, published bool) error {
	return s.db.Update(ctx, "status_page", id, map[string]any{
		"published":     published,
		"modified_date": Now(),
	})
}

// Delete 删除状态页及其分组、公告、域名绑定。
func (s *StatusPageStore) Delete(ctx context.Context, id int64) error {
	// 先删分组，再删 monitor_group 关联（外键已级联，但显式清更稳）
	if _, err := s.db.Exec(ctx,
		"DELETE FROM monitor_group WHERE group_id IN (SELECT id FROM [group] WHERE status_page_id = ?)", id); err != nil {
		return err
	}
	for _, q := range []string{
		"DELETE FROM [group] WHERE status_page_id = ?",
		"DELETE FROM incident WHERE status_page_id = ?",
		"DELETE FROM status_page_cname WHERE status_page_id = ?",
	} {
		if _, err := s.db.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	return s.db.Delete(ctx, "status_page", id)
}

// ---------------- 自定义域名 ----------------

// CNAMEs 返回绑定到状态页的域名列表。
func (s *StatusPageStore) CNAMEs(ctx context.Context, statusPageID int64) ([]string, error) {
	rows, err := s.db.Query(ctx,
		"SELECT domain FROM status_page_cname WHERE status_page_id = ? ORDER BY id", statusPageID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Str("domain"))
	}
	return out, nil
}

// SetCNAMEs 覆盖式设置域名列表。
func (s *StatusPageStore) SetCNAMEs(ctx context.Context, statusPageID int64, domains []string) error {
	if _, err := s.db.Exec(ctx,
		"DELETE FROM status_page_cname WHERE status_page_id = ?", statusPageID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if _, err := s.db.Insert(ctx, "status_page_cname", map[string]any{
			"status_page_id": statusPageID,
			"domain":         d,
		}); err != nil {
			return fmt.Errorf("绑定域名 %s 失败: %w", d, err)
		}
	}
	return nil
}

// DomainMapping 返回 domain -> slug 的映射，供 HTTP 层按 Host 定位状态页。
func (s *StatusPageStore) DomainMapping(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT c.domain AS domain, p.slug AS slug FROM status_page_cname c
		 JOIN status_page p ON c.status_page_id = p.id`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[strings.ToLower(r.Str("domain"))] = r.Str("slug")
	}
	return out, nil
}

// ---------------- 事件公告 ----------------

func toIncident(r Row) *Incident {
	return &Incident{
		ID:              r.Int("id"),
		Title:           r.Str("title"),
		Content:         r.Str("content"),
		Style:           r.Str("style"),
		CreatedDate:     pickStr(r, "created_date", "createdDate"),
		LastUpdatedDate: pickStr(r, "last_updated_date", "lastUpdatedDate"),
		Pin:             r.Bool("pin"),
		Active:          r.Bool("active"),
		StatusPageID:    pickInt(r, "status_page_id", "statusPageId"),
	}
}

// Incidents 取某状态页的全部公告，order 为 "pinned" 时置顶在前。
func (s *StatusPageStore) Incidents(ctx context.Context, statusPageID int64, onlyActive bool) ([]*Incident, error) {
	q := "SELECT * FROM incident WHERE status_page_id = ?"
	if onlyActive {
		q += " AND active = 1"
	}
	q += " ORDER BY pin DESC, created_date DESC"

	rows, err := s.db.Query(ctx, q, statusPageID)
	if err != nil {
		return nil, err
	}
	out := make([]*Incident, 0, len(rows))
	for _, r := range rows {
		out = append(out, toIncident(r))
	}
	return out, nil
}

// IncidentsPaged 按游标分页取公告（时间倒序）。
func (s *StatusPageStore) IncidentsPaged(ctx context.Context, statusPageID int64, cursor string, limit int) ([]*Incident, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	q := "SELECT * FROM incident WHERE status_page_id = ?"
	args := []any{statusPageID}
	if cursor != "" {
		q += " AND created_date < ?"
		args = append(args, cursor)
	}
	q += " ORDER BY created_date DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]*Incident, 0, len(rows))
	for _, r := range rows {
		out = append(out, toIncident(r))
	}
	return out, nil
}

// GetIncident 按 id 取公告。
func (s *StatusPageStore) GetIncident(ctx context.Context, id int64) (*Incident, error) {
	row, err := s.db.FindByID(ctx, "incident", id)
	if err != nil || row == nil {
		return nil, err
	}
	return toIncident(row), nil
}

// CreateIncident 新建公告。
func (s *StatusPageStore) CreateIncident(ctx context.Context, in *Incident) (int64, error) {
	in.CreatedDate = Now()
	in.LastUpdatedDate = in.CreatedDate
	return s.db.Insert(ctx, "incident", map[string]any{
		"title":             in.Title,
		"content":           in.Content,
		"style":             in.Style,
		"created_date":      in.CreatedDate,
		"last_updated_date": in.LastUpdatedDate,
		"pin":               in.Pin,
		"active":            in.Active,
		"status_page_id":    in.StatusPageID,
	})
}

// SaveIncident 更新公告正文。
func (s *StatusPageStore) SaveIncident(ctx context.Context, in *Incident) error {
	in.LastUpdatedDate = Now()
	in.CreatedDate = Now()
	return s.db.Update(ctx, "incident", in.ID, map[string]any{
		"title":             in.Title,
		"content":           in.Content,
		"style":             in.Style,
		"created_date":      in.CreatedDate,
		"last_updated_date": in.LastUpdatedDate,
		"pin":               in.Pin,
		"active":            in.Active,
	})
}

// UpdateIncidentColumn 改单个字段（置顶、已解决等）。
func (s *StatusPageStore) UpdateIncidentColumn(ctx context.Context, id int64, column string, value any) error {
	return s.db.Update(ctx, "incident", id, map[string]any{
		column:              value,
		"last_updated_date": Now(),
	})
}

// DeleteIncident 删除公告。
func (s *StatusPageStore) DeleteIncident(ctx context.Context, id int64) error {
	return s.db.Delete(ctx, "incident", id)
}

// ---------------- 公开视图 ----------------

// MonitorIDsForPage 返回状态页上所有公开分组的监控 id（去重）。
func (s *StatusPageStore) MonitorIDsForPage(ctx context.Context, statusPageID int64) ([]int64, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT mg.monitor_id AS mid
		 FROM monitor_group mg
		 JOIN [group] g ON mg.group_id = g.id
		 WHERE g.[public] = 1 AND g.status_page_id = ?`, statusPageID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Int("mid"))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// PublicGroups 组装状态页对外展示的分组树。
func (s *StatusPageStore) PublicGroups(ctx context.Context, statusPageID int64, showTags bool) ([]PublicGroup, error) {
	groups, err := s.db.Query(ctx,
		`SELECT id, name, weight FROM [group]
		 WHERE [public] = 1 AND status_page_id = ? ORDER BY weight ASC, id ASC`, statusPageID)
	if err != nil {
		return nil, err
	}

	out := make([]PublicGroup, 0, len(groups))
	for _, g := range groups {
		gid := g.Int("id")
		grp := PublicGroup{
			ID:          gid,
			Name:        g.Str("name"),
			Weight:      int(g.Int("weight")),
			MonitorList: []PublicMonitor{},
		}

		rows, err := s.db.Query(ctx,
			`SELECT m.id AS id, m.name AS name, m.type AS type, m.url AS url,
			        mg.send_url AS send_url, mg.custom_url AS custom_url
			 FROM monitor_group mg
			 JOIN monitor m ON mg.monitor_id = m.id
			 WHERE mg.group_id = ?
			 ORDER BY mg.weight ASC, m.id ASC`, gid)
		if err != nil {
			return nil, err
		}

		for _, r := range rows {
			pm := PublicMonitor{
				ID:        r.Int("id"),
				Name:      r.Str("name"),
				Type:      r.Str("type"),
				SendURL:   r.Bool("send_url"),
				CustomURL: r.Str("custom_url"),
			}
			if pm.SendURL {
				pm.URL = pm.CustomURL
				if pm.URL == "" {
					pm.URL = r.Str("url")
				}
			}
			if showTags {
				tags, err := s.tagsOf(ctx, pm.ID)
				if err == nil {
					pm.Tags = tags
				}
			}
			grp.MonitorList = append(grp.MonitorList, pm)
		}

		out = append(out, grp)
	}
	return out, nil
}

func (s *StatusPageStore) tagsOf(ctx context.Context, monitorID int64) ([]Tag, error) {
	rows, err := s.db.Query(ctx,
		`SELECT t.id AS id, t.name AS name, t.color AS color, t.created_date AS created_date
		 FROM monitor_tag mt JOIN tag t ON mt.tag_id = t.id
		 WHERE mt.monitor_id = ? ORDER BY t.id`, monitorID)
	if err != nil {
		return nil, err
	}
	out := make([]Tag, 0, len(rows))
	for _, r := range rows {
		out = append(out, Tag{
			ID:          r.Int("id"),
			Name:        r.Str("name"),
			Color:       r.Str("color"),
			CreatedDate: r.Str("created_date"),
		})
	}
	return out, nil
}

// SetGroups 覆盖式保存状态页的公开分组（对应官方 saveStatusPage 的 publicGroupList 处理）。
//
// 分组按顺序重排 weight；分组内的监控同样按顺序写 weight。
// 返回值会把新分配的 group id 回填到入参上。
func (s *StatusPageStore) SetGroups(ctx context.Context, statusPageID int64, groups []PublicGroup) error {
	keep := make([]int64, 0, len(groups))

	for gi := range groups {
		g := &groups[gi]

		var gid int64
		if g.ID > 0 {
			// 只允许改属于本状态页的公开分组
			row, err := s.db.QueryOne(ctx,
				"SELECT id FROM [group] WHERE id = ? AND [public] = 1 AND status_page_id = ?",
				g.ID, statusPageID)
			if err != nil {
				return err
			}
			if row != nil {
				gid = g.ID
			}
		}

		if gid > 0 {
			if err := s.db.Update(ctx, "group", gid, map[string]any{
				"name":           g.Name,
				"public":         true,
				"weight":         gi + 1,
				"status_page_id": statusPageID,
			}); err != nil {
				return err
			}
		} else {
			var err error
			gid, err = s.db.Insert(ctx, "group", map[string]any{
				"name":           g.Name,
				"public":         true,
				"weight":         gi + 1,
				"status_page_id": statusPageID,
			})
			if err != nil {
				return err
			}
		}

		g.ID = gid
		keep = append(keep, gid)

		if _, err := s.db.Exec(ctx, "DELETE FROM monitor_group WHERE group_id = ?", gid); err != nil {
			return err
		}
		for mi, m := range g.MonitorList {
			if _, err := s.db.Insert(ctx, "monitor_group", map[string]any{
				"group_id":   gid,
				"monitor_id": m.ID,
				"weight":     mi + 1,
				"send_url":   m.SendURL,
				"custom_url": m.CustomURL,
			}); err != nil {
				return err
			}
		}
	}

	// 清掉本次没出现的分组
	if len(keep) == 0 {
		if _, err := s.db.Exec(ctx, "DELETE FROM [group] WHERE status_page_id = ?", statusPageID); err != nil {
			return err
		}
		return nil
	}

	marks := make([]string, len(keep))
	args := make([]any, 0, len(keep)+1)
	for i, id := range keep {
		marks[i] = "?"
		args = append(args, id)
	}
	args = append(args, statusPageID)

	_, err := s.db.Exec(ctx,
		"DELETE FROM [group] WHERE id NOT IN ("+strings.Join(marks, ",")+") AND status_page_id = ?", args...)
	return err
}

// PublicMonitorIDs 返回所有公开分组里的监控 id（去重）。
// 徽章端用它判断"这个监控能不能对外展示状态"。
func (s *StatusPageStore) PublicMonitorIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT mg.monitor_id AS mid
		 FROM monitor_group mg
		 JOIN [group] g ON mg.group_id = g.id
		 WHERE g.[public] = 1`)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Int("mid"))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
