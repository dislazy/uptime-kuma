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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/auth"
	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/conditions"
	"github.com/dislazy/uptime-kuma/go_server/internal/config"
	"github.com/dislazy/uptime-kuma/go_server/internal/domainexpiry"
	"github.com/dislazy/uptime-kuma/go_server/internal/hub"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/maintenance"
	"github.com/dislazy/uptime-kuma/go_server/internal/monitor"
	"github.com/dislazy/uptime-kuma/go_server/internal/notify"
	"github.com/dislazy/uptime-kuma/go_server/internal/socketio"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
	"github.com/dislazy/uptime-kuma/go_server/internal/version"
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
	DomainSvc  *domainexpiry.Service

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
	if err := unmarshalArg(c.Args[i], dst); err != nil {
		return fmt.Errorf("参数 %d 解析失败: %w", i, err)
	}
	return nil
}

// OptArg 取可选参数，缺失时不报错。
func (c *Ctx) OptArg(i int, dst any) bool {
	if i >= len(c.Args) {
		return false
	}
	return unmarshalArg(c.Args[i], dst) == nil
}

// unmarshalArg 在目标是指针整数、载荷却是数字字符串时，先剥引号再解析。
//
// 官方前端有不少事件把**路由参数**直接当 ID 传，例如 Details.vue:
//
//	socket.emit("getMonitor", this.$route.params.id, (res) => ...)
//
// vue-router 的 params.id 永远是字符串 "22"。官方 Node 后端把参数丢进 SQL
// （`WHERE id = ?`），SQLite/MySQL 会隐式把字符串转成数字，所以从来没人发现；
// Go 这边 json.Unmarshal(`"22"` → int64) 直接报
// "cannot unmarshal string into Go value of type int64"，事件回 {ok:false}，
// 前端 `if (res.ok)` 不成立 → 监控详情页（/dashboard/:id）整页空白。
func unmarshalArg(raw json.RawMessage, dst any) error {
	err := json.Unmarshal(raw, dst)
	if err == nil || !isIntegerPtr(dst) {
		return err
	}

	s, ok := numericString(raw)
	if !ok {
		return err
	}
	return json.Unmarshal([]byte(s), dst)
}

// numericString 判断载荷是否是「纯数字字符串」，是则返回去掉引号后的字面量。
func numericString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) < 2 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		return "", false
	}

	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return "", false
	}
	return s, true
}

// isIntegerPtr 判断 dst 是否指向一个整数（只有这种目标才做字符串兼容）。
func isIntegerPtr(dst any) bool {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return false
	}
	switch v.Elem().Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// Handler 是事件处理函数。
type Handler func(*Ctx) (any, error)

// Register 绑定服务端回调与全部事件。
func Register(srv *socketio.Server, d *Deps) {
	srv.OnConnect = func(sock *socketio.Socket) {
		d.Hub.Add(sock)

		// 事件注册必须同步完成，且要先于任何可能阻塞的操作：
		// 客户端连上后紧接着 emit 的第一个事件（前端是 login / needSetup）
		// 必须马上能被处理。官方 socket.io 也是在 connection 回调里同步注册的。
		user := attach(sock, d)

		// 首屏推送要读全量监控与心跳，放 goroutine 免得压住这条连接的读循环。
		go pushInitial(sock, d, user)
	}
	srv.OnDisconnect = func(sock *socketio.Socket) {
		d.Hub.Remove(sock)
	}
}

// attach 解析会话并注册事件处理器，返回当前登录用户（未登录为 nil）。
func attach(sock *socketio.Socket, d *Deps) *auth.User {
	ctx := context.Background()

	var user *auth.User
	if d.Cfg.DisableAuth {
		user = d.disableAuthUser(ctx)
	} else {
		user, _ = d.Auth.UserFromHeader(ctx, sock.Header().Get("Cookie"))
	}

	registerEvents(sock, d, user)

	if user != nil {
		sock.SetUserID(user.ID)
	}
	return user
}

// pushInitial 是连接建立后的首屏推送，严格对齐官方 io.on("connection") 的顺序。
func pushInitial(sock *socketio.Socket, d *Deps, user *auth.User) {
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

	if user != nil {
		sock.Emit("session", user.Username)
		afterLogin(sock, d, user)
	} else if sock.UserID() == "" {
		// 首屏推送在 goroutine 里，期间客户端可能已经自己发了 login。
		// 那种情况下再推 loginRequired 会让已登录的前端退回登录页。
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
	sock.Emit("proxyList", buildProxyList(ctx, d))
	sock.Emit("dockerHostList", buildDockerHostList(ctx, d))
	sock.Emit("remoteBrowserList", buildRemoteBrowserList(ctx, d))

	// 心跳历史：官方在 dashboardRendered 后延迟发送，这里直接发，
	// 前端同样能处理，且省掉一次往返。
	sendHeartbeatLists(ctx, sock, d, monitorList)

	// 看板可用率：官方 sendDeferredLists 紧跟 heartbeatLists 推 stats。
	// 前端 socket.js 的 socket.on("stats") 用它填 uptimeList —— 列表左侧
	// 的可用率胶囊靠它显示 "100%"，发空对象就全变 "NA"。
	sock.Emit("stats", buildStats(ctx, d, monitorList))

	// 首次使用：官方 afterLogin 询问浏览器时区（initServerTimezone 未初始化时）
	if _, ok, _ := d.Set.Raw(ctx, "initServerTimezone"); !ok {
		logx.Debugf("handler", "emit initServerTimezone")
		sock.Emit("initServerTimezone")
	}
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
	sock.On("addMonitorTag", wrap(addMonitorTag))
	sock.On("editMonitorTag", wrap(editMonitorTag))
	sock.On("deleteMonitorTag", wrap(deleteMonitorTag))

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

	// —— 代理（对齐官方 proxy-socket-handler.js）——
	sock.On("addProxy", wrap(addProxy))
	sock.On("deleteProxy", wrap(deleteProxy))

	// —— 远程浏览器（对齐官方 remote-browser-socket-handler.js）——
	sock.On("addRemoteBrowser", wrap(addRemoteBrowser))
	sock.On("deleteRemoteBrowser", wrap(deleteRemoteBrowser))
	sock.On("testRemoteBrowser", wrap(testRemoteBrowser))

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

	// —— API Key 管理（对齐官方 api-key-socket-handler.js）——
	sock.On("addAPIKey", wrap(addAPIKey))
	sock.On("getAPIKeyList", wrap(getAPIKeyList))
	sock.On("deleteAPIKey", wrap(deleteAPIKey))

	// —— 域名到期（对齐官方 server.js:721 checkDomain）——
	sock.On("checkDomain", wrap(checkDomain))

	// —— 尚未实现的周边功能：返回明确的占位结果，避免前端弹错误提示 ——
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
