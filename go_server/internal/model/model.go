// Package model 定义跨包共享的领域类型。
// 该包不依赖任何其他内部包，避免循环引用。
package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// 监控状态，取值必须与官方 src/util.ts 一致（前端按数字判断颜色）。
const (
	StatusDown        = 0
	StatusUp          = 1
	StatusPending     = 2
	StatusMaintenance = 3
)

// StatusName 返回状态的中文名，仅用于日志。
func StatusName(s int) string {
	switch s {
	case StatusUp:
		return "UP"
	case StatusDown:
		return "DOWN"
	case StatusPending:
		return "PENDING"
	case StatusMaintenance:
		return "MAINTENANCE"
	default:
		return "UNKNOWN"
	}
}

// FlipStatus 在 UP / DOWN 之间翻转，用于 upside_down 监控。
func FlipStatus(s int) int {
	switch s {
	case StatusUp:
		return StatusDown
	case StatusDown:
		return StatusUp
	default:
		return s
	}
}

// Monitor 是对 monitor 表的轻量包装。
//
// 之所以保留 Raw：monitor 表有 80+ 列且随版本增长，用强类型 struct 全量映射
// 会在官方加字段时反复返工。这里只把调度必需字段提出来，其余按需读 Raw。
type Monitor struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Active        bool           `json:"active"`
	UserID        int64          `json:"userId"`
	Type          string         `json:"type"`
	Interval      int            `json:"interval"`
	RetryInterval int            `json:"retryInterval"`
	MaxRetries    int            `json:"maxretries"`
	UpsideDown    bool           `json:"upsideDown"`
	Weight        int            `json:"weight"`
	Parent        *int64         `json:"parent"`
	PushToken     string         `json:"pushToken"`
	CreatedDate   string         `json:"createdDate"`
	Raw           map[string]any `json:"-"`
	Notifications []int64        `json:"notificationIDList"`
	Tags          []TagValue     `json:"tags"`
	GroupIDs      []int64        `json:"-"`
}

// TagValue 是 monitor_tag 的投影。
type TagValue struct {
	TagID int64  `json:"tag_id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Value string `json:"value"`
}

// Get 读取原始列，缺失返回 ""。
// 会自动尝试 snake_case / camelCase 变体，兼容官方两种列名风格。
func (m *Monitor) Get(key string) string {
	if m.Raw == nil {
		return ""
	}
	if v, ok := m.Raw[key]; ok {
		return stringifyCell(v)
	}
	for _, alt := range keyVariants(key) {
		if v, ok := m.Raw[alt]; ok {
			return stringifyCell(v)
		}
	}
	return ""
}

func stringifyCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// keyVariants 返回字段名的蛇形/驼峰变体。
func keyVariants(key string) []string {
	var out []string

	// camel → snake
	var snake strings.Builder
	for i, r := range key {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				snake.WriteByte('_')
			}
			snake.WriteRune(r - 'A' + 'a')
			continue
		}
		snake.WriteRune(r)
	}
	if s := snake.String(); s != key {
		out = append(out, s)
	}

	// snake → camel
	var camel strings.Builder
	upper := false
	for _, r := range key {
		if r == '_' {
			upper = true
			continue
		}
		if upper && r >= 'a' && r <= 'z' {
			camel.WriteRune(r - 'a' + 'A')
			upper = false
			continue
		}
		upper = false
		camel.WriteRune(r)
	}
	if c := camel.String(); c != key {
		out = append(out, c)
	}

	return out
}

// GetInt 读取整型列。
func (m *Monitor) GetInt(key string) int64 {
	s := strings.TrimSpace(m.Get(key))
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, err2 := strconv.ParseFloat(s, 64)
		if err2 != nil {
			return 0
		}
		return int64(f)
	}
	return n
}

// GetFloat 读取浮点列。
func (m *Monitor) GetFloat(key string) float64 {
	s := strings.TrimSpace(m.Get(key))
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// GetBool 读取布尔列。
func (m *Monitor) GetBool(key string) bool {
	s := strings.ToLower(strings.TrimSpace(m.Get(key)))
	return s == "1" || s == "true" || s == "yes"
}

// Has 判断列是否存在。
func (m *Monitor) Has(key string) bool {
	if m.Raw == nil {
		return false
	}
	_, ok := m.Raw[key]
	return ok
}

// ConfigValue 读取 monitor.config 里的键。
//
// 官方从 2026-10 起把 per-type 的零散字段收进了 config JSON（如
// systemServiceName），新库只有 config 没有旧列。这里先查 config，
// 再回退到旧的 snake_case 列，两种库都能读。
func (m *Monitor) ConfigValue(key string) string {
	if raw := m.Get("config"); raw != "" && raw != "{}" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			if v, ok := obj[key]; ok {
				return stringifyCell(v)
			}
		}
	}
	return m.Get(key)
}

// URL 返回监控目标地址。
func (m *Monitor) URL() string { return m.Get("url") }

// Hostname 返回主机名。
func (m *Monitor) Hostname() string { return m.Get("hostname") }

// Port 返回端口。
func (m *Monitor) Port() int { return int(m.GetInt("port")) }

// Timeout 返回监控自定义超时（秒），0 表示用默认值。
func (m *Monitor) Timeout() time.Duration {
	v := m.GetFloat("timeout")
	if v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}

// Beat 是一条心跳记录。
type Beat struct {
	ID        int64  `json:"-"`
	MonitorID int64  `json:"monitorID"`
	Status    int    `json:"status"`
	Msg       string `json:"msg"`
	Time      string `json:"time"`
	Ping      *int64 `json:"ping,omitempty"`
	Duration  int    `json:"duration"`
	Important bool   `json:"important"`
	DownCount int    `json:"downCount"`
}

// JSON 把心跳转为前端期望的结构。
func (b *Beat) JSON() map[string]any {
	out := map[string]any{
		"monitorID": b.MonitorID,
		"status":    b.Status,
		"msg":       b.Msg,
		"time":      b.Time,
		"duration":  b.Duration,
		"important": b.Important,
	}
	if b.Ping != nil {
		out["ping"] = *b.Ping
	}
	if b.DownCount > 0 {
		out["downCount"] = b.DownCount
	}
	return out
}
