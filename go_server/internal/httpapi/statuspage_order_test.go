package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestStatusPageHeartbeatOrderIsAscending 锁住公开心跳接口的顺序。
//
// 官方 status-page-router.js 是「按 time DESC 取 100 条 → reverse」，
// 也就是返回**正序**（最后一个是最新），前端统一用 beats[len-1] 当当前状态
// （src/mixins/socket.js、src/beat-state.ts）。
//
// 2026-11-21 线上：这里多反转了一次，返回倒序，前端把 100 条里最旧的当成当前
// 状态——服务已经恢复 UP，页面却一直显示几小时前的 DOWN。
func TestStatusPageHeartbeatOrderIsAscending(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()

	// 状态页 + 分组 + 成员
	spID, err := api.StatusPage.Create(ctx, "测试状态页", "check")
	if err != nil {
		t.Fatalf("创建状态页失败: %v", err)
	}
	if err := api.StatusPage.SetPublished(ctx, spID, true); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	// monitor_group.monitor_id 有外键，先建一个监控
	res, err := api.DB.Exec(ctx,
		`INSERT INTO monitor (name, type, active, user_id, interval, retry_interval, maxretries, upside_down, weight, created_date)
		 VALUES ('m', 'http', 1, NULL, 60, 60, 0, 0, 0, '2026-11-21 00:00:00')`)
	if err != nil {
		t.Fatalf("插入监控失败: %v", err)
	}
	monitorID, _ := res.LastInsertId()

	res, err = api.DB.Exec(ctx, `INSERT INTO "group" (name, status_page_id, public, weight) VALUES (?, ?, 1, 0)`, "G1", spID)
	if err != nil {
		t.Fatalf("插入分组失败: %v", err)
	}
	groupID, _ := res.LastInsertId()
	if _, err := api.DB.Exec(ctx,
		`INSERT INTO monitor_group (monitor_id, group_id, weight) VALUES (?, ?, 0)`, monitorID, groupID); err != nil {
		t.Fatalf("插入分组成员失败: %v", err)
	}

	// 三条心跳：老 → 新，最旧是 DOWN，最新是 UP
	beats := []struct {
		time   string
		status int
	}{
		{"2026-11-21 10:00:00.000", 0},
		{"2026-11-21 10:10:00.000", 1},
		{"2026-11-21 10:20:00.000", 1},
	}
	for _, b := range beats {
		if _, err := api.DB.Exec(ctx,
			`INSERT INTO heartbeat (important, monitor_id, status, msg, time, duration, down_count) VALUES (0, ?, ?, '', ?, 0, 0)`,
			monitorID, b.status, b.time); err != nil {
			t.Fatalf("插入心跳失败: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status-page/heartbeat/check", nil)
	req.SetPathValue("slug", "check")
	rec := httptest.NewRecorder()
	api.handleStatusPageHeartbeat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		HeartbeatList map[string][]struct {
			Status int    `json:"status"`
			Time   string `json:"time"`
		} `json:"heartbeatList"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}

	list := out.HeartbeatList[strconv.FormatInt(monitorID, 10)]
	if len(list) != len(beats) {
		t.Fatalf("心跳条数 %d，期望 %d", len(list), len(beats))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Time >= list[i].Time {
			t.Fatalf("心跳必须按时间正序：第 %d 条 %s 不早于第 %d 条 %s",
				i-1, list[i-1].Time, i, list[i].Time)
		}
	}
	if last := list[len(list)-1]; last.Status != 1 || last.Time != "2026-11-21 10:20:00.000" {
		t.Fatalf("最后一条必须是最新心跳（前端用 beats[len-1] 当当前状态），实际 %+v", last)
	}
}
