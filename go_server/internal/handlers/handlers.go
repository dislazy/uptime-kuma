// Package handlers 实现所有 socket 事件。
//
// 一个事件一个函数，签名统一为 func(*Ctx) (any, error)：
//   - 返回值会被自动包装成 ack（nil 视为 {ok:true}）
//   - 返回 error 会被包装成 {ok:false, msg:...}
//
// 这样新增事件只需要写函数 + 在 Register 里挂一行，不用关心协议细节。
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/auth"
	"github.com/songbo/uptime-kuma/go_server/internal/checkers"
	"github.com/songbo/uptime-kuma/go_server/internal/conditions"
	"github.com/songbo/uptime-kuma/go_server/internal/config"
	"github.com/songbo/uptime-kuma/go_server/internal/hub"
	"github.com/songbo/uptime-kuma/go_server/internal/logx"
	"github.com/songbo/uptime-kuma/go_server/internal/maintenance"
	"github.com/songbo/uptime-kuma/go_server/internal/monitor"
	"github.com/songbo/uptime-kuma/go_server/internal/notify"
	"github.com/songbo/uptime-kuma/go_server/internal/socketio"
	"github.com/songbo/uptime-kuma/go_server/internal/store"
	"github.com/songbo/uptime-kuma/go_server/internal/version"
)

// Deps 是 handler 依赖的所有服务。
type Deps struct {
	Cfg        *config.Config
	DB         *store.DB
	Mon        *store.MonitorStore
	Beat       *store.HeartbeatStore
	Stat       *store.StatsStore
	Notif      *store.NotificationStore
	Set        *store.Settings
	Tag        *store.TagStore
	Maint      *store.MaintenanceStore
	StatusPage *store.StatusPageStore
	Auth       *auth.Service
	Hub        *hub.Hub
	Mgr        *monitor.Manager
	MaintMgr   *maintenance.Manager
	Eng        *notify.Engine

	StartedAt time.Time
}

// Ctx 是单次事件调用的上下文。
type Ctx struct {
	*Deps
	Socket *socketio.Socket
	Args   []json.RawMessage
	Ctx    context.Context

	mu     sync.Mutex
	userID string
}

// UserID 返回当前登录用户 ID；未登录返回空串。
func (c *Ctx) UserID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID
}

// RequireLogin 对应官方 checkLogin(socket)，未登录直接报错。
func (c *Ctx) RequireLogin() error {
	if c.Cfg.DisableAuth {
		return nil
	}
	if c.RequireUserID() == "" {
		return errors.New("please login")
	}
	return nil
}

// RequireUserID 取用户 ID，未登录返回空串。
func (c *Ctx) RequireUserID() string {
	if id := c.UserID(); id != "" {
		return id
	}
	return c.Socket.UserID()
}

// Arg 取第 i 个参数并反序列化到 dst。
func (c *Ctx) Arg(i int, dst any) error {
	if i >= len(c.Args) {
		return fmt.Errorf("缺少第 %d 个参数", i)
	}
	if err := json.Unmarshal(c.Args[i], dst); err != nil {
		return fmt.Errorf("参数 %d 解析失败: %w", i, err)
	}
	return nil
}

// OptArg 取可选参数，缺失时不报错。
func (c *Ctx) OptArg(i int, dst any) bool {
	if i >= len(c.Args) {
		return false
	}
	return json.Unmarshal(c.Args[i], dst) == nil
}

// Handler 是事件处理函数。
type Handler func(*Ctx) (any, error)

// Register 绑定服务端回调与全部事件。
func Register(srv *socketio.Server, d *Deps) {
	srv.OnConnect = func(sock *socketio.Socket) {
		d.Hub.Add(sock)
		bootstrap(sock, d)
	}
	srv.OnDisconnect = func(sock *socketio.Socket) {
		d.Hub.Remove(sock)
	}
}

// bootstrap 是连接建立后的初始化，严格对齐官方 io.on("connection") 的顺序。
func bootstrap(sock *socketio.Socket, d *Deps) {
	ctx := context.Background()

	// 1) 先发 info —— 前端登录页也要显示版本号
	emitInfo(sock, d, false)

	// 2) 是否需要初始化
	hasUser, err := d.Auth.HasUser(ctx)
	if err != nil {
		logx.Warnf("handler", "检查用户失败: %v", err)
	}
	if !hasUser && !d.Cfg.DisableAuth {
		sock.Emit("setup")
	}

	// 3) 解析会话
	var user *auth.User
	if d.Cfg.DisableAuth {
		user = d.disableAuthUser(ctx)
	} else {
		user, _ = d.Auth.UserFromHeader(ctx, sock.Header().Get("Cookie"))
	}

	registerEvents(sock, d, user)

	if user != nil {
		sock.SetUserID(user.ID)
		sock.Emit("session", user.Username)
		afterLogin(sock, d, user)
	} else {
		sock.Emit("loginRequired")
	}
}

// afterLogin 是登录后的首屏数据推送，顺序与官方一致。
func afterLogin(sock *socketio.Socket, d *Deps, user *auth.User) {
	ctx := context.Background()

	monitorList, err := buildMonitorList(ctx, d)
	if err != nil {
		logx.Errorf("handler", "读取监控列表失败: %v", err)
	}
	sock.Emit("monitorList", monitorList)

	emitInfo(sock, d, true)

	// 其余列表并行推送
	if list, err := d.Notif.List(ctx); err == nil {
		flat := make([]map[string]any, 0, len(list))
		for _, n := range list {
			flat = append(flat, n.Flatten())
		}
		sock.Emit("notificationList", flat)
	}
	sock.Emit("maintenanceList", d.MaintMgr.JSONList())
	sock.Emit("monitorTypeList", monitorTypeList())
	sock.Emit("statusPageList", buildStatusPageList(ctx, d))
	sock.Emit("proxyList", []any{})
	sock.Emit("dockerHostList", buildDockerHostList(ctx, d))
	sock.Emit("remoteBrowserList", []any{})

	// 心跳历史：官方在 dashboardRendered 后延迟发送，这里直接发，
	// 前端同样能处理，且省掉一次往返。
	sendHeartbeatLists(ctx, sock, d, monitorList)
	sock.Emit("stats", map[int64]any{})
}

// registerEvents 注册全部事件处理器。
func registerEvents(sock *socketio.Socket, d *Deps, user *auth.User) {
	var userID string
	if user != nil {
		userID = user.ID
	}

	wrap := func(h Handler) socketio.Handler {
		return func(args []json.RawMessage) (any, error) {
			c := &Ctx{Deps: d, Socket: sock, Args: args, Ctx: context.Background(), userID: userID}
			return h(c)
		}
	}

	// —— 鉴权 ——
	sock.On("getWebpushVapidPublicKey", wrap(getWebpushVapidPublicKey))
	sock.On("needSetup", wrap(needSetup))
	sock.On("login", wrap(login))
	sock.On("loginByToken", wrap(loginByToken))

	// —— 监控 ——
	sock.On("add", wrap(addMonitor))
	sock.On("editMonitor", wrap(editMonitor))
	sock.On("deleteMonitor", wrap(deleteMonitor))
	sock.On("getMonitor", wrap(getMonitor))
	sock.On("getMonitorList", wrap(getMonitorList))
	sock.On("pauseMonitor", wrap(pauseMonitor))
	sock.On("resumeMonitor", wrap(resumeMonitor))
	sock.On("getMonitorBeats", wrap(getMonitorBeats))
	sock.On("getHeartbeatHistory", wrap(getHeartbeatHistory))
	sock.On("getMonitorChartData", wrap(getMonitorChartData))
	sock.On("monitorImportantHeartbeatListCount", wrap(importantCount))
	sock.On("monitorImportantHeartbeatListPaged", wrap(importantPaged))

	// —— 通知 ——
	sock.On("addNotification", wrap(addNotification))
	sock.On("deleteNotification", wrap(deleteNotification))
	sock.On("testNotification", wrap(testNotification))
	sock.On("checkApprise", wrap(checkApprise))

	// —— 标签 ——
	sock.On("getTags", wrap(getTags))
	sock.On("addTag", wrap(addTag))
	sock.On("editTag", wrap(editTag))
	sock.On("deleteTag", wrap(deleteTag))

	// —— 维护窗口 ——
	sock.On("addMaintenance", wrap(addMaintenance))
	sock.On("editMaintenance", wrap(editMaintenance))
	sock.On("deleteMaintenance", wrap(deleteMaintenance))
	sock.On("getMaintenance", wrap(getMaintenance))
	sock.On("getMaintenanceList", wrap(getMaintenanceList))
	sock.On("pauseMaintenance", wrap(pauseMaintenance))
	sock.On("resumeMaintenance", wrap(resumeMaintenance))
	sock.On("addMonitorMaintenance", wrap(addMonitorMaintenance))
	sock.On("addMaintenanceStatusPage", wrap(addMaintenanceStatusPage))
	sock.On("getMonitorMaintenance", wrap(getMonitorMaintenance))
	sock.On("getMaintenanceStatusPage", wrap(getMaintenanceStatusPage))

	// —— 状态页与公告 ——
	sock.On("addStatusPage", wrap(addStatusPage))
	sock.On("saveStatusPage", wrap(saveStatusPage))
	sock.On("deleteStatusPage", wrap(deleteStatusPage))
	sock.On("getStatusPage", wrap(getStatusPage))
	sock.On("postIncident", wrap(postIncident))
	sock.On("editIncident", wrap(editIncident))
	sock.On("deleteIncident", wrap(deleteIncident))
	sock.On("resolveIncident", wrap(resolveIncident))
	sock.On("unpinIncident", wrap(unpinIncident))
	sock.On("getIncidentHistory", wrap(getIncidentHistory))

	// —— Docker 主机 ——
	sock.On("addDockerHost", wrap(addDockerHost))
	sock.On("deleteDockerHost", wrap(deleteDockerHost))
	sock.On("testDockerHost", wrap(testDockerHost))

	// —— 设置与系统 ——
	sock.On("getSettings", wrap(getSettings))
	sock.On("setSettings", wrap(setSettings))
	sock.On("getDatabaseSize", wrap(getDatabaseSize))
	sock.On("shrinkDatabase", wrap(shrinkDatabase))
	sock.On("clearEvents", wrap(clearEvents))
	sock.On("clearHeartbeats", wrap(clearHeartbeats))
	sock.On("clearStatistics", wrap(clearStatistics))
	sock.On("getPushExample", wrap(getPushExample))
	sock.On("initServerTimezone", wrap(initServerTimezone))
	sock.On("disconnectOtherSocketClients", wrap(disconnectOtherSocketClients))

	// —— 尚未实现的周边功能：返回明确的占位结果，避免前端弹错误提示 ——
	sock.On("checkDomain", wrap(checkDomain))
	sock.On("getGameList", wrap(getGameList))
	sock.On("getPM2ProcessList", wrap(getPM2ProcessList))
	sock.On("testChrome", wrap(testChrome))

}

// ---------------- 通用工具 ----------------

// emitInfo 推送服务器信息。hideVersion 与官方一致：未登录不下发运行时细节。
func emitInfo(sock *socketio.Socket, d *Deps, full bool) {
	info := map[string]any{
		"version":        version.Version,
		"latestVersion":  version.Version,
		"serverTimezone": "UTC",
		"primaryBaseURL": "",
	}
	if full {
		info["runtime"] = map[string]any{
			"platform": "linux",
			"arch":     "amd64",
			"type":     "go",
		}
		info["dbType"] = "sqlite"
		info["isContainer"] = true
	}
	sock.Emit("info", info)
}

// monitorTypeList 返回支持的监控类型，前端用于判断某些 UI 是否可用。
// monitorTypeList 对应官方 sendMonitorTypeList。
//
// 返回的是对象而不是数组：前端用 monitorTypeList[type].supportsConditions
// 决定是否显示条件面板，用 .conditionVariables 渲染变量下拉框。
// 返回数组会让这两处全部拿到 undefined，条件功能直接消失。
func monitorTypeList() map[string]any {
	names := append([]string{}, checkers.Types()...)
	// push / group / manual 不走 checkers 注册表，但前端下拉框里都有
	names = append(names, "push", "group", "manual")

	out := make(map[string]any, len(names))
	for _, name := range names {
		support, vars := conditions.SupportFor(name)
		out[name] = map[string]any{
			"supportsConditions": support,
			"conditionVariables": vars,
		}
	}
	return out
}

// jsonTypeOf 把任意 JSON 值转成可写入数据库的 Go 值。
func jsonTypeOf(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	default:
		return t
	}
}

// asString 把 JSON 值转成字符串。
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// asBool 把 JSON 值转成布尔。
func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "true" || t == "1"
	default:
		return false
	}
}

func normalizeName(s string) string { return strings.TrimSpace(s) }
