package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/cron"
	"github.com/songbo/uptime-kuma/go_server/internal/logx"
	"github.com/songbo/uptime-kuma/go_server/internal/store"
)

// maintenancePayload 是前端 EditMaintenance.vue 提交的结构。
type maintenancePayload struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Strategy    string `json:"strategy"`
	Active      any    `json:"active"`

	DateRange []any `json:"dateRange"`
	TimeRange []struct {
		Hours   int `json:"hours"`
		Minutes int `json:"minutes"`
		Seconds int `json:"seconds"`
	} `json:"timeRange"`

	Weekdays        []int  `json:"weekdays"`
	DaysOfMonth     []any  `json:"daysOfMonth"`
	IntervalDay     int    `json:"intervalDay"`
	Cron            string `json:"cron"`
	DurationMinutes int    `json:"durationMinutes"`
	TimezoneOption  string `json:"timezoneOption"`
}

// toMaintenance 等价于官方 Maintenance.jsonToBean。
func (p *maintenancePayload) toMaintenance() (*store.Maintenance, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return nil, errors.New("请输入维护窗口名称")
	}

	m := &store.Maintenance{
		ID:          p.ID,
		Title:       title,
		Description: p.Description,
		Active:      truthy(p.Active),
		Strategy:    p.Strategy,
		Timezone:    p.TimezoneOption,
		IntervalDay: p.IntervalDay,
	}
	if m.Strategy == "" {
		m.Strategy = store.StrategySingle
	}

	// dateRange 的两个元素都可能是 null
	if len(p.DateRange) > 0 && p.DateRange[0] != nil {
		s, ok := p.DateRange[0].(string)
		if !ok || !validDateLiteral(s) {
			return nil, errors.New("Invalid start date")
		}
		m.StartDate = s
	}
	if len(p.DateRange) > 1 && p.DateRange[1] != nil {
		s, ok := p.DateRange[1].(string)
		if !ok || !validDateLiteral(s) {
			return nil, errors.New("Invalid end date")
		}
		m.EndDate = s
	}

	switch {
	case m.Strategy == store.StrategyCron:
		m.Duration = p.DurationMinutes * 60
		m.Cron = strings.TrimSpace(p.Cron)
		if m.Cron == "" {
			return nil, errors.New("请输入 cron 表达式")
		}
		if !cron.Valid(m.Cron) {
			return nil, fmt.Errorf("cron 表达式无效: %s", m.Cron)
		}

	case strings.HasPrefix(m.Strategy, "recurring-"):
		if len(p.TimeRange) >= 2 {
			m.StartTime = formatTimeObject(p.TimeRange[0].Hours, p.TimeRange[0].Minutes)
			m.EndTime = formatTimeObject(p.TimeRange[1].Hours, p.TimeRange[1].Minutes)
		}
		m.Weekdays = p.Weekdays
		m.DaysOfMonth = p.DaysOfMonth

		// 与官方一致：先补 cron 再校验，非法直接回错
		gen, duration, err := generateCronFor(m)
		if err != nil {
			return nil, err
		}
		m.Cron = gen
		m.Duration = duration
		if !cron.Valid(m.Cron) {
			return nil, fmt.Errorf("cron 表达式无效: %s", m.Cron)
		}
	}

	return m, nil
}

// generateCronFor 是官方 generateCron() 的移植，供保存时立刻校验。
func generateCronFor(m *store.Maintenance) (string, int, error) {
	hour, minute, ok := splitHHMM(m.StartTime)
	if !ok {
		return "", 0, fmt.Errorf("开始时间格式非法: %q", m.StartTime)
	}
	duration := calcDurationSec(m.StartTime, m.EndTime)

	switch m.Strategy {
	case store.StrategyRecurringInterval:
		return fmt.Sprintf("%d %d * * *", minute, hour), duration, nil

	case store.StrategyRecurringWeekday:
		if len(m.Weekdays) == 0 {
			return "", 0, errors.New("请至少选择一个星期")
		}
		days := append([]int(nil), m.Weekdays...)
		sortIntSlice(days)
		parts := make([]string, len(days))
		for i, d := range days {
			if d < 0 || d > 6 {
				return "", 0, fmt.Errorf("星期取值非法: %d", d)
			}
			parts[i] = strconv.Itoa(d)
		}
		return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(parts, ",")), duration, nil

	case store.StrategyRecurringDayOfMonth:
		days := make([]string, 0, len(m.DaysOfMonth))
		seen := map[string]bool{}
		for _, d := range m.DaysOfMonth {
			tok, ok := dayOfMonthToken(d)
			if !ok || seen[tok] {
				continue
			}
			seen[tok] = true
			days = append(days, tok)
		}
		if len(days) == 0 {
			return "", 0, errors.New("请至少选择一天")
		}
		return fmt.Sprintf("%d %d %s * *", minute, hour, strings.Join(days, ",")), duration, nil
	}

	return "", 0, fmt.Errorf("未知的重复策略: %s", m.Strategy)
}

// dayOfMonthToken 把前端可能混着的数字/字符串转成 cron 片段。
func dayOfMonthToken(v any) (string, bool) {
	switch t := v.(type) {
	case int:
		if t < 1 || t > 31 {
			return "", false
		}
		return strconv.Itoa(t), true
	case float64:
		n := int(t)
		if n < 1 || n > 31 {
			return "", false
		}
		return strconv.Itoa(n), true
	case string:
		if t == "lastDay1" {
			return "L", true
		}
		// lastDay2..4 cron 表达不了，官方同样忽略
		if n, err := strconv.Atoi(t); err == nil && n >= 1 && n <= 31 {
			return strconv.Itoa(n), true
		}
	}
	return "", false
}

// ---------------- socket 事件 ----------------

// addMaintenance 新建维护窗口。
func addMaintenance(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var p maintenancePayload
	if err := c.Arg(0, &p); err != nil {
		return nil, err
	}
	p.ID = 0

	m, err := p.toMaintenance()
	if err != nil {
		return nil, err
	}
	m.UserID = numericUserID(c)

	id, err := c.Maint.Save(c.Ctx, m)
	if err != nil {
		return nil, err
	}

	// 新建时若前端没单独提交绑定关系，这里保持为空
	if err := c.MaintMgr.Reload(c.Ctx, id); err != nil {
		logx.Warnf("maintenance", "重载维护窗口 %d 失败: %v", id, err)
	}
	c.MaintMgr.Broadcast()

	return map[string]any{
		"ok":            true,
		"msg":           "successAdded",
		"msgi18n":       true,
		"maintenanceID": id,
	}, nil
}

// editMaintenance 修改维护窗口。
func editMaintenance(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var p maintenancePayload
	if err := c.Arg(0, &p); err != nil {
		return nil, err
	}
	if p.ID <= 0 {
		return nil, errors.New("缺少维护窗口 ID")
	}

	existing, err := c.Maint.Get(c.Ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, errors.New("维护窗口不存在")
	}

	m, err := p.toMaintenance()
	if err != nil {
		return nil, err
	}
	m.UserID = existing.UserID
	// 间隔策略的 last_start_date 要保留，否则会立刻重复触发
	m.LastStartDate = existing.LastStartDate

	if _, err := c.Maint.Save(c.Ctx, m); err != nil {
		return nil, err
	}

	if err := c.MaintMgr.Reload(c.Ctx, m.ID); err != nil {
		logx.Warnf("maintenance", "重载维护窗口 %d 失败: %v", m.ID, err)
	}
	c.MaintMgr.Broadcast()

	return map[string]any{
		"ok":            true,
		"msg":           "Saved.",
		"msgi18n":       true,
		"maintenanceID": m.ID,
	}, nil
}

// deleteMaintenance 删除维护窗口。
func deleteMaintenance(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Maint.Delete(c.Ctx, id); err != nil {
		return nil, err
	}
	c.MaintMgr.Remove(id)
	c.MaintMgr.Broadcast()

	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// getMaintenance 取单个维护窗口。
func getMaintenance(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	data := c.MaintMgr.JSONOf(id)
	if data == nil {
		return nil, errors.New("维护窗口不存在")
	}
	return map[string]any{"ok": true, "maintenance": data}, nil
}

// getMaintenanceList 重新推送列表。
func getMaintenanceList(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}
	c.MaintMgr.Broadcast()
	return map[string]any{"ok": true}, nil
}

// addMonitorMaintenance 覆盖式设置维护窗口绑定的监控。
func addMonitorMaintenance(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	var monitors []struct {
		ID int64 `json:"id"`
	}
	_ = c.OptArg(1, &monitors)

	ids := make([]int64, 0, len(monitors))
	for _, m := range monitors {
		if m.ID > 0 {
			ids = append(ids, m.ID)
		}
	}

	if err := c.Maint.SetMonitors(c.Ctx, id, ids); err != nil {
		return nil, err
	}
	if err := c.MaintMgr.Reload(c.Ctx, id); err != nil {
		logx.Warnf("maintenance", "重载维护窗口 %d 失败: %v", id, err)
	}

	return map[string]any{"ok": true, "msg": "successAdded", "msgi18n": true}, nil
}

// addMaintenanceStatusPage 覆盖式设置维护窗口绑定的状态页。
func addMaintenanceStatusPage(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	var pages []struct {
		ID int64 `json:"id"`
	}
	_ = c.OptArg(1, &pages)

	ids := make([]int64, 0, len(pages))
	for _, p := range pages {
		if p.ID > 0 {
			ids = append(ids, p.ID)
		}
	}

	if err := c.Maint.SetStatusPages(c.Ctx, id, ids); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "msg": "successAdded", "msgi18n": true}, nil
}

// getMonitorMaintenance 返回绑定的监控 id 列表。
func getMonitorMaintenance(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	ids, err := c.Maint.MonitorIDs(c.Ctx, id)
	if err != nil {
		return nil, err
	}

	list := make([]map[string]any, 0, len(ids))
	for _, mid := range ids {
		list = append(list, map[string]any{"id": mid})
	}
	return map[string]any{"ok": true, "monitors": list}, nil
}

// getMaintenanceStatusPage 返回绑定的状态页。
func getMaintenanceStatusPage(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	pages, err := c.Maint.StatusPages(c.Ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "statusPages": pages}, nil
}

// pauseMaintenance 暂停维护窗口。
func pauseMaintenance(c *Ctx) (any, error) { return setMaintenanceActive(c, false) }

// resumeMaintenance 恢复维护窗口。
func resumeMaintenance(c *Ctx) (any, error) { return setMaintenanceActive(c, true) }

func setMaintenanceActive(c *Ctx, active bool) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	m, err := c.Maint.Get(c.Ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("Maintenance not found")
	}

	if err := c.Maint.SetActive(c.Ctx, id, active); err != nil {
		return nil, err
	}
	if err := c.MaintMgr.Reload(c.Ctx, id); err != nil {
		logx.Warnf("maintenance", "重载维护窗口 %d 失败: %v", id, err)
	}
	c.MaintMgr.Broadcast()

	msg := "successPaused"
	if active {
		msg = "successResumed"
	}
	return map[string]any{"ok": true, "msg": msg, "msgi18n": true}, nil
}

// ---------------- 小工具 ----------------

// truthy 兼容前端把 active 传成 1 / true / "1" 的情况。
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	default:
		return false
	}
}

// numericUserID 在用户 ID 是纯数字时才写 user_id。
// better-auth 的 ID 是随机字符串，写进 INTEGER 列会破坏外键。
func numericUserID(c *Ctx) int64 {
	id := c.RequireUserID()
	if id == "" {
		return 0
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// validDateLiteral 做个宽松校验：必须能被解析，且年份不超过 9999。
func validDateLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil && y > 9999 {
			return false
		}
	}
	_, ok := parseLocalLoose(s)
	return ok
}

// parseLocalLoose 只判断字符串是否像个时间，不做时区归一。
func parseLocalLoose(s string) (time.Time, bool) {
	for _, l := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05.000", "2006-01-02T15:04:05", "2006-01-02T15:04",
		"2006-01-02 15:04:05.000", "2006-01-02 15:04:05", "2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func formatTimeObject(hours, minutes int) string {
	return fmt.Sprintf("%02d:%02d", hours, minutes)
}

func splitHHMM(s string) (hour, minute int, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func calcDurationSec(startTime, endTime string) int {
	sh, sm, ok1 := splitHHMM(startTime)
	eh, em, ok2 := splitHHMM(endTime)
	if !ok1 || !ok2 {
		return 0
	}
	d := (eh*3600 + em*60) - (sh*3600 + sm*60)
	if d < 0 {
		d += 24 * 3600
	}
	return d
}

func sortIntSlice(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
