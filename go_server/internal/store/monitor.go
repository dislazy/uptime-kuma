package store

import (
	"context"
	"fmt"

	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// MonitorStore 负责 monitor 表及其关联表（monitor_notification / monitor_tag）。
type MonitorStore struct {
	db *DB
}

// NewMonitorStore 构造。
func NewMonitorStore(db *DB) *MonitorStore { return &MonitorStore{db: db} }

// List 读取全部监控。includeInactive=false 时只返回启用的。
func (s *MonitorStore) List(ctx context.Context, includeInactive bool) ([]*model.Monitor, error) {
	q := "SELECT * FROM monitor"
	if !includeInactive {
		q += " WHERE active = 1"
	}
	q += " ORDER BY weight ASC, id ASC"

	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}

	out := make([]*model.Monitor, 0, len(rows))
	for _, r := range rows {
		m := toMonitor(r)
		if err := s.hydrate(ctx, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Get 按 ID 读取单个监控。
func (s *MonitorStore) Get(ctx context.Context, id int64) (*model.Monitor, error) {
	r, err := s.db.FindByID(ctx, "monitor", id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	m := toMonitor(r)
	if err := s.hydrate(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// FindByPushToken 按推送 token 查找监控。
// 官方列名是 pushToken，自建库是 push_token，两种都试。
func (s *MonitorStore) FindByPushToken(ctx context.Context, token string) (*model.Monitor, error) {
	if token == "" {
		return nil, nil
	}
	for _, col := range []string{"push_token", "pushToken"} {
		if len(s.db.Columns(ctx, "monitor")) > 0 && !s.db.Columns(ctx, "monitor")[col] {
			continue
		}
		r, err := s.db.QueryOne(ctx, "SELECT * FROM monitor WHERE ["+col+"] = ? LIMIT 1", token)
		if err != nil {
			return nil, err
		}
		if r != nil {
			m := toMonitor(r)
			if err := s.hydrate(ctx, m); err != nil {
				return nil, err
			}
			return m, nil
		}
	}
	return nil, nil
}

// Create 插入监控，返回新 ID。
func (s *MonitorStore) Create(ctx context.Context, values map[string]any) (int64, error) {
	return s.db.Insert(ctx, "monitor", values)
}

// Update 更新监控。
func (s *MonitorStore) Update(ctx context.Context, id int64, values map[string]any) error {
	return s.db.Update(ctx, "monitor", id, values)
}

// SetColumn 更新单个列，用于 dns_last_result、push_token 这类运行期字段。
func (s *MonitorStore) SetColumn(ctx context.Context, id int64, column string, value any) error {
	q := fmt.Sprintf("UPDATE monitor SET [%s] = ? WHERE id = ?", sanitizeIdent(column))
	_, err := s.db.Exec(ctx, q, normalizeValue(value), id)
	return err
}

// Delete 删除监控，并清理关联表。
// 官方库开启了外键级联，这里仍显式清理，兼容关闭外键的老库。
func (s *MonitorStore) Delete(ctx context.Context, id int64) error {
	return s.db.Tx(ctx, func(tx *Tx) error {
		for _, t := range []struct{ table, col string }{
			{"monitor_notification", "monitor_id"},
			{"monitor_tag", "monitor_id"},
			{"monitor_group", "monitor_id"},
			{"heartbeat", "monitor_id"},
			{"stat_minutely", "monitor_id"},
			{"stat_hourly", "monitor_id"},
			{"stat_daily", "monitor_id"},
		} {
			if !s.db.HasTable(ctx, t.table) {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf("DELETE FROM [%s] WHERE %s = ?", t.table, t.col), id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM monitor WHERE id = ?", id)
		return err
	})
}

// ResolveNotificationIDs 把前端的通知绑定关系写回关联表。
func (s *MonitorStore) ResolveNotificationIDs(ctx context.Context, monitorID int64, ids []int64) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM monitor_notification WHERE monitor_id = ?", monitorID); err != nil {
		return err
	}
	for _, nid := range ids {
		if _, err := s.db.Exec(ctx,
			"INSERT INTO monitor_notification (monitor_id, notification_id) VALUES (?, ?)", monitorID, nid); err != nil {
			return err
		}
	}
	return nil
}

// ResolveTagIDs 把前端的标签绑定关系写回关联表。
func (s *MonitorStore) ResolveTagIDs(ctx context.Context, monitorID int64, tags []model.TagValue) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM monitor_tag WHERE monitor_id = ?", monitorID); err != nil {
		return err
	}
	for _, t := range tags {
		if _, err := s.db.Exec(ctx,
			"INSERT INTO monitor_tag (monitor_id, tag_id, value) VALUES (?, ?, ?)", monitorID, t.TagID, t.Value); err != nil {
			return err
		}
	}
	return nil
}

// ResolveGroupIDs 把分组关系写回关联表。
func (s *MonitorStore) ResolveGroupIDs(ctx context.Context, monitorID int64, ids []int64) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM monitor_group WHERE monitor_id = ?", monitorID); err != nil {
		return err
	}
	for _, gid := range ids {
		if _, err := s.db.Exec(ctx,
			"INSERT INTO monitor_group (monitor_id, group_id, weight, send_url) VALUES (?, ?, 1000, 0)", monitorID, gid); err != nil {
			return err
		}
	}
	return nil
}

// hydrate 填充通知、标签、分组。
func (s *MonitorStore) hydrate(ctx context.Context, m *model.Monitor) error {
	if m == nil {
		return nil
	}

	nrows, err := s.db.Query(ctx,
		"SELECT notification_id FROM monitor_notification WHERE monitor_id = ?", m.ID)
	if err != nil {
		return err
	}
	m.Notifications = make([]int64, 0, len(nrows))
	for _, r := range nrows {
		m.Notifications = append(m.Notifications, r.Int("notification_id"))
	}

	trows, err := s.db.Query(ctx, `
		SELECT mt.tag_id, mt.value, t.name, t.color
		FROM monitor_tag mt JOIN tag t ON t.id = mt.tag_id
		WHERE mt.monitor_id = ?`, m.ID)
	if err != nil {
		return err
	}
	m.Tags = make([]model.TagValue, 0, len(trows))
	for _, r := range trows {
		m.Tags = append(m.Tags, model.TagValue{
			TagID: r.Int("tag_id"),
			Name:  r.Str("name"),
			Color: r.Str("color"),
			Value: r.Str("value"),
		})
	}

	grows, err := s.db.Query(ctx, "SELECT group_id FROM monitor_group WHERE monitor_id = ?", m.ID)
	if err != nil {
		return err
	}
	m.GroupIDs = make([]int64, 0, len(grows))
	for _, r := range grows {
		m.GroupIDs = append(m.GroupIDs, r.Int("group_id"))
	}

	// docker 监控需要额外把 docker_host 表里的连接信息解析进 Raw。
	// 官方是 R.load("docker_host", this.docker_host)，checker 侧只拿得到解析后的对象。
	if m.Type == "docker" {
		if hid := pickInt(m.Raw, "docker_host", "dockerHost"); hid > 0 {
			if info, err := s.DockerHostByID(ctx, hid); err == nil {
				checkers.SetDockerHost(m, info)
			}
		}
	}

	return nil
}

func toMonitor(r Row) *model.Monitor {
	m := &model.Monitor{
		ID:            r.Int("id"),
		Name:          r.Str("name"),
		Active:        r.Bool("active"),
		UserID:        pickInt(r, "user_id", "userId"),
		Type:          r.Str("type"),
		Interval:      int(r.Int("interval")),
		RetryInterval: int(pickInt(r, "retry_interval", "retryInterval")),
		MaxRetries:    int(r.Int("maxretries")),
		UpsideDown:    pickBool(r, "upside_down", "upsideDown"),
		Weight:        int(r.Int("weight")),
		PushToken:     pickStr(r, "push_token", "pushToken"),
		CreatedDate:   pickStr(r, "created_date", "createdDate"),
		Raw:           map[string]any(r),
	}
	if p, ok := r["parent"]; ok && p != nil {
		id := r.Int("parent")
		m.Parent = &id
	}
	if m.Interval <= 0 {
		m.Interval = 20
	}
	return m
}

// pickStr / pickInt / pickBool 在两种列名风格中取第一个存在的值。
func pickStr(r Row, keys ...string) string {
	for _, k := range keys {
		if v, ok := r[k]; ok && v != nil {
			return r.Str(k)
		}
	}
	return ""
}

func pickInt(r Row, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := r[k]; ok && v != nil {
			return r.Int(k)
		}
	}
	return 0
}

func pickBool(r Row, keys ...string) bool {
	for _, k := range keys {
		if v, ok := r[k]; ok && v != nil {
			return r.Bool(k)
		}
	}
	return false
}

// Children 返回某个分组的直接子监控。
//
// 不走 List() 是为了避免为每个子监控都做三次关联查询（通知/标签/分组）——
// 分组判状态只需要 id、name、active 和最后一条心跳。
func (s *MonitorStore) Children(ctx context.Context, parentID int64) ([]*model.Monitor, error) {
	rows, err := s.db.Query(ctx, "SELECT * FROM monitor WHERE parent = ? ORDER BY weight ASC, id ASC", parentID)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Monitor, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMonitor(r))
	}
	return out, nil
}
