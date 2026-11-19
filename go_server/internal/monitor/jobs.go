package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/logx"
)

// sprintfRetry 生成重试中的提示文案，与官方 "... (retry 1/3)" 格式对齐。
func sprintfRetry(msg string, retries, max int) string {
	return fmt.Sprintf("%s (retry %d/%d)", msg, retries, max)
}

// StartRollup 启动聚合与清理任务。
//
//   - 每 10 分钟：把 1 小时前的分钟数据滚成小时数据
//   - 每小时：把 1 天前的小时数据滚成天数据
//   - 每天：清理超期的分钟 / 小时数据，控制库体积（低配机器上很关键）
func (mgr *Manager) StartRollup(ctx context.Context, keepDays int) {
	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()

		rollupTicker := time.NewTicker(10 * time.Minute)
		pruneTicker := time.NewTicker(24 * time.Hour)
		defer rollupTicker.Stop()
		defer pruneTicker.Stop()

		// 启动时先补一次，避免重启后统计断档
		mgr.rollupOnce(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-mgr.stop:
				return
			case <-rollupTicker.C:
				mgr.rollupOnce(ctx)
			case <-pruneTicker.C:
				mgr.prune(ctx, keepDays)
			}
		}
	}()
}

func (mgr *Manager) rollupOnce(ctx context.Context) {
	now := time.Now().Unix()

	// 分钟 → 小时：只处理已经完整结束的分钟
	if err := mgr.stats.RollupHourly(ctx, now-3600); err != nil {
		logx.Debugf("monitor", "小时聚合失败: %v", err)
	}
	// 小时 → 天：只处理已经完整结束的小时
	if err := mgr.stats.RollupDaily(ctx, now-3600); err != nil {
		logx.Debugf("monitor", "天聚合失败: %v", err)
	}
}

func (mgr *Manager) prune(ctx context.Context, keepDays int) {
	if keepDays <= 0 {
		keepDays = 180
	}
	now := time.Now().Unix()

	// 分钟表只留 48 小时；小时表留 90 天；天表按 keepDays
	targets := []struct {
		table string
		cut   int64
	}{
		{"stat_minutely", now - 48*3600},
		{"stat_hourly", now - 90*86400},
		{"stat_daily", now - int64(keepDays)*86400},
	}

	for _, t := range targets {
		if err := mgr.stats.Prune(ctx, t.table, t.cut); err != nil {
			logx.Warnf("monitor", "清理 %s 失败: %v", t.table, err)
		}
	}
}
