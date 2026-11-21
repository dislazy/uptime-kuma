package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RemoteBrowser 对应官方 remote_browser 表（官方 knex 建表，name/url/user_id）。
// 存放 Playwright 兼容的浏览器服务器 WS 地址（如 browserless 容器或
// playwright.chromium.launchServer() 起的端点）。
type RemoteBrowser struct {
	ID   int64
	Name string
	URL  string
}

// JSON 返回给前端的形态，字段名与官方 model/remote_browser.js toJSON() 一致。
func (rb RemoteBrowser) JSON() map[string]any {
	return map[string]any{
		"id":   rb.ID,
		"url":  rb.URL,
		"name": rb.Name,
	}
}

// RemoteBrowsers 列出全部远程浏览器（官方 sendRemoteBrowserList 用 R.find 全表）。
func (s *MonitorStore) RemoteBrowsers(ctx context.Context) ([]RemoteBrowser, error) {
	if !s.db.HasTable(ctx, "remote_browser") {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, "SELECT * FROM remote_browser ORDER BY id")
	if err != nil {
		return nil, err
	}
	out := make([]RemoteBrowser, 0, len(rows))
	for _, r := range rows {
		out = append(out, RemoteBrowser{
			ID:   r.Int("id"),
			Name: r.Str("name"),
			URL:  r.Str("url"),
		})
	}
	return out, nil
}

// SaveRemoteBrowser 新建或更新，语义对齐官方 RemoteBrowser.save：
// remoteBrowserID 存在即更新，找不到报 "Remote browser not found"。
func (s *MonitorStore) SaveRemoteBrowser(ctx context.Context, id int64, userID, name, url string) (int64, error) {
	if !s.db.HasTable(ctx, "remote_browser") {
		return 0, errors.New("remote_browser table is missing")
	}

	values := map[string]any{
		"name": name,
		"url":  url,
	}
	if userID != "" {
		values["user_id"] = userID
	}

	if id > 0 {
		found, err := s.db.Query(ctx, "SELECT id FROM remote_browser WHERE id = ?", id)
		if err != nil {
			return 0, err
		}
		if len(found) == 0 {
			return 0, errors.New("Remote browser not found")
		}
		if err := s.db.Update(ctx, "remote_browser", id, values); err != nil {
			return 0, err
		}
		return id, nil
	}
	return s.db.Insert(ctx, "remote_browser", values)
}

// DeleteRemoteBrowser 删除并把引用它的监控置空（官方 Proxy.delete 同款级联）。
func (s *MonitorStore) DeleteRemoteBrowser(ctx context.Context, id int64) error {
	if !s.db.HasTable(ctx, "remote_browser") {
		return errors.New("remote_browser table is missing")
	}
	found, err := s.db.Query(ctx, "SELECT id FROM remote_browser WHERE id = ?", id)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return errors.New("Remote browser not found")
	}

	if col := s.monitorRemoteBrowserColumn(ctx); col != "" {
		if _, err := s.db.Exec(ctx, fmt.Sprintf(
			"UPDATE monitor SET %s = NULL WHERE %s = ?", col, col), id); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(ctx, "DELETE FROM remote_browser WHERE id = ?", id)
	return err
}

// RemoteBrowserURLByID 按 ID 取 WS 端点，供 real-browser 探测时解析
// （官方 getRemoteBrowser → RemoteBrowser.get，每次探测现查）。
func (s *MonitorStore) RemoteBrowserURLByID(ctx context.Context, id int64) (string, error) {
	if id <= 0 || !s.db.HasTable(ctx, "remote_browser") {
		return "", fmt.Errorf("Remote browser %d not found", id)
	}
	rows, err := s.db.Query(ctx, "SELECT url FROM remote_browser WHERE id = ?", id)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("Remote browser %d not found", id)
	}
	url := strings.TrimSpace(rows[0].Str("url"))
	if url == "" {
		return "", fmt.Errorf("Remote browser %d has no url", id)
	}
	return url, nil
}

// monitorRemoteBrowserColumn 解析 monitor 表里 remote_browser 外键的真实列名。
func (s *MonitorStore) monitorRemoteBrowserColumn(ctx context.Context) string {
	cols := s.db.Columns(ctx, "monitor")
	return resolveColumn(cols, "remote_browser")
}
