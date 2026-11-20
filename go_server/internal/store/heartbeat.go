package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// HeartbeatStore 负责心跳写入与查询。
//
// 写入走批量通道：监控越多，逐个 INSERT 越容易把 SQLite 写锁打满。
// 这里把心跳先丢进带缓冲的 channel，由单个 goroutine 每 500ms 或每 100 条
// 提交一次事务，写入次数下降一个数量级。
type HeartbeatStore struct {
	db *DB

	ch       chan model.Beat
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// 批量写入参数。
const (
	beatBufferSize   = 2048
	beatFlushEvery   = 500 * time.Millisecond
	beatFlushMaxRows = 100
)

// NewHeartbeatStore 构造。
func NewHeartbeatStore(db *DB) *HeartbeatStore {
	return &HeartbeatStore{
		db:   db,
		ch:   make(chan model.Beat, beatBufferSize),
		stop: make(chan struct{}),
	}
}

// Start 启动后台批量写入。
func (h *HeartbeatStore) Start(ctx context.Context) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.flushLoop(ctx)
	}()
}

// Stop 停止并落盘剩余数据。
func (h *HeartbeatStore) Stop() {
	h.stopOnce.Do(func() { close(h.stop) })
	h.wg.Wait()
}

// Enqueue 异步写入一条心跳（非阻塞，缓冲满则丢弃并告警）。
func (h *HeartbeatStore) Enqueue(b model.Beat) {
	select {
	case h.ch <- b:
	default:
		logx.Warnf("heartbeat", "心跳缓冲已满，丢弃 monitor #%d 的一条记录", b.MonitorID)
	}
}

// Save 同步写入一条心跳，返回自增 ID。
func (h *HeartbeatStore) Save(ctx context.Context, b model.Beat) (int64, error) {
	return h.db.Insert(ctx, "heartbeat", beatToMap(b))
}

func (h *HeartbeatStore) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(beatFlushEvery)
	defer ticker.Stop()

	batch := make([]model.Beat, 0, beatFlushMaxRows)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := h.writeBatch(ctx, batch); err != nil {
			logx.Errorf("heartbeat", "批量写入失败: %v", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-h.stop:
			// 排空通道后落盘
			for {
				select {
				case b := <-h.ch:
					batch = append(batch, b)
					if len(batch) >= beatFlushMaxRows {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case b := <-h.ch:
			batch = append(batch, b)
			if len(batch) >= beatFlushMaxRows {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (h *HeartbeatStore) writeBatch(ctx context.Context, beats []model.Beat) error {
	return h.db.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO heartbeat (important, monitor_id, status, msg, time, ping, duration, down_count)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for _, b := range beats {
			var ping any
			if b.Ping != nil {
				ping = *b.Ping
			}
			if _, err := stmt.ExecContext(ctx,
				boolToInt(b.Important), b.MonitorID, b.Status, b.Msg, b.Time, ping, b.Duration, b.DownCount,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func beatToMap(b model.Beat) map[string]any {
	m := map[string]any{
		"monitor_id": b.MonitorID,
		"status":     b.Status,
		"msg":        b.Msg,
		"time":       b.Time,
		"duration":   b.Duration,
		"important":  b.Important,
		"down_count": b.DownCount,
	}
	if b.Ping != nil {
		m["ping"] = *b.Ping
	}
	return m
}

// Recent 返回最近 limit 条心跳，按时间升序（与官方 getMonitorBeats 一致）。
func (h *HeartbeatStore) Recent(ctx context.Context, monitorID int64, limit int) ([]model.Beat, error) {
	rows, err := h.db.Query(ctx, `
		SELECT * FROM (
			SELECT * FROM heartbeat WHERE monitor_id = ? ORDER BY time DESC LIMIT ?
		) ORDER BY time ASC`, monitorID, limit)
	if err != nil {
		return nil, err
	}
	return toBeats(rows), nil
}

// Paged 分页返回重要心跳（状态变化），用于事件列表。
func (h *HeartbeatStore) Paged(ctx context.Context, monitorID int64, limit, offset int) ([]model.Beat, error) {
	rows, err := h.db.Query(ctx, `
		SELECT * FROM (
			SELECT * FROM heartbeat
			WHERE monitor_id = ? AND important = 1
			ORDER BY time DESC LIMIT ? OFFSET ?
		) ORDER BY time ASC`, monitorID, limit, offset)
	if err != nil {
		return nil, err
	}
	return toBeats(rows), nil
}

// CountImportant 统计重要心跳条数。
func (h *HeartbeatStore) CountImportant(ctx context.Context, monitorID int64) (int64, error) {
	row, err := h.db.QueryOne(ctx,
		"SELECT COUNT(*) AS n FROM heartbeat WHERE monitor_id = ? AND important = 1", monitorID)
	if err != nil || row == nil {
		return 0, err
	}
	return row.Int("n"), nil
}

// Range 返回时间区间内的心跳，用于图表与 uptime 计算。
func (h *HeartbeatStore) Range(ctx context.Context, monitorID int64, from, to time.Time) ([]model.Beat, error) {
	rows, err := h.db.Query(ctx, `
		SELECT * FROM heartbeat
		WHERE monitor_id = ? AND time >= ? AND time <= ?
		ORDER BY time ASC`, monitorID, FormatTime(from), FormatTime(to))
	if err != nil {
		return nil, err
	}
	return toBeats(rows), nil
}

// Clear 清空某监控的心跳。
func (h *HeartbeatStore) Clear(ctx context.Context, monitorID int64) error {
	_, err := h.db.Exec(ctx, "DELETE FROM heartbeat WHERE monitor_id = ?", monitorID)
	return err
}

// ClearAll 清空全部心跳。
func (h *HeartbeatStore) ClearAll(ctx context.Context) error {
	_, err := h.db.Exec(ctx, "DELETE FROM heartbeat")
	return err
}

// StatRows 供 uptime 包读取聚合表。
func (h *HeartbeatStore) StatRows(ctx context.Context, table string, monitorID int64, from, to int64) ([]Row, error) {
	switch table {
	case "stat_minutely", "stat_hourly", "stat_daily":
	default:
		return nil, fmt.Errorf("非法的统计表名: %s", table)
	}
	return h.db.Query(ctx,
		fmt.Sprintf("SELECT * FROM %s WHERE monitor_id = ? AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp ASC", table),
		monitorID, from, to)
}

func toBeats(rows []Row) []model.Beat {
	out := make([]model.Beat, 0, len(rows))
	for _, r := range rows {
		b := model.Beat{
			ID:        r.Int("id"),
			MonitorID: r.Int("monitor_id"),
			Status:    int(r.Int("status")),
			Msg:       r.Str("msg"),
			Time:      r.Str("time"),
			Duration:  int(r.Int("duration")),
			Important: r.Bool("important"),
			DownCount: int(r.Int("down_count")),
		}
		if r["ping"] != nil {
			p := r.Int("ping")
			b.Ping = &p
		}
		out = append(out, b)
	}
	return out
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// StatusCount 是按状态分组的心跳计数。
type StatusCount struct {
	Status int64
	Count  int64
}

// CountByStatus 统计某个时间点之后各状态的心跳数，
// 供统计表缺失时回算可用率（例如刚接管的旧库还没聚合数据）。
func (h *HeartbeatStore) CountByStatus(ctx context.Context, monitorID, sinceUnix int64) ([]StatusCount, error) {
	since := time.Unix(sinceUnix, 0).UTC().Format(TimeLayout)
	rows, err := h.db.Query(ctx,
		`SELECT status AS st, COUNT(*) AS n FROM heartbeat
		 WHERE monitor_id = ? AND time >= ? GROUP BY status`, monitorID, since)
	if err != nil {
		return nil, err
	}

	out := make([]StatusCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, StatusCount{Status: r.Int("st"), Count: r.Int("n")})
	}
	return out, nil
}
