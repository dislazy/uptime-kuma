package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// HandlePush 处理一次被动推送（/api/push/:token）。
//
// 流程与主动检查一致：落库 → 聚合 → 广播 → 状态变化时通知。
func (mgr *Manager) HandlePush(ctx context.Context, token, statusStr, msg string, ping *int64) error {
	if token == "" {
		return fmt.Errorf("push token 不能为空")
	}

	m, err := mgr.mon.FindByPushToken(ctx, token)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("无效的 push token")
	}

	status := model.StatusUp
	switch statusStr {
	case "down", "0":
		status = model.StatusDown
	case "up", "1", "":
		status = model.StatusUp
	default:
		return fmt.Errorf("status 只能是 up 或 down")
	}

	if msg == "" {
		if status == model.StatusDown {
			msg = "Received a down push"
		} else {
			msg = "OK"
		}
	}

	// 取上一条心跳判断是否状态变化
	prevStatus := -1
	if beats, err := mgr.beats.Recent(ctx, m.ID, 1); err == nil && len(beats) > 0 {
		prevStatus = beats[len(beats)-1].Status
	}

	now := time.Now()
	beat := model.Beat{
		MonitorID: m.ID,
		Status:    status,
		Msg:       msg,
		Time:      store.FormatTime(now),
		Ping:      ping,
		Important: prevStatus >= 0 && prevStatus != status,
	}

	if _, err := mgr.beats.Save(ctx, beat); err != nil {
		return err
	}

	if ping != nil {
		minuteTS := now.Unix() / 60 * 60
		_ = mgr.stats.UpsertMinutely(ctx, m.ID, minuteTS, *ping, status)
	}
	mgr.invalidateStats(m.ID)

	payload := beat.JSON()
	payload["stats"] = mgr.statsFor(ctx, m.ID)
	mgr.hub.Broadcast("heartbeat", payload)

	if beat.Important && mgr.notify != nil && prevStatus >= 0 {
		mgr.notify.Notify(ctx, m, beat, prevStatus)
	}

	return nil
}
