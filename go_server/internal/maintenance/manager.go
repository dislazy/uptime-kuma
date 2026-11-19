// Package maintenance 实现维护窗口的运行时。
//
// 与官方一致地支持六种策略：
//
//	manual                    手动，active 即一直处于维护中
//	single                    单个时间区间 [start_date, end_date]
//	cron                      用户直接填 cron 表达式 + duration
//	recurring-interval        每 N 天，间隔不足时跳过本次触发
//	recurring-weekday         每周指定的几天
//	recurring-day-of-month    每月指定的几天（支持 "lastDay1" → 月末）
//
// 状态判定完全按官方 getStatus() 的顺序，因为维护中的监控会在
// 检查前被改写为 MAINTENANCE(3)，顺序错了会导致心跳状态抖动。
package maintenance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/cron"
	"github.com/songbo/uptime-kuma/go_server/internal/logx"
	"github.com/songbo/uptime-kuma/go_server/internal/store"
)

// Broadcaster 由 hub.Hub 实现，用于状态变化时推送列表。
type Broadcaster interface {
	Broadcast(event string, data ...any)
}

// Manager 持有全部维护窗口的运行时状态。
type Manager struct {
	ms *store.MaintenanceStore
	h  Broadcaster
	tz *time.Location

	mu    sync.RWMutex
	items map[int64]*item
	links map[int64][]int64 // monitorID -> []maintenanceID

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type item struct {
	m      *store.Maintenance
	sched  *cron.Schedule
	status string
}

// New 构造管理器。serverTZ 为 setting 里的 serverTimezone。
func New(ms *store.MaintenanceStore, h Broadcaster, serverTZ string) *Manager {
	loc := time.UTC
	if serverTZ != "" {
		if l, err := time.LoadLocation(serverTZ); err == nil {
			loc = l
		}
	}
	return &Manager{
		ms:    ms,
		h:     h,
		tz:    loc,
		items: map[int64]*item{},
		links: map[int64][]int64{},
		stop:  make(chan struct{}),
	}
}

// Start 载入全部维护窗口并启动状态监视。
func (mgr *Manager) Start(ctx context.Context) error {
	if err := mgr.Load(ctx); err != nil {
		return err
	}

	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		mgr.loop()
	}()

	logx.Infof("maintenance", "维护窗口已加载，共 %d 个", mgr.Count())
	return nil
}

// Stop 停止监视。
func (mgr *Manager) Stop() {
	mgr.stopOnce.Do(func() { close(mgr.stop) })
	mgr.wg.Wait()
}

// Load 全量重载。
func (mgr *Manager) Load(ctx context.Context) error {
	list, err := mgr.ms.List(ctx)
	if err != nil {
		return fmt.Errorf("读取维护窗口失败: %w", err)
	}
	links, err := mgr.ms.Links(ctx)
	if err != nil {
		return fmt.Errorf("读取监控绑定失败: %w", err)
	}

	items := make(map[int64]*item, len(list))
	now := time.Now()

	for _, m := range list {
		mgr.ensureCron(ctx, m)
		it := &item{m: m}
		if m.Cron != "" {
			s, err := cron.Parse(m.Cron)
			if err != nil {
				// cron 写错了不能让整个列表加载失败，退化成「永不触发」
				logx.Warnf("maintenance", "维护窗口 %d 的 cron 无效(%q): %v", m.ID, m.Cron, err)
			} else {
				it.sched = s
			}
		}
		it.status = mgr.computeStatus(it, now)
		items[m.ID] = it
	}

	mgr.mu.Lock()
	mgr.items = items
	mgr.links = links
	mgr.mu.Unlock()
	return nil
}

// Reload 重载单个维护窗口（add/edit 之后调用）。
func (mgr *Manager) Reload(ctx context.Context, id int64) error {
	m, err := mgr.ms.Get(ctx, id)
	if err != nil {
		return err
	}
	if m == nil {
		mgr.Remove(id)
		return nil
	}

	mgr.ensureCron(ctx, m)

	it := &item{m: m}
	if m.Cron != "" {
		if s, err := cron.Parse(m.Cron); err == nil {
			it.sched = s
		} else {
			logx.Warnf("maintenance", "维护窗口 %d 的 cron 无效(%q): %v", m.ID, m.Cron, err)
		}
	}
	it.status = mgr.computeStatus(it, time.Now())

	mgr.mu.Lock()
	mgr.items[m.ID] = it
	mgr.mu.Unlock()

	// 绑定关系可能变了，重新拉一次
	if links, err := mgr.ms.Links(ctx); err == nil {
		mgr.mu.Lock()
		mgr.links = links
		mgr.mu.Unlock()
	}
	return nil
}

// Remove 移除运行时状态。
func (mgr *Manager) Remove(id int64) {
	mgr.mu.Lock()
	delete(mgr.items, id)
	mgr.mu.Unlock()
}

// Count 返回维护窗口数量。
func (mgr *Manager) Count() int {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return len(mgr.items)
}

// Status 返回某个维护窗口的当前状态。
func (mgr *Manager) Status(id int64) string {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if it, ok := mgr.items[id]; ok {
		return it.status
	}
	return store.MaintUnknown
}

// IsUnderMaintenance 判断某个监控是否处于维护中（不处理父分组）。
func (mgr *Manager) IsUnderMaintenance(monitorID int64) bool {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	for _, maid := range mgr.links[monitorID] {
		it, ok := mgr.items[maid]
		if !ok {
			continue
		}
		if it.status == store.MaintUnderMaintenance {
			return true
		}
	}
	return false
}

// ActiveMaintenanceIDs 返回当前处于维护中的维护窗口 id 集合，
// 供状态页等只读场景一次取全。
func (mgr *Manager) ActiveMaintenanceIDs() map[int64]bool {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	out := make(map[int64]bool, len(mgr.items))
	for id, it := range mgr.items {
		if it.status == store.MaintUnderMaintenance {
			out[id] = true
		}
	}
	return out
}

// JSONList 生成前端期望的 maintenanceList（key 为字符串 id）。
func (mgr *Manager) JSONList() map[string]any {
	now := time.Now()

	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	out := make(map[string]any, len(mgr.items))
	for id, it := range mgr.items {
		out[strconv.FormatInt(id, 10)] = mgr.jsonOf(it, now)
	}
	return out
}

// JSONOf 生成单个维护窗口的 JSON。
func (mgr *Manager) JSONOf(id int64) map[string]any {
	now := time.Now()
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	it, ok := mgr.items[id]
	if !ok {
		return nil
	}
	return mgr.jsonOf(it, now)
}

// Broadcast 主动推一次列表（在增删改后调用）。
func (mgr *Manager) Broadcast() {
	if mgr.h == nil {
		return
	}
	mgr.h.Broadcast("maintenanceList", mgr.JSONList())
}

// loop 每秒比对一次状态，变化时广播。
func (mgr *Manager) loop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-mgr.stop:
			return
		case now := <-ticker.C:
			changed := false

			mgr.mu.Lock()
			for _, it := range mgr.items {
				st := mgr.computeStatus(it, now)
				if st != it.status {
					it.status = st
					changed = true
					logx.Debugf("maintenance", "维护窗口 %d 状态变为 %s", it.m.ID, st)
				}
			}
			mgr.mu.Unlock()

			if changed && mgr.h != nil {
				mgr.h.Broadcast("maintenanceList", mgr.JSONList())
			}
		}
	}
}

// —— 状态计算 ——

// computeStatus 移植自官方 getStatus()，顺序不可调换。
func (mgr *Manager) computeStatus(it *item, now time.Time) string {
	m := it.m
	if !m.Active {
		return store.MaintInactive
	}

	loc := mgr.tzOf(m)

	if m.Strategy == store.StrategyManual {
		return store.MaintUnderMaintenance
	}

	if m.StartDate != "" {
		if start, ok := parseLocal(m.StartDate, loc); ok && now.Before(start) {
			return store.MaintScheduled
		}
	}
	if m.EndDate != "" {
		if end, ok := parseLocal(m.EndDate, loc); ok && now.After(end) {
			return store.MaintEnded
		}
	}

	if m.Strategy == store.StrategySingle {
		return store.MaintUnderMaintenance
	}

	if _, _, running := mgr.runningTimeslot(it, now); running {
		return store.MaintUnderMaintenance
	}
	return store.MaintScheduled
}

// runningTimeslot 推算「当前是否落在某个窗口内」以及该窗口的起止。
// 算法与官方 getRunningTimeslot() 一致：从 now-duration 开始找下一个触发点，
// 若该触发点 + duration 还没过去，说明现在就在窗口里。
func (mgr *Manager) runningTimeslot(it *item, now time.Time) (time.Time, time.Time, bool) {
	m := it.m
	if it.sched == nil || m.Duration <= 0 {
		return time.Time{}, time.Time{}, false
	}

	dur := time.Duration(m.Duration) * time.Second
	from := now.Add(-dur)

	start := mgr.nextTrigger(it, from)
	if start.IsZero() {
		return time.Time{}, time.Time{}, false
	}

	end := start.Add(dur)
	if now.After(start) && now.Before(end) {
		return start, end, true
	}
	return time.Time{}, time.Time{}, false
}

// nextTrigger 取 from 之后的下一个有效触发点。
// recurring-interval 需要跳过间隔不足的触发，所以要有个有限次的重试。
func (mgr *Manager) nextTrigger(it *item, from time.Time) time.Time {
	loc := mgr.tzOf(it.m)
	if from.Location() != loc {
		from = from.In(loc)
	}

	cur := from
	// 间隔策略最多尝试 400 次（日触发约一年多），够覆盖任何合理间隔。
	for i := 0; i < 400; i++ {
		nxt := it.sched.Next(cur)
		if nxt.IsZero() {
			return time.Time{}
		}

		if it.m.Strategy == store.StrategyRecurringInterval &&
			it.m.IntervalDay > 1 && it.m.LastStartDate != "" {
			if last, err := store.ParseTime(it.m.LastStartDate); err == nil {
				// 官方减 1.1 小时，抵消时区带来的偏差
				last = last.Add(-66 * time.Minute)
				days := int(cur.Sub(last).Hours() / 24)
				if days < it.m.IntervalDay {
					cur = nxt
					continue
				}
			}
		}
		return nxt
	}
	return time.Time{}
}

// —— cron 生成 ——

// ensureCron 为 recurring 策略补出 cron 与 duration，并把变更写回库。
// 对应官方 run() 里的 1.21.2 迁移逻辑。
func (mgr *Manager) ensureCron(ctx context.Context, m *store.Maintenance) {
	beforeCron, beforeDur := m.Cron, m.Duration

	switch {
	case m.Strategy == store.StrategyCron:
		// 用户自填，不动

	case !strings.HasPrefix(m.Strategy, "recurring-"):
		m.Cron = ""
		m.Duration = 0

	case m.Strategy == store.StrategyRecurringInterval,
		m.Strategy == store.StrategyRecurringWeekday,
		m.Strategy == store.StrategyRecurringDayOfMonth:
		hour, minute, ok := splitTime(m.StartTime)
		if !ok {
			logx.Warnf("maintenance", "维护窗口 %d 的 start_time 非法: %q", m.ID, m.StartTime)
			return
		}

		switch m.Strategy {
		case store.StrategyRecurringInterval:
			m.Cron = fmt.Sprintf("%d %d * * *", minute, hour)
		case store.StrategyRecurringWeekday:
			days := append([]int(nil), m.Weekdays...)
			if len(days) == 0 {
				logx.Warnf("maintenance", "维护窗口 %d 未选星期，跳过 cron 生成", m.ID)
				return
			}
			sortInts(days)
			parts := make([]string, len(days))
			for i, d := range days {
				parts[i] = strconv.Itoa(d)
			}
			m.Cron = fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(parts, ","))
		case store.StrategyRecurringDayOfMonth:
			days := make([]string, 0, len(m.DaysOfMonth))
			seen := map[string]bool{}
			for _, d := range m.DaysOfMonth {
				var tok string
				switch v := d.(type) {
				case int:
					tok = strconv.Itoa(v)
				case float64:
					tok = strconv.Itoa(int(v))
				case string:
					// cron 只认 "L"，lastDay2..4 无法表达，忽略
					if v == "lastDay1" {
						tok = "L"
					} else {
						if n, err := strconv.Atoi(v); err == nil {
							tok = strconv.Itoa(n)
						} else {
							continue
						}
					}
				default:
					continue
				}
				if tok == "" || seen[tok] {
					continue
				}
				seen[tok] = true
				days = append(days, tok)
			}
			if len(days) == 0 {
				logx.Warnf("maintenance", "维护窗口 %d 未选日期，跳过 cron 生成", m.ID)
				return
			}
			m.Cron = fmt.Sprintf("%d %d %s * *", minute, hour, strings.Join(days, ","))
		}

		m.Duration = calcDuration(m.StartTime, m.EndTime)
	}

	if m.Cron != beforeCron || m.Duration != beforeDur {
		if _, err := mgr.ms.Save(ctx, m); err != nil {
			logx.Warnf("maintenance", "回写 cron 失败(id=%d): %v", m.ID, err)
		} else {
			logx.Debugf("maintenance", "维护窗口 %d 生成 cron: %q, duration: %ds", m.ID, m.Cron, m.Duration)
		}
	}
}

// calcDuration 算 start_time → end_time 的秒数，跨天补 24 小时。
func calcDuration(startTime, endTime string) int {
	sh, sm, ok1 := splitTime(startTime)
	eh, em, ok2 := splitTime(endTime)
	if !ok1 || !ok2 {
		return 0
	}
	d := (eh*3600 + em*60) - (sh*3600 + sm*60)
	if d < 0 {
		d += 24 * 3600
	}
	return d
}

func splitTime(s string) (hour, minute int, ok bool) {
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

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// —— 时区与时间解析 ——

func (mgr *Manager) tzOf(m *store.Maintenance) *time.Location {
	if m.Timezone == "" || m.Timezone == "SAME_AS_SERVER" {
		return mgr.tz
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		return mgr.tz
	}
	return loc
}

// parseLocal 把「维护时区下的本地时间字面量」解析成绝对时间。
// 前端传的是 "2026-11-19T22:00"（无时区），官方用 dayjs.tz(v, tz) 处理。
func parseLocal(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}

	// 带显式时区的就按其时区来
	for _, l := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			return t.In(loc), true
		}
	}

	layouts := []string{
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// —— JSON 输出 ——

func (mgr *Manager) jsonOf(it *item, now time.Time) map[string]any {
	m := it.m
	loc := mgr.tzOf(m)

	// dateRange：官方仅在 start 存在时给值，但始终占位第一个元素
	dateRange := make([]any, 0, 2)
	if m.StartDate != "" {
		dateRange = append(dateRange, m.StartDate)
	} else {
		dateRange = append(dateRange, nil)
	}
	if m.EndDate != "" {
		dateRange = append(dateRange, m.EndDate)
	}

	weekdays := m.Weekdays
	if weekdays == nil {
		weekdays = []int{}
	}
	daysOfMonth := m.DaysOfMonth
	if daysOfMonth == nil {
		daysOfMonth = []any{}
	}

	out := map[string]any{
		"id":              m.ID,
		"title":           m.Title,
		"description":     m.Description,
		"strategy":        m.Strategy,
		"intervalDay":     m.IntervalDay,
		"active":          m.Active,
		"dateRange":       dateRange,
		"timeRange":       []any{timeObject(m.StartTime), timeObject(m.EndTime)},
		"weekdays":        weekdays,
		"daysOfMonth":     daysOfMonth,
		"timeslotList":    mgr.timeslots(it, now, loc),
		"cron":            m.Cron,
		"duration":        m.Duration,
		"durationMinutes": m.Duration / 60,
		"timezone":        loc.String(),
		"timezoneOption":  timezoneOption(m.Timezone),
		"timezoneOffset":  zoneOffset(loc),
		"status":          it.status,
	}

	if m.LastStartDate != "" {
		out["lastStartDate"] = m.LastStartDate
	}
	return out
}

// timeslots 按官方 toPublicJSON 的规则生成 timeslotList。
func (mgr *Manager) timeslots(it *item, now time.Time, loc *time.Location) []any {
	m := it.m
	out := make([]any, 0, 2)

	switch m.Strategy {
	case store.StrategyManual:
		return out

	case store.StrategySingle:
		out = append(out, map[string]any{
			"startDate": nullable(m.StartDate),
			"endDate":   nullable(m.EndDate),
		})
		return out
	}

	// cron / recurring：正在跑的窗口 + 下一个窗口
	if start, end, running := mgr.runningTimeslot(it, now); running {
		out = append(out, map[string]any{
			"startDate": start.UTC().Format(time.RFC3339Nano),
			"endDate":   end.UTC().Format(time.RFC3339Nano),
		})
	}

	if it.sched != nil && m.Duration > 0 {
		if nxt := mgr.nextTrigger(it, now.In(loc)); !nxt.IsZero() {
			out = append(out, map[string]any{
				"startDate": nxt.UTC().Format(time.RFC3339Nano),
				"endDate":   nxt.Add(time.Duration(m.Duration) * time.Second).UTC().Format(time.RFC3339Nano),
			})
		}
	}
	return out
}

func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// timeObject 让 "HH:mm" 变成前端 vue-datepicker 需要的 {hours, minutes, seconds}。
func timeObject(s string) map[string]any {
	h, m, ok := splitTime(s)
	if !ok {
		return map[string]any{"hours": 0, "minutes": 0}
	}
	return map[string]any{"hours": h, "minutes": m, "seconds": 0}
}

func timezoneOption(tz string) string {
	if tz == "" {
		return "SAME_AS_SERVER"
	}
	return tz
}

// zoneOffset 生成 "+08:00" 这种格式，对应 dayjs 的 format("Z")。
func zoneOffset(loc *time.Location) string {
	_, offset := time.Now().In(loc).Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
}
