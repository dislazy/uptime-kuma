package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// Notification 是通知渠道配置。
// config 列存的是 JSON，例如：
//
//	{"type":"telegram","telegramBotToken":"xxx","telegramChatID":"123"}
type Notification struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Active    bool           `json:"active"`
	IsDefault bool           `json:"isDefault"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
}

// NotificationStore 负责 notification 表。
type NotificationStore struct {
	db *DB
}

// NewNotificationStore 构造。
func NewNotificationStore(db *DB) *NotificationStore { return &NotificationStore{db: db} }

// List 返回全部通知渠道。
func (s *NotificationStore) List(ctx context.Context) ([]*Notification, error) {
	rows, err := s.db.Query(ctx, "SELECT * FROM notification ORDER BY id ASC")
	if err != nil {
		return nil, err
	}

	out := make([]*Notification, 0, len(rows))
	for _, r := range rows {
		out = append(out, toNotification(r))
	}
	return out, nil
}

// Get 按 ID 读取。
func (s *NotificationStore) Get(ctx context.Context, id int64) (*Notification, error) {
	r, err := s.db.FindByID(ctx, "notification", id)
	if err != nil || r == nil {
		return nil, err
	}
	return toNotification(r), nil
}

// ForMonitor 返回绑定到某监控的通知渠道。
// 若监控没有显式绑定任何渠道，则回退到所有 is_default 的渠道。
func (s *NotificationStore) ForMonitor(ctx context.Context, monitorID int64) ([]*Notification, error) {
	rows, err := s.db.Query(ctx, `
		SELECT n.* FROM notification n
		JOIN monitor_notification mn ON mn.notification_id = n.id
		WHERE mn.monitor_id = ? AND n.active = 1
		ORDER BY n.id ASC`, monitorID)
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		rows, err = s.db.Query(ctx, "SELECT * FROM notification WHERE active = 1 AND is_default = 1")
		if err != nil {
			return nil, err
		}
	}

	out := make([]*Notification, 0, len(rows))
	for _, r := range rows {
		out = append(out, toNotification(r))
	}
	return out, nil
}

// Create 新建渠道，返回 ID。
func (s *NotificationStore) Create(ctx context.Context, values map[string]any) (int64, error) {
	return s.db.Insert(ctx, "notification", values)
}

// Update 更新渠道。
func (s *NotificationStore) Update(ctx context.Context, id int64, values map[string]any) error {
	return s.db.Update(ctx, "notification", id, values)
}

// Delete 删除渠道及其绑定关系。
func (s *NotificationStore) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM monitor_notification WHERE notification_id = ?", id); err != nil {
		return err
	}
	return s.db.Delete(ctx, "notification", id)
}

func toNotification(r Row) *Notification {
	n := &Notification{
		ID:        r.Int("id"),
		Name:      r.Str("name"),
		Active:    r.Bool("active"),
		IsDefault: r.Bool("is_default"),
	}

	raw := r.Str("config")
	if raw == "" {
		n.Config = map[string]any{}
		return n
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		n.Config = map[string]any{"_raw": raw, "_error": fmt.Sprintf("配置不是合法 JSON: %v", err)}
		return n
	}

	n.Config = cfg
	if t, ok := cfg["type"].(string); ok {
		n.Type = t
	}
	return n
}

// Flatten 返回前端期望的扁平结构。
//
// 官方把整个通知对象（name、type 以及各渠道字段）平铺后一起下发，
// 前端组件直接读 notification.telegramBotToken 这种顶层字段。
func (n *Notification) Flatten() map[string]any {
	out := make(map[string]any, len(n.Config)+6)
	for k, v := range n.Config {
		out[k] = v
	}
	out["id"] = n.ID
	out["name"] = n.Name
	out["active"] = n.Active
	out["isDefault"] = n.IsDefault
	out["type"] = n.Type
	delete(out, "config")
	return out
}

// EncodeConfig 把配置序列化为官方格式（含 type 字段）。
func EncodeConfig(cfg map[string]any, typ string) (string, error) {
	copied := make(map[string]any, len(cfg)+1)
	for k, v := range cfg {
		copied[k] = v
	}
	if typ != "" {
		copied["type"] = typ
	}
	b, err := json.Marshal(copied)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
