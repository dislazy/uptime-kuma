package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"github.com/dislazy/uptime-kuma/go_server/internal/socketio"
)

// ---------------- 列表 ----------------

// buildMonitorList 构造前端 dashboard 需要的 map[id]monitor。
func buildMonitorList(ctx context.Context, d *Deps) (map[int64]map[string]any, error) {
	list, err := d.Mon.List(ctx, true)
	if err != nil {
		return nil, err
	}

	// 先算出父子关系，monitorJSON 需要 childrenIDs
	children := map[int64][]int64{}
	for _, m := range list {
		if m.Parent != nil {
			children[*m.Parent] = append(children[*m.Parent], m.ID)
		}
	}

	out := make(map[int64]map[string]any, len(list))
	for _, m := range list {
		out[m.ID] = monitorJSON(m, children[m.ID])
	}
	return out, nil
}

// monitorJSON 把数据库行转成前端期望的 camelCase 结构。
//
// 做法：列名统一转 camelCase，再补上需要计算/转换的字段。
// 这样官方新增列时不用改代码也能带到前端。
func monitorJSON(m *model.Monitor, childrenIDs []int64) map[string]any {
	out := make(map[string]any, len(m.Raw)+8)

	for k, v := range m.Raw {
		// 下划线开头的是服务端内部字段（如 docker 的 _docker_daemon），不下发前端
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[jsKeyOf(k)] = v
	}

	// 基础字段兜底
	out["id"] = m.ID
	out["name"] = m.Name
	out["type"] = m.Type
	out["active"] = m.Active
	out["interval"] = m.Interval
	out["parent"] = m.Parent

	// accepted_statuscodes_json → accepted_statuscodes 数组
	codes := []string{"200-299"}
	if raw, ok := out["acceptedStatuscodesJson"].(string); ok && raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil && len(parsed) > 0 {
			codes = parsed
		}
	} else if raw, ok := out["accepted_statuscodes_json"].(string); ok && raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil && len(parsed) > 0 {
			codes = parsed
		}
	}
	out["accepted_statuscodes"] = codes
	delete(out, "acceptedStatuscodesJson")

	// JSON 字符串字段还原成数组/对象，前端按结构体读。
	// 这份清单对应官方 toJSON 里做 JSON.parse 的字段，缺一个就会让编辑表单拿到字符串。
	// config 是 2026-10 起官方新增的 JSON 列（systemServiceName 等 per-type 字段都收进来了），
	// 官方 toJSON 里是 config: this.getConfig()，返回解析后的对象，前端编辑表单按对象读。
	for _, k := range []string{"conditions", "rabbitmqNodes", "kafkaProducerBrokers", "kafkaProducerSaslOptions", "config"} {
		if raw, ok := out[k].(string); ok && raw != "" {
			var parsed any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				out[k] = parsed
			}
		}
	}

	// 通知与标签
	ids := make([]int64, 0, len(m.Notifications))
	ids = append(ids, m.Notifications...)
	out["notificationIDList"] = ids

	tags := make([]map[string]any, 0, len(m.Tags))
	for _, t := range m.Tags {
		tags = append(tags, map[string]any{
			"tag_id": t.TagID,
			"name":   t.Name,
			"color":  t.Color,
			"value":  t.Value,
		})
	}
	out["tags"] = tags

	// 树形展示相关
	if childrenIDs == nil {
		childrenIDs = []int64{}
	}
	out["childrenIDs"] = childrenIDs
	out["path"] = []string{}
	out["pathName"] = m.Name

	// 前端会调 monitor.getUrl()，由 socket.js 里的 assignMonitorUrlParser 注入，这里不需要提供

	return out
}

// jsKeyOverrides 是"列名与前端字段名不一致"的例外表。
//
// 官方 server.js 里既有 bean.basic_auth_user = monitor.basic_auth_user 这种
// 全 snake_case 的字段，也有 bean.upsideDown = monitor.upsideDown 这种全 camelCase 的。
// 不能简单地把所有列名驼峰化，否则前端读不到值（编辑表单会丢字段）。
var jsKeyOverrides = map[string]string{
	"basic_auth_user":                   "basic_auth_user",
	"basicAuthUser":                     "basic_auth_user",
	"basic_auth_pass":                   "basic_auth_pass",
	"basicAuthPass":                     "basic_auth_pass",
	"oauth_client_id":                   "oauth_client_id",
	"oauthClientId":                     "oauth_client_id",
	"oauth_client_secret":               "oauth_client_secret",
	"oauthClientSecret":                 "oauth_client_secret",
	"oauth_auth_method":                 "oauth_auth_method",
	"oauthAuthMethod":                   "oauth_auth_method",
	"oauth_token_url":                   "oauth_token_url",
	"oauthTokenUrl":                     "oauth_token_url",
	"oauth_scopes":                      "oauth_scopes",
	"oauthScopes":                       "oauth_scopes",
	"oauth_audience":                    "oauth_audience",
	"oauthAudience":                     "oauth_audience",
	"dns_resolve_type":                  "dns_resolve_type",
	"dnsResolveType":                    "dns_resolve_type",
	"dns_resolve_server":                "dns_resolve_server",
	"dnsResolveServer":                  "dns_resolve_server",
	"dns_last_result":                   "dns_last_result",
	"dnsLastResult":                     "dns_last_result",
	"docker_container":                  "docker_container",
	"dockerContainer":                   "docker_container",
	"docker_host":                       "docker_host",
	"dockerHost":                        "docker_host",
	"manual_status":                     "manual_status",
	"manualStatus":                      "manual_status",
	"screenshot_delay":                  "screenshot_delay",
	"screenshotDelay":                   "screenshot_delay",
	"remote_browser":                    "remote_browser",
	"remoteBrowser":                     "remote_browser",
	"ping_numeric":                      "ping_numeric",
	"pingNumeric":                       "ping_numeric",
	"ping_count":                        "ping_count",
	"pingCount":                         "ping_count",
	"ping_per_request_timeout":          "ping_per_request_timeout",
	"pingPerRequestTimeout":             "ping_per_request_timeout",
	"retry_only_on_status_code_failure": "retry_only_on_status_code_failure",
	"retryOnlyOnStatusCodeFailure":      "retry_only_on_status_code_failure",
	"created_date":                      "createdDate",
	"user_id":                           "userId",
}

// jsKeyOf 决定某个数据库列在前端 JSON 里叫什么。
func jsKeyOf(column string) string {
	if js, ok := jsKeyOverrides[column]; ok {
		return js
	}
	return snakeToCamel(column)
}

func snakeToCamel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	upper := false
	for _, r := range s {
		if r == '_' {
			upper = true
			continue
		}
		if upper && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 'a' + 'A')
			upper = false
			continue
		}
		upper = false
		b.WriteRune(r)
	}
	return b.String()
}

// sendMonitorList 向单个连接推送监控列表。
func sendMonitorList(sock *socketio.Socket, d *Deps) {
	list, err := buildMonitorList(context.Background(), d)
	if err != nil {
		logx.Errorf("handler", "推送监控列表失败: %v", err)
		return
	}
	sock.Emit("monitorList", list)
}

// broadcastMonitorList 向所有连接推送监控列表（增删改后调用）。
func broadcastMonitorList(d *Deps) {
	list, err := buildMonitorList(context.Background(), d)
	if err != nil {
		logx.Errorf("handler", "广播监控列表失败: %v", err)
		return
	}
	d.Hub.Broadcast("monitorList", list)
}

// sendHeartbeatLists 一次性推送所有监控的近期心跳。
func sendHeartbeatLists(ctx context.Context, sock *socketio.Socket, d *Deps, monitorList map[int64]map[string]any) {
	for id := range monitorList {
		beats, err := d.Beat.Recent(ctx, id, 100)
		if err != nil {
			continue
		}
		payload := make([]map[string]any, 0, len(beats))
		for i := range beats {
			payload = append(payload, beats[i].JSON())
		}
		sock.Emit("heartbeatList", id, payload, true)
	}
}

// buildStats 汇总每个监控的看板统计（uptime24 / uptime720 / uptime1y + avgPing）。
//
// 对齐官方 server.js 里 sendDeferredLists 的 MonitorStats.getCached/refresh：
// 前端 socket.js 的 socket.on("stats") 按 data[monitorID].uptime24 读，
// 塞进 $root.uptimeList，监控列表左侧的可用率胶囊（Uptime.vue）才有值。
// 这里返回 map[string]any，JSON 序列化后 key 就是前端用的 "22"。
func buildStats(ctx context.Context, d *Deps, monitorList map[int64]map[string]any) map[string]any {
	if d.Mgr == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(monitorList))
	for id := range monitorList {
		out[strconv.FormatInt(id, 10)] = d.Mgr.Stats(ctx, id)
	}
	return out
}

// ---------------- 事件 ----------------

// addMonitor 对应官方 "add" 事件。
func addMonitor(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	values, err := monitorColumns(payload)
	if err != nil {
		return nil, err
	}

	// 必填兜底
	if name := normalizeName(asString(payload["name"])); name != "" {
		values["name"] = name
	} else {
		return nil, fmt.Errorf("监控名称不能为空")
	}
	if typ := asString(payload["type"]); typ != "" {
		values["type"] = typ
	} else {
		return nil, fmt.Errorf("监控类型不能为空")
	}

	values["active"] = true
	values["created_date"] = time.Now().UTC().Format("2006-01-02 15:04:05")

	// push 类型需要 token
	if values["type"] == "push" {
		if tok := asString(payload["pushToken"]); tok != "" {
			values["push_token"] = tok
		} else {
			values["push_token"] = newPushToken()
		}
	}

	id, err := c.Mon.Create(c.Ctx, values)
	if err != nil {
		return nil, err
	}

	// 关联关系
	if ids, ok := payload["notificationIDList"]; ok {
		_ = c.Mon.ResolveNotificationIDs(c.Ctx, id, toInt64Slice(ids))
	}
	if tags, ok := payload["tags"]; ok {
		_ = c.Mon.ResolveTagIDs(c.Ctx, id, toTags(tags))
	}

	if err := c.Mgr.Upsert(c.Ctx, id); err != nil {
		logx.Warnf("handler", "监控 %d 加入调度失败: %v", id, err)
	}

	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "id": id}, nil
}

// editMonitor 对应官方 "editMonitor"。
func editMonitor(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	id := int64(numberOf(payload["id"]))
	if id <= 0 {
		return nil, fmt.Errorf("缺少监控 ID")
	}

	values, err := monitorColumns(payload)
	if err != nil {
		return nil, err
	}
	delete(values, "created_date")
	delete(values, "push_token") // 编辑时不允许改 token

	if name := normalizeName(asString(payload["name"])); name != "" {
		values["name"] = name
	}

	if err := c.Mon.Update(c.Ctx, id, values); err != nil {
		return nil, err
	}

	if ids, ok := payload["notificationIDList"]; ok {
		_ = c.Mon.ResolveNotificationIDs(c.Ctx, id, toInt64Slice(ids))
	}
	if tags, ok := payload["tags"]; ok {
		_ = c.Mon.ResolveTagIDs(c.Ctx, id, toTags(tags))
	}

	if err := c.Mgr.Upsert(c.Ctx, id); err != nil {
		logx.Warnf("handler", "监控 %d 刷新调度失败: %v", id, err)
	}

	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "monitorID": id, "id": id}, nil
}

// deleteMonitor 对应官方 "deleteMonitor"。
func deleteMonitor(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Mon.Delete(c.Ctx, id); err != nil {
		return nil, err
	}
	c.Mgr.Remove(id)

	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// getMonitor 返回单个监控的完整数据（编辑页面用）。
func getMonitor(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	m, err := c.Mon.Get(c.Ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("monitor not found")
	}

	return map[string]any{"ok": true, "monitor": monitorJSON(m, nil)}, nil
}

// getMonitorList 重新推送列表。
func getMonitorList(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}
	sendMonitorList(c.Socket, c.Deps)
	return nil, nil
}

// pauseMonitor 停用监控。
func pauseMonitor(c *Ctx) (any, error) {
	return setActive(c, false)
}

// resumeMonitor 启用监控。
func resumeMonitor(c *Ctx) (any, error) {
	return setActive(c, true)
}

func setActive(c *Ctx, active bool) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Mon.Update(c.Ctx, id, map[string]any{"active": active}); err != nil {
		return nil, err
	}
	if active {
		_ = c.Mgr.Upsert(c.Ctx, id)
	}

	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "successUpdated", "msgi18n": true}, nil
}

// ---------------- 心跳查询 ----------------

// getMonitorBeats 返回指定小时数内的心跳。入参 (monitorID, periodHours)。
func getMonitorBeats(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var (
		id     int64
		period float64 = 24
	)
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}
	_ = c.OptArg(1, &period)
	if period <= 0 {
		period = 24
	}

	to := time.Now()
	from := to.Add(-time.Duration(period * float64(time.Hour)))

	beats, err := c.Beat.Range(c.Ctx, id, from, to)
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(beats))
	for i := range beats {
		out = append(out, beats[i].JSON())
	}
	return map[string]any{"ok": true, "data": out}, nil
}

// getHeartbeatHistory 返回最近一段时间的心跳（HeartbeatBar 用）。
func getHeartbeatHistory(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	to := time.Now()
	from := to.Add(-24 * time.Hour)

	beats, err := c.Beat.Range(c.Ctx, id, from, to)
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(beats))
	for i := range beats {
		out = append(out, beats[i].JSON())
	}
	return map[string]any{"ok": true, "data": out}, nil
}

// getMonitorChartData 按粒度返回图表数据。入参 (monitorID, periodHours)。
func getMonitorChartData(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var (
		id     int64
		period float64 = 24
	)
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}
	_ = c.OptArg(1, &period)

	now := time.Now().Unix()
	hours := period
	if hours <= 0 {
		hours = 24
	}

	var (
		table = "stat_minutely"
		from  = now - int64(hours*3600)
	)
	switch {
	case hours > 24*30:
		table = "stat_daily"
	case hours > 24:
		table = "stat_hourly"
	}

	rows, err := c.Stat.Series(c.Ctx, table, id, from, now)
	if err != nil {
		return nil, err
	}

	points := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		up := r.Int("up")
		down := r.Int("down")
		total := up + down
		uptime := 0.0
		if total > 0 {
			uptime = float64(up) / float64(total) * 100
		}
		points = append(points, map[string]any{
			"timestamp": r.Int("timestamp") * 1000,
			"uptime":    math2(uptime),
			"ping":      math2(r.Float("ping")),
			"up":        up,
			"down":      down,
		})
	}

	return map[string]any{"ok": true, "data": points, "period": period}, nil
}

// importantCount 重要心跳条数。monitorID 可为 null（表示全部）。
func importantCount(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id *int64
	_ = c.OptArg(0, &id)

	var (
		n   int64
		err error
	)
	if id == nil {
		row, e := c.DB.QueryOne(c.Ctx, "SELECT COUNT(*) AS n FROM heartbeat WHERE important = 1")
		if e == nil && row != nil {
			n = row.Int("n")
		}
		err = e
	} else {
		n, err = c.Beat.CountImportant(c.Ctx, *id)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "count": n}, nil
}

// importantPaged 分页返回重要心跳。入参 (monitorID|null, offset, limit)。
func importantPaged(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var (
		id     *int64
		offset int
		limit  = 25
	)
	_ = c.OptArg(0, &id)
	_ = c.OptArg(1, &offset)
	_ = c.OptArg(2, &limit)
	if limit <= 0 {
		limit = 25
	}

	var (
		beats []model.Beat
		err   error
	)
	if id == nil {
		rows, e := c.DB.Query(c.Ctx, `
			SELECT * FROM (
				SELECT * FROM heartbeat WHERE important = 1
				ORDER BY time DESC LIMIT ? OFFSET ?
			) AS t ORDER BY time ASC`, limit, offset)
		if e == nil {
			for _, r := range rows {
				b := model.Beat{
					MonitorID: r.Int("monitor_id"),
					Status:    int(r.Int("status")),
					Msg:       r.Str("msg"),
					Time:      r.Str("time"),
					Duration:  int(r.Int("duration")),
					Important: true,
				}
				if r["ping"] != nil {
					v := r.Int("ping")
					b.Ping = &v
				}
				beats = append(beats, b)
			}
		}
		err = e
	} else {
		beats, err = c.Beat.Paged(c.Ctx, *id, limit, offset)
	}
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(beats))
	for i := range beats {
		out = append(out, beats[i].JSON())
	}
	return map[string]any{"ok": true, "data": out}, nil
}

// ---------------- 字段转换 ----------------

// 不写入 monitor 表的字段（关联关系或前端计算字段）。
var skipMonitorKeys = map[string]bool{
	"id":                 true,
	"notificationIDList": true,
	"tags":               true,
	"childrenIDs":        true,
	"path":               true,
	"pathName":           true,
	"getUrl":             true,
	"pushToken":          true,
	"isDefault":          true,
}

// 需要序列化成 JSON 字符串的字段。
var jsonMonitorKeys = map[string]bool{
	"headers":                  true,
	"kafkaProducerBrokers":     true,
	"kafkaProducerSaslOptions": true,
	"rabbitmqNodes":            true,
	"conditions":               true,
	"grpcMetadata":             true,
	"config":                   true,
}

// monitorColumns 把前端对象转成可写库的列集合。
func monitorColumns(payload map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(payload))

	for k, v := range payload {
		if skipMonitorKeys[k] || v == nil {
			continue
		}

		switch {
		case k == "accepted_statuscodes":
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			out["accepted_statuscodes_json"] = string(b)

		case jsonMonitorKeys[k]:
			out[k] = asString(v)

		default:
			out[k] = jsonTypeOf(v)
		}
	}

	return out, nil
}

func toInt64Slice(v any) []int64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(arr))
	for _, item := range arr {
		out = append(out, int64(numberOf(item)))
	}
	return out
}

func toTags(v any) []model.TagValue {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]model.TagValue, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, model.TagValue{
			TagID: int64(numberOf(m["tag_id"])),
			Value: asString(m["value"]),
		})
	}
	return out
}

func numberOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case string:
		var f float64
		_, _ = fmt.Sscan(t, &f)
		return f
	default:
		return 0
	}
}

func math2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// newPushToken 生成 32 位推送 token（与官方长度一致）。
func newPushToken() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = charset[uint64(seed)>>33%uint64(len(charset))]
	}
	return string(b)
}
