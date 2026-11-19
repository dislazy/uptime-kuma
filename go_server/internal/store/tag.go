package store

import (
	"context"
	"time"
)

// Tag 是标签。
type Tag struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	CreatedDate string `json:"createdDate"`
}

// TagStore 负责 tag 表。
type TagStore struct {
	db *DB
}

// NewTagStore 构造。
func NewTagStore(db *DB) *TagStore { return &TagStore{db: db} }

// List 返回全部标签。
func (s *TagStore) List(ctx context.Context) ([]Tag, error) {
	rows, err := s.db.Query(ctx, "SELECT * FROM tag ORDER BY id ASC")
	if err != nil {
		return nil, err
	}

	out := make([]Tag, 0, len(rows))
	for _, r := range rows {
		out = append(out, Tag{
			ID:          r.Int("id"),
			Name:        r.Str("name"),
			Color:       r.Str("color"),
			CreatedDate: pickStr(r, "created_date", "createdDate"),
		})
	}
	return out, nil
}

// Create 新建标签。
func (s *TagStore) Create(ctx context.Context, name, color string) (int64, error) {
	return s.db.Insert(ctx, "tag", map[string]any{
		"name":         name,
		"color":        color,
		"created_date": time.Now().UTC().Format("2006-01-02 15:04:05"),
	})
}

// Update 修改标签。
func (s *TagStore) Update(ctx context.Context, id int64, name, color string) error {
	return s.db.Update(ctx, "tag", id, map[string]any{"name": name, "color": color})
}

// Delete 删除标签及绑定关系。
func (s *TagStore) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM monitor_tag WHERE tag_id = ?", id); err != nil {
		return err
	}
	return s.db.Delete(ctx, "tag", id)
}
