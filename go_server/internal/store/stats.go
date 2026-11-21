package store

import (
	"context"
	"fmt"
)

// StatsStore 负责 stat_minutely / stat_hourly / stat_daily 三张聚合表。
//
// 设计：每次心跳只写"分钟"粒度（一次 upsert，代价极小），
// 再定时把分钟聚合成小时、小时聚合成天。这样 1 年数据也只有 ~3.6 万行/监控，
// 前端拉 1 年 uptime 不需要扫全量 heartbeat。
type StatsStore struct {
	db *DB
}

// NewStatsStore 构造。
func NewStatsStore(db *DB) *StatsStore { return &StatsStore{db: db} }

// UpsertMinutely 把一次心跳累加到当前分钟的聚合行。
func (s *StatsStore) UpsertMinutely(ctx context.Context, monitorID, minuteTS, ping int64, status int) error {
	up, down := 0, 0
	switch status {
	case 1:
		up = 1
	case 0:
		down = 1
	default:
		// PENDING / MAINTENANCE 不计入 uptime 分母
		return nil
	}

	_, err := s.db.Exec(ctx, upsertMinutelySQL(s.db), monitorID, minuteTS, ping, up, down)
	return err
}

// upsertMinutelySQL 返回当前方言的分钟聚合 UPSERT 语句。
//
// 两种方言的写法差异：
//
//	SQLite  ON CONFLICT(...) DO UPDATE + excluded.x + MAX(a,b)
//	MySQL   ON DUPLICATE KEY UPDATE + VALUES(x) + GREATEST(a,b)
//
// MySQL 的 MAX() 是聚合函数，两参数取大必须用 GREATEST，这是最容易写错的一处。
func upsertMinutelySQL(db *DB) string {
	if db.isMySQL() {
		return `INSERT INTO stat_minutely (monitor_id, timestamp, ping, up, down)
		        VALUES (?, ?, ?, ?, ?)
		        ON DUPLICATE KEY UPDATE
		            ping = (stat_minutely.ping * (stat_minutely.up + stat_minutely.down) + VALUES(ping))
		                   / GREATEST(stat_minutely.up + stat_minutely.down + 1, 1),
		            up   = stat_minutely.up + VALUES(up),
		            down = stat_minutely.down + VALUES(down)`
	}
	return `INSERT INTO stat_minutely (monitor_id, timestamp, ping, up, down)
	        VALUES (?, ?, ?, ?, ?)
	        ON CONFLICT(monitor_id, timestamp) DO UPDATE SET
	            ping = (stat_minutely.ping * (stat_minutely.up + stat_minutely.down) + excluded.ping)
	                   / MAX(stat_minutely.up + stat_minutely.down + 1, 1),
	            up   = stat_minutely.up + excluded.up,
	            down = stat_minutely.down + excluded.down`
}

// RollupHourly 把 [before-window, before) 的分钟数据聚合成小时数据（幂等）。
func (s *StatsStore) RollupHourly(ctx context.Context, beforeMinute int64) error {
	// 只回看 6 小时，重启后补数据不会扫全表
	return s.rollup(ctx, "stat_minutely", "stat_hourly", 3600, beforeMinute, beforeMinute-6*3600)
}

// RollupDaily 把 [before-window, before) 的小时数据聚合成天数据（幂等）。
func (s *StatsStore) RollupDaily(ctx context.Context, beforeHour int64) error {
	return s.rollup(ctx, "stat_hourly", "stat_daily", 86400, beforeHour, beforeHour-48*3600)
}

func (s *StatsStore) rollup(ctx context.Context, src, dst string, bucket, before, from int64) error {
	// 已是聚合行，直接按桶重算，避免重复累加。
	//
	// 方言差异两处：
	//   - CAST(... AS INTEGER) —— MySQL 没有 INTEGER 这个转换目标，要用 SIGNED；
	//   - ON CONFLICT / excluded.x —— MySQL 用 ON DUPLICATE KEY UPDATE / VALUES(x)。
	castType := "INTEGER"
	upsert := `ON CONFLICT(monitor_id, timestamp) DO UPDATE SET
		    ping = excluded.ping,
		    up   = excluded.up,
		    down = excluded.down`
	if s.db.isMySQL() {
		castType = "SIGNED"
		upsert = `ON DUPLICATE KEY UPDATE
		    ping = VALUES(ping),
		    up   = VALUES(up),
		    down = VALUES(down)`
	}

	q := fmt.Sprintf(`
		INSERT INTO %[2]s (monitor_id, timestamp, ping, up, down)
		SELECT monitor_id,
		       CAST(timestamp / %[3]d AS %[4]s) * %[3]d AS bucket,
		       CASE WHEN SUM(up + down) > 0
		            THEN SUM(ping * (up + down)) / SUM(up + down)
		            ELSE 0 END,
		       SUM(up),
		       SUM(down)
		FROM %[1]s
		WHERE timestamp < ? AND timestamp >= ?
		GROUP BY monitor_id, bucket
		%[5]s`, src, dst, bucket, castType, upsert)

	_, err := s.db.Exec(ctx, q, before, from)
	return err
}

// Prune 清理指定表里早于 cutoff 的聚合数据。
func (s *StatsStore) Prune(ctx context.Context, table string, cutoff int64) error {
	switch table {
	case "stat_minutely", "stat_hourly", "stat_daily":
	default:
		return fmt.Errorf("非法的统计表名: %s", table)
	}
	_, err := s.db.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE timestamp < ?", table), cutoff)
	return err
}

// Window 是查询区间的统计结果。
type Window struct {
	Up    int64
	Down  int64
	Ping  float64
	Count int64
}

// Percent 返回 uptime 百分比（0–100）。没有样本时返回 0。
func (w Window) Percent() float64 {
	total := w.Up + w.Down
	if total == 0 {
		return 0
	}
	return float64(w.Up) / float64(total) * 100
}

// Query 汇总指定表在 [from, to] 区间内的数据。
func (s *StatsStore) Query(ctx context.Context, table string, monitorID, from, to int64) (Window, error) {
	switch table {
	case "stat_minutely", "stat_hourly", "stat_daily":
	default:
		return Window{}, fmt.Errorf("非法的统计表名: %s", table)
	}

	q := fmt.Sprintf(`
		SELECT COALESCE(SUM(up), 0) AS up,
		       COALESCE(SUM(down), 0) AS down,
		       COALESCE(SUM(ping * (up + down)) / NULLIF(SUM(up + down), 0), 0) AS ping,
		       COUNT(*) AS cnt
		FROM %s WHERE monitor_id = ? AND timestamp >= ? AND timestamp <= ?`, table)

	row, err := s.db.QueryOne(ctx, q, monitorID, from, to)
	if err != nil {
		return Window{}, err
	}
	if row == nil {
		return Window{}, nil
	}
	return Window{
		Up:    row.Int("up"),
		Down:  row.Int("down"),
		Ping:  row.Float("ping"),
		Count: row.Int("cnt"),
	}, nil
}

// Series 返回图表用的时间序列。
func (s *StatsStore) Series(ctx context.Context, table string, monitorID, from, to int64) ([]Row, error) {
	switch table {
	case "stat_minutely", "stat_hourly", "stat_daily":
	default:
		return nil, fmt.Errorf("非法的统计表名: %s", table)
	}

	q := fmt.Sprintf(`SELECT timestamp, ping, up, down FROM %s
	                  WHERE monitor_id = ? AND timestamp >= ? AND timestamp <= ?
	                  ORDER BY timestamp ASC`, table)
	return s.db.Query(ctx, q, monitorID, from, to)
}

// ClearForMonitor 清空某监控的聚合数据。
func (s *StatsStore) ClearForMonitor(ctx context.Context, monitorID int64) error {
	for _, t := range []string{"stat_minutely", "stat_hourly", "stat_daily"} {
		if _, err := s.db.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE monitor_id = ?", t), monitorID); err != nil {
			return err
		}
	}
	return nil
}
