package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Settings 提供设置项读写。
//
// 兼容要点：官方把值 JSON.stringify 后存入 setting.value，
// 所以 "zh-CN" 实际存的是 `"zh-CN"`，数字存 `1`，对象存 `{...}`。
// 读取时必须先尝试 JSON 解析，失败才按原始字符串处理。
type Settings struct {
	db    *DB
	mu    sync.RWMutex
	cache map[string]string // key -> 原始 value（未解析）
	ttl   map[string]time.Time
}

// NewSettings 构造设置仓库。
func NewSettings(db *DB) *Settings {
	return &Settings{db: db, cache: map[string]string{}, ttl: map[string]time.Time{}}
}

// Raw 返回原始（未 JSON 解析）的字符串值。
func (s *Settings) Raw(ctx context.Context, key string) (string, bool, error) {
	s.mu.RLock()
	if v, ok := s.cache[key]; ok && time.Since(s.ttl[key]) < time.Minute {
		s.mu.RUnlock()
		return v, true, nil
	}
	s.mu.RUnlock()

	row, err := s.db.QueryOne(ctx, "SELECT value FROM setting WHERE `key` = ?", key)
	if err != nil {
		return "", false, err
	}
	if row == nil {
		return "", false, nil
	}

	v := row.Str("value")
	s.mu.Lock()
	s.cache[key] = v
	s.ttl[key] = time.Now()
	s.mu.Unlock()
	return v, true, nil
}

// Get 返回解析后的值（string / float64 / bool / map / slice）。
func (s *Settings) Get(ctx context.Context, key string) (any, error) {
	raw, ok, err := s.Raw(ctx, key)
	if err != nil || !ok {
		return nil, err
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw, nil
	}
	return v, nil
}

// GetString 返回字符串值，缺失时返回 def。
func (s *Settings) GetString(ctx context.Context, key, def string) string {
	v, err := s.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// GetBool 返回布尔值。
func (s *Settings) GetBool(ctx context.Context, key string, def bool) bool {
	v, err := s.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || t == "true"
	default:
		return def
	}
}

// GetInt 返回整数值。
func (s *Settings) GetInt(ctx context.Context, key string, def int64) int64 {
	v, err := s.Get(ctx, key)
	if err != nil || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return def
		}
		return n
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return def
	}
}

// Set 以官方格式（JSON 编码）写入设置。
func (s *Settings) Set(ctx context.Context, key string, value any, typeName string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("序列化设置 %s 失败: %w", key, err)
	}

	existing, err := s.db.QueryOne(ctx, "SELECT id FROM setting WHERE `key` = ?", key)
	if err != nil {
		return err
	}

	if existing == nil {
		_, err = s.db.Insert(ctx, "setting", map[string]any{
			"key":   key,
			"value": string(encoded),
			"type":  typeName,
		})
	} else {
		_, err = s.db.Exec(ctx, "UPDATE setting SET value = ? WHERE `key` = ?", string(encoded), key)
	}
	if err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.cache, key)
	s.mu.Unlock()
	return nil
}

// All 返回全部设置（解析后的 map），用于 socket 的 getSettings。
func (s *Settings) All(ctx context.Context) (map[string]any, error) {
	rows, err := s.db.Query(ctx, "SELECT `key`, value FROM setting")
	if err != nil {
		return nil, err
	}

	out := make(map[string]any, len(rows))
	for _, r := range rows {
		raw := r.Str("value")
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			out[r.Str("key")] = raw
			continue
		}
		out[r.Str("key")] = v

		s.mu.Lock()
		s.cache[r.Str("key")] = raw
		s.ttl[r.Str("key")] = time.Now()
		s.mu.Unlock()
	}
	return out, nil
}

// SetMany 批量写入同一 type 的设置，返回真正写成功的键。
// 对齐官方 setSettings：只有同 type 的键才允许覆盖。
func (s *Settings) SetMany(ctx context.Context, typeName string, data map[string]any) ([]string, error) {
	changed := make([]string, 0, len(data))

	for k, v := range data {
		encoded, err := json.Marshal(v)
		if err != nil {
			continue
		}

		row, err := s.db.QueryOne(ctx, "SELECT id, type FROM setting WHERE `key` = ?", k)
		if err != nil {
			return changed, err
		}

		if row == nil {
			if _, err := s.db.Insert(ctx, "setting", map[string]any{
				"key": k, "value": string(encoded), "type": typeName,
			}); err != nil {
				return changed, err
			}
		} else {
			if row.Str("type") != typeName {
				continue
			}
			if _, err := s.db.Exec(ctx, "UPDATE setting SET value = ? WHERE `key` = ?", string(encoded), k); err != nil {
				return changed, err
			}
		}
		changed = append(changed, k)
	}

	s.mu.Lock()
	for _, k := range changed {
		delete(s.cache, k)
	}
	s.mu.Unlock()
	return changed, nil
}

// SeedDefaults 写入首次启动需要的最小设置集合，缺失才写。
func (s *Settings) SeedDefaults(ctx context.Context, timezone string) error {
	defaults := map[string]struct {
		value    any
		typeName string
	}{
		"entryPage": {"statusPage-default", "general"},
		// 官方 DEFAULT_KEEP_PERIOD = 365（clear-old-data.js），
		// 2026-11 上游 #8213 把前端显示兜底从 180 改为 365，这里同步对齐。
		"keepDataPeriodDays": {365, "general"},
		"serverTimezone":     {timezone, "general"},
		"checkUpdate":        {true, "general"},
		"checkBeta":          {false, "general"},
		"trustProxy":         {false, "general"},
		"apiKey":             {"", "general"},
		"disableAuth":        {false, "general"},
		"databaseVersion":    {2, ""},
	}

	for k, d := range defaults {
		if _, ok, err := s.Raw(ctx, k); err != nil {
			return err
		} else if ok {
			continue
		}
		if err := s.Set(ctx, k, d.value, d.typeName); err != nil {
			return err
		}
	}
	return nil
}
