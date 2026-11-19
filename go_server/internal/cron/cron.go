// Package cron 实现 Uptime Kuma 维护窗口所需的最小 cron 求值器。
//
// 官方用的是 croner，语法为 5 字段（分 时 日 月 周），额外支持
// day-of-month 上的 "L"（当月最后一天）。本包只求值、不调度，
// 调用方（维护窗口、聚合任务）自己决定什么时候问 "下一次是何时"。
//
// 支持：
//
//   - 任意值
//     ?        等价于 *
//     5        单值
//     1-10     范围
//     */5      步长
//     1-10/2   范围 + 步长
//     5/10     起点 + 步长（等同 5-59/10）
//     1,3,5    列表
//     L        当月最后一天（仅 day-of-month）
//     MON..SUN 星期名（仅 day-of-week）
//     JAN..DEC 月份名（仅 month）
//     @yearly @annually @monthly @weekly @daily @midnight @hourly
//
// day-of-month 与 day-of-week 同时被限制时，按标准 cron 语义取「或」：
// 只要命中其中之一就触发。
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 是解析后的表达式。零值不可用，必须经 Parse 构造。
type Schedule struct {
	spec string

	minutes uint64 // 0-59
	hours   uint64 // 0-23
	dom     uint64 // 1-31
	months  uint64 // 1-12
	dow     uint64 // 0-6，0 = 周日

	domStar bool // day-of-month 是 *（决定与 dow 的「或」语义）
	dowStar bool
	lastDay bool // day-of-month 含 L
}

// macros 是 @ 开头的便捷写法。
var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// Parse 解析 5 字段表达式。timezone 前缀（CRON_TZ=...）会被忽略，
// 时区由调用方通过 time.Time 的 Location 决定。
func Parse(spec string) (*Schedule, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return nil, fmt.Errorf("cron 表达式为空")
	}

	// 容忍 "CRON_TZ=Asia/Shanghai 0 3 * * *" 这种写法，去掉前缀。
	if strings.HasPrefix(raw, "CRON_TZ=") || strings.HasPrefix(raw, "TZ=") {
		if i := strings.IndexByte(raw, ' '); i > 0 {
			raw = strings.TrimSpace(raw[i+1:])
		}
	}

	if strings.HasPrefix(raw, "@") {
		low := strings.ToLower(raw)
		expanded, ok := macros[low]
		if !ok {
			return nil, fmt.Errorf("不支持的 cron 宏: %s", raw)
		}
		raw = expanded
	}

	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 需要 5 个字段，实际 %d 个: %s", len(fields), raw)
	}

	s := &Schedule{spec: raw}

	var err error
	if s.minutes, _, err = parseField(fields[0], 0, 59, nil, false); err != nil {
		return nil, fmt.Errorf("分钟字段: %w", err)
	}
	if s.hours, _, err = parseField(fields[1], 0, 23, nil, false); err != nil {
		return nil, fmt.Errorf("小时字段: %w", err)
	}

	var lastDay bool
	if s.dom, lastDay, err = parseField(fields[2], 1, 31, nil, true); err != nil {
		return nil, fmt.Errorf("日字段: %w", err)
	}
	s.lastDay = lastDay
	s.domStar = isStar(fields[2])

	if s.months, _, err = parseField(fields[3], 1, 12, monthNames, false); err != nil {
		return nil, fmt.Errorf("月字段: %w", err)
	}

	// day-of-week 允许 7 表示周日，统一折叠到 0。
	dow, _, err := parseField(fields[4], 0, 7, dowNames, false)
	if err != nil {
		return nil, fmt.Errorf("周字段: %w", err)
	}
	if dow&(1<<7) != 0 {
		dow |= 1 << 0
		dow &^= 1 << 7
	}
	s.dow = dow
	s.dowStar = isStar(fields[4])

	if s.minutes == 0 || s.hours == 0 || s.months == 0 {
		return nil, fmt.Errorf("cron 表达式无有效取值: %s", raw)
	}
	if s.dom == 0 && !s.lastDay {
		return nil, fmt.Errorf("日字段无有效取值: %s", raw)
	}
	if s.dow == 0 {
		return nil, fmt.Errorf("周字段无有效取值: %s", raw)
	}

	return s, nil
}

// Valid 判断表达式是否合法。
func Valid(spec string) bool {
	_, err := Parse(spec)
	return err == nil
}

// String 返回解析时的原始表达式。
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.spec
}

func isStar(field string) bool {
	f := strings.ToLower(strings.TrimSpace(field))
	return f == "*" || f == "?"
}

// parseField 把单个字段解析成位图。lastDayOK 为真时识别 "L"。
func parseField(field string, min, max int, names map[string]int, lastDayOK bool) (uint64, bool, error) {
	var bits uint64
	var lastDay bool

	field = strings.TrimSpace(field)
	if field == "" {
		return 0, false, fmt.Errorf("空字段")
	}

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// 步长
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err != nil || n <= 0 {
				return 0, false, fmt.Errorf("非法步长: %s", part)
			}
			step = n
			part = strings.TrimSpace(part[:i])
		}

		var lo, hi int
		switch {
		case part == "*" || part == "?":
			lo, hi = min, max

		case lastDayOK && strings.EqualFold(part, "L"):
			lastDay = true
			continue

		case strings.Contains(part, "-"):
			// 范围；两端都允许写名字
			i := strings.IndexByte(part, '-')
			a, err := fieldValue(part[:i], names)
			if err != nil {
				return 0, false, err
			}
			b, err := fieldValue(part[i+1:], names)
			if err != nil {
				return 0, false, err
			}
			// 允许 22-2 这种跨月回绕写法
			if a <= b {
				lo, hi = a, b
			} else {
				lo, hi = a, max
				setRange(&bits, min, b)
			}

		default:
			v, err := fieldValue(part, names)
			if err != nil {
				return 0, false, err
			}
			lo = v
			hi = max
			// 没写步长时 "5" 就只是 5 本身
			if step == 1 {
				hi = v
			}
		}

		if lo < min {
			lo = min
		}
		if hi > max {
			hi = max
		}
		if lo > hi {
			return 0, false, fmt.Errorf("范围非法: %s", part)
		}

		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}

	return bits, lastDay, nil
}

func fieldValue(tok string, names map[string]int) (int, error) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return 0, fmt.Errorf("空取值")
	}
	if names != nil {
		if v, ok := names[strings.ToUpper(tok)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("非法取值: %s", tok)
	}
	return v, nil
}

func setRange(bits *uint64, lo, hi int) {
	if lo < 0 {
		lo = 0
	}
	for v := lo; v <= hi; v++ {
		*bits |= 1 << uint(v)
	}
}

func has(bits uint64, v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return bits&(1<<uint(v)) != 0
}

// dayMatch 实现 dom 与 dow 的「或」语义。
func (s *Schedule) dayMatch(t time.Time) bool {
	domOK := has(s.dom, t.Day())
	if s.lastDay {
		// 当月最后一天 = 明天进入下个月
		if t.AddDate(0, 0, 1).Month() != t.Month() {
			domOK = true
		}
	}
	dowOK := has(s.dow, int(t.Weekday()))

	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next 返回严格晚于 after 的下一个触发时刻（精确到分钟）。
// 找不到（比如 2 月 30 日这种永远不会成立的表达式）时返回零值。
func (s *Schedule) Next(after time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}

	loc := after.Location()
	// 从下一分钟开始，避免 after 本身带秒时重复命中当前分钟。
	t := time.Date(after.Year(), after.Month(), after.Day(),
		after.Hour(), after.Minute(), 0, 0, loc).Add(time.Minute)

	limit := t.AddDate(5, 0, 1)

	for t.Before(limit) {
		if !has(s.months, int(t.Month())) {
			// 跳到下个月 1 号 0 点
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatch(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !has(s.hours, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !has(s.minutes, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
