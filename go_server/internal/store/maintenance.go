package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 维护窗口的六种策略，与官方一致。
const (
	StrategyManual              = "manual"
	StrategySingle              = "single"
	StrategyCron                = "cron"
	StrategyRecurringInterval   = "recurring-interval"
	StrategyRecurringWeekday    = "recurring-weekday"
	StrategyRecurringDayOfMonth = "recurring-day-of-month"
)

// 维护窗口状态，取值与官方 getStatus() 返回一致。
const (
	MaintInactive         = "inactive"
	MaintScheduled        = "scheduled"
	MaintUnderMaintenance = "under-maintenance"
	MaintEnded            = "ended"
	MaintUnknown          = "unknown"
)

// Maintenance 是 maintenance 表的一行。
type Maintenance struct {
	ID          int64
	Title       string
	Description string
	UserID      int64
	Active      bool
	Strategy    string

	// StartDate / EndDate 是「维护时区下的本地时间字面量」，
	// 前端传什么就存什么（官方同样不做归一化），解析时按 timezone 解释。
	StartDate string
	EndDate   string

	StartTime   string // "HH:mm" 或 "HH:mm:ss"
	EndTime     string
	Weekdays    []int
	DaysOfMonth []any // 元素可能是数字，也可能是 "lastDay1"

	IntervalDay   int
	Cron          string
	Timezone      string
	Duration      int    // 秒
	LastStartDate string // UTC
}

// MaintenanceStore 负责 maintenance 及其关联表。
type MaintenanceStore struct {
	db *DB
}

// NewMaintenanceStore 构造。
func NewMaintenanceStore(db *DB) *MaintenanceStore { return &MaintenanceStore{db: db} }

// toMaintenance 把通用行转成结构体。
func toMaintenance(r Row) *Maintenance {
	m := &Maintenance{
		ID:            r.Int("id"),
		Title:         r.Str("title"),
		Description:   r.Str("description"),
		UserID:        pickInt(r, "user_id", "userId"),
		Active:        pickBool(r, "active"),
		Strategy:      r.Str("strategy"),
		StartDate:     pickStr(r, "start_date", "startDate"),
		EndDate:       pickStr(r, "end_date", "endDate"),
		StartTime:     pickStr(r, "start_time", "startTime"),
		EndTime:       pickStr(r, "end_time", "endTime"),
		IntervalDay:   int(pickInt(r, "interval_day", "intervalDay")),
		Cron:          r.Str("cron"),
		Timezone:      r.Str("timezone"),
		Duration:      int(pickInt(r, "duration")),
		LastStartDate: pickStr(r, "last_start_date", "lastStartDate"),
	}
	m.Weekdays = parseIntList(pickStr(r, "weekdays"))
	m.DaysOfMonth = parseAnyList(pickStr(r, "days_of_month", "daysOfMonth"))
	return m
}

func parseIntList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var nums []int
	if err := json.Unmarshal([]byte(s), &nums); err == nil && nums != nil {
		return nums
	}
	// 混入非数字（如 "lastDay1"）时退化成逐个解析
	var raws []any
	if err := json.Unmarshal([]byte(s), &raws); err != nil {
		return nil
	}
	for _, v := range raws {
		if f, ok := v.(float64); ok {
			nums = append(nums, int(f))
		}
	}
	return nums
}

func parseAnyList(s string) []any {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var out []any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	// 把数字统一成 int，避免前端拿到 1.0 这种浮点
	for i, v := range out {
		if f, ok := v.(float64); ok && f == float64(int(f)) {
			out[i] = int(f)
		}
	}
	return out
}

// List 返回全部维护窗口。
func (s *MaintenanceStore) List(ctx context.Context) ([]*Maintenance, error) {
	rows, err := s.db.Query(ctx, "SELECT * FROM maintenance ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	out := make([]*Maintenance, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMaintenance(r))
	}
	return out, nil
}

// Get 按 id 取一个维护窗口。
func (s *MaintenanceStore) Get(ctx context.Context, id int64) (*Maintenance, error) {
	row, err := s.db.FindByID(ctx, "maintenance", id)
	if err != nil || row == nil {
		return nil, err
	}
	return toMaintenance(row), nil
}

// values 把结构体摊平成列值。
func (m *Maintenance) values() map[string]any {
	weekdays := "[]"
	if m.Weekdays != nil {
		if b, err := json.Marshal(m.Weekdays); err == nil {
			weekdays = string(b)
		}
	}

	days := "[]"
	if m.DaysOfMonth != nil {
		if b, err := json.Marshal(m.DaysOfMonth); err == nil {
			days = string(b)
		}
	}

	nullIfEmpty := func(s string) any {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return s
	}

	// better-auth 的用户 ID 是随机字符串，无法写进 INTEGER 外键列，
	// 这种情况下留 NULL，而不是写 0 触发 FK 约束失败。
	var userID any
	if m.UserID > 0 {
		userID = m.UserID
	}

	return map[string]any{
		"title":           m.Title,
		"description":     m.Description,
		"user_id":         userID,
		"active":          m.Active,
		"strategy":        m.Strategy,
		"start_date":      nullIfEmpty(m.StartDate),
		"end_date":        nullIfEmpty(m.EndDate),
		"start_time":      nullIfEmpty(m.StartTime),
		"end_time":        nullIfEmpty(m.EndTime),
		"weekdays":        weekdays,
		"days_of_month":   days,
		"interval_day":    m.IntervalDay,
		"cron":            m.Cron,
		"timezone":        nullIfEmpty(m.Timezone),
		"duration":        m.Duration,
		"last_start_date": nullIfEmpty(m.LastStartDate),
	}
}

// Save 插入或更新，返回主键。
func (s *MaintenanceStore) Save(ctx context.Context, m *Maintenance) (int64, error) {
	if m.ID > 0 {
		if err := s.db.Update(ctx, "maintenance", m.ID, m.values()); err != nil {
			return 0, err
		}
		return m.ID, nil
	}

	id, err := s.db.Insert(ctx, "maintenance", m.values())
	if err != nil {
		return 0, err
	}
	m.ID = id
	return id, nil
}

// SetActive 只切 active 开关。
func (s *MaintenanceStore) SetActive(ctx context.Context, id int64, active bool) error {
	return s.db.Update(ctx, "maintenance", id, map[string]any{"active": active})
}

// MarkLastStart 记录本次窗口的启动时刻（UTC）。
func (s *MaintenanceStore) MarkLastStart(ctx context.Context, id int64, t time.Time) error {
	return s.db.Update(ctx, "maintenance", id, map[string]any{"last_start_date": FormatTime(t)})
}

// Delete 删除维护窗口及其关联。
func (s *MaintenanceStore) Delete(ctx context.Context, id int64) error {
	for _, t := range []string{"monitor_maintenance", "maintenance_status_page", "maintenance_timeslot"} {
		if _, err := s.db.Exec(ctx, "DELETE FROM "+t+" WHERE maintenance_id = ?", id); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", t, err)
		}
	}
	return s.db.Delete(ctx, "maintenance", id)
}

// MonitorIDs 返回该维护窗口绑定的监控 id。
func (s *MaintenanceStore) MonitorIDs(ctx context.Context, maintenanceID int64) ([]int64, error) {
	rows, err := s.db.Query(ctx,
		`SELECT mm.monitor_id AS mid FROM monitor_maintenance mm
		 JOIN monitor m ON mm.monitor_id = m.id
		 WHERE mm.maintenance_id = ? ORDER BY mm.monitor_id`, maintenanceID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Int("mid"))
	}
	return out, nil
}

// SetMonitors 覆盖式设置监控绑定。
func (s *MaintenanceStore) SetMonitors(ctx context.Context, maintenanceID int64, monitorIDs []int64) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM monitor_maintenance WHERE maintenance_id = ?", maintenanceID); err != nil {
		return err
	}
	for _, mid := range monitorIDs {
		if _, err := s.db.Insert(ctx, "monitor_maintenance", map[string]any{
			"monitor_id":     mid,
			"maintenance_id": maintenanceID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// StatusPageRef 是维护窗口绑定的状态页。
type StatusPageRef struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// StatusPages 返回绑定的状态页。
func (s *MaintenanceStore) StatusPages(ctx context.Context, maintenanceID int64) ([]StatusPageRef, error) {
	rows, err := s.db.Query(ctx,
		`SELECT sp.id AS id, sp.title AS title FROM maintenance_status_page msp
		 JOIN status_page sp ON msp.status_page_id = sp.id
		 WHERE msp.maintenance_id = ? ORDER BY sp.id`, maintenanceID)
	if err != nil {
		return nil, err
	}
	out := make([]StatusPageRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, StatusPageRef{ID: r.Int("id"), Title: r.Str("title")})
	}
	return out, nil
}

// SetStatusPages 覆盖式设置状态页绑定。
func (s *MaintenanceStore) SetStatusPages(ctx context.Context, maintenanceID int64, ids []int64) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM maintenance_status_page WHERE maintenance_id = ?", maintenanceID); err != nil {
		return err
	}
	for _, sid := range ids {
		if _, err := s.db.Insert(ctx, "maintenance_status_page", map[string]any{
			"status_page_id": sid,
			"maintenance_id": maintenanceID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Links 返回 monitorID -> maintenanceID 列表的全量映射，
// 运行时用来快速判断某个监控是否在维护中。
func (s *MaintenanceStore) Links(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.Query(ctx,
		"SELECT monitor_id AS mid, maintenance_id AS maid FROM monitor_maintenance")
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]int64)
	for _, r := range rows {
		mid := r.Int("mid")
		out[mid] = append(out[mid], r.Int("maid"))
	}
	return out, nil
}

// MaintenanceIDsForStatusPage 返回绑定到某个状态页的维护窗口 id。
func (s *MaintenanceStore) MaintenanceIDsForStatusPage(ctx context.Context, statusPageID int64) ([]int64, error) {
	rows, err := s.db.Query(ctx,
		"SELECT maintenance_id AS mid FROM maintenance_status_page WHERE status_page_id = ?", statusPageID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Int("mid"))
	}
	return out, nil
}
