package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/auth"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/notify"
	"github.com/dislazy/uptime-kuma/go_server/internal/socketio"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// ---------------- Web Push ----------------

// getWebpushVapidPublicKey 返回浏览器订阅所需的 VAPID 公钥。
// 前端新建 Webpush 通知时会先调它，再调 pushManager.subscribe。
// 首次调用时如果没有密钥现生成一对。
func getWebpushVapidPublicKey(c *Ctx) (any, error) {
	pub, err := notify.EnsureVAPIDKeys(c.Ctx, c.Set)
	if err != nil {
		return map[string]any{"ok": false, "msg": err.Error()}, nil
	}
	return map[string]any{"ok": true, "msg": pub}, nil
}

// ---------------- 鉴权 ----------------

// needSetup 前端用来判断是否跳初始化页。
func needSetup(c *Ctx) (any, error) {
	has, err := c.Auth.HasUser(c.Ctx)
	if err != nil {
		return nil, err
	}
	return !has && !c.Cfg.DisableAuth, nil
}

// login 兼容旧版 socket 登录（v3 前端已改用 better-auth HTTP 接口）。
func login(c *Ctx) (any, error) {
	var (
		username string
		password string
		token    string
	)
	_ = c.OptArg(0, &username)
	_ = c.OptArg(1, &password)
	_ = c.OptArg(2, &token)

	if token != "" {
		user, err := c.Auth.UserByToken(c.Ctx, token)
		if err != nil || user == nil {
			return nil, fmt.Errorf("无效的登录令牌")
		}
		c.Socket.SetUserID(user.ID)
		c.Socket.Emit("session", user.Username)
		afterLogin(c.Socket, c.Deps, user)
		return map[string]any{"ok": true, "token": token}, nil
	}

	if username == "" || password == "" {
		return nil, fmt.Errorf("用户名或密码不能为空")
	}

	user, cookieValue, err := c.Auth.Login(c.Ctx, username, password, c.Socket.RemoteAddr(), "", true)
	if err != nil {
		return nil, err
	}

	// socket 场景没有 http.ResponseWriter，直接把会话值回给前端保存
	c.Socket.SetUserID(user.ID)
	c.Socket.Emit("session", user.Username)
	afterLogin(c.Socket, c.Deps, user)

	return map[string]any{"ok": true, "token": cookieValue}, nil
}

// loginByToken 用 Cookie 值换会话。
func loginByToken(c *Ctx) (any, error) {
	var token string
	if err := c.Arg(0, &token); err != nil {
		return nil, err
	}

	user, err := c.Auth.UserFromHeader(c.Ctx, auth.CookieName+"="+token)
	if err != nil || user == nil {
		return nil, fmt.Errorf("无效的登录令牌")
	}

	c.Socket.SetUserID(user.ID)
	c.Socket.Emit("session", user.Username)
	afterLogin(c.Socket, c.Deps, user)
	return map[string]any{"ok": true}, nil
}

// disableAuthUser 在关闭鉴权时返回的虚拟用户。
func (d *Deps) disableAuthUser(ctx context.Context) *auth.User {
	row, err := d.DB.QueryOne(ctx, "SELECT id, username, name FROM better_auth_user LIMIT 1")
	if err == nil && row != nil {
		return &auth.User{ID: row.Str("id"), Username: row.Str("username"), Name: row.Str("name")}
	}
	return &auth.User{ID: "disable-auth", Username: "admin", Name: "admin"}
}

// ---------------- 通知 ----------------

// addNotification 新增/修改通知渠道。
func addNotification(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	var id int64
	_ = c.OptArg(1, &id)

	name := asString(payload["name"])
	typ := asString(payload["type"])
	if name == "" || typ == "" {
		return nil, fmt.Errorf("通知名称与类型不能为空")
	}

	// 官方把整个对象序列化进 config，这里保持一致
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	values := map[string]any{
		"name":       name,
		"active":     true,
		"is_default": asBool(payload["isDefault"]),
		"config":     string(encoded),
	}

	if id > 0 {
		if err := c.Notif.Update(c.Ctx, id, values); err != nil {
			return nil, err
		}
	} else {
		id, err = c.Notif.Create(c.Ctx, values)
		if err != nil {
			return nil, err
		}
	}

	broadcastNotificationList(c.Deps)
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "id": id}, nil
}

// deleteNotification 删除通知渠道。
func deleteNotification(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Notif.Delete(c.Ctx, id); err != nil {
		return nil, err
	}
	broadcastNotificationList(c.Deps)
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// testNotification 发送测试消息。
func testNotification(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	n := &store.Notification{
		Name:   asString(payload["name"]),
		Type:   asString(payload["type"]),
		Config: payload,
	}
	if n.Name == "" {
		n.Name = n.Type
	}

	msg, err := c.Eng.SendTest(c.Ctx, n)
	if err != nil {
		logx.Warnf("handler", "测试通知失败: %v", err)
		return map[string]any{"ok": false, "msg": err.Error()}, nil
	}
	return map[string]any{"ok": true, "msg": msg}, nil
}

// checkApprise 检查 apprise 是否可用。Go 版未内置 apprise 命令行，返回 false。
func checkApprise(c *Ctx) (any, error) {
	return false, nil
}

func broadcastNotificationList(d *Deps) {
	list, err := d.Notif.List(context.Background())
	if err != nil {
		return
	}
	flat := make([]map[string]any, 0, len(list))
	for _, n := range list {
		flat = append(flat, n.Flatten())
	}
	d.Hub.Broadcast("notificationList", flat)
}

// ---------------- 标签 ----------------

func getTags(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}
	list, err := c.Tag.List(c.Ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "tags": list}, nil
}

func addTag(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	id, err := c.Tag.Create(c.Ctx, asString(payload["name"]), asString(payload["color"]))
	if err != nil {
		return nil, err
	}
	broadcastTags(c.Deps)
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "id": id}, nil
}

func editTag(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	id := int64(numberOf(payload["id"]))
	if err := c.Tag.Update(c.Ctx, id, asString(payload["name"]), asString(payload["color"])); err != nil {
		return nil, err
	}
	broadcastTags(c.Deps)
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true}, nil
}

func deleteTag(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Tag.Delete(c.Ctx, id); err != nil {
		return nil, err
	}
	broadcastTags(c.Deps)
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// addMonitorTag 对应官方 server.js addMonitorTag（server.js:1012）：
// 给已有监控加标签，回包 successAdded。
func addMonitorTag(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var tagID, monitorID int64
	if err := c.Arg(0, &tagID); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &monitorID); err != nil {
		return nil, err
	}
	var value string
	_ = c.OptArg(2, &value)

	if _, err := c.DB.Exec(c.Ctx,
		"INSERT INTO monitor_tag (tag_id, monitor_id, value) VALUES (?, ?, ?)",
		tagID, monitorID, value); err != nil {
		return nil, err
	}
	// 官方随后增量推送该监控；Go 用全量 monitorList 广播等价覆盖
	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "successAdded", "msgi18n": true}, nil
}

// editMonitorTag 对应官方 server.js editMonitorTag（server.js:1034）。
func editMonitorTag(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var tagID, monitorID int64
	if err := c.Arg(0, &tagID); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &monitorID); err != nil {
		return nil, err
	}
	var value string
	if err := c.Arg(2, &value); err != nil {
		return nil, err
	}

	if _, err := c.DB.Exec(c.Ctx,
		"UPDATE monitor_tag SET value = ? WHERE tag_id = ? AND monitor_id = ?",
		value, tagID, monitorID); err != nil {
		return nil, err
	}
	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "successEdited", "msgi18n": true}, nil
}

// deleteMonitorTag 对应官方 server.js deleteMonitorTag（server.js:1056）：
// 前端 TagEditDialog / TagsManager 在用，value 参与匹配。
func deleteMonitorTag(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var tagID, monitorID int64
	if err := c.Arg(0, &tagID); err != nil {
		return nil, err
	}
	if err := c.Arg(1, &monitorID); err != nil {
		return nil, err
	}
	var value string
	_ = c.OptArg(2, &value)

	if _, err := c.DB.Exec(c.Ctx,
		"DELETE FROM monitor_tag WHERE tag_id = ? AND monitor_id = ? AND value = ?",
		tagID, monitorID, value); err != nil {
		return nil, err
	}
	broadcastMonitorList(c.Deps)
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

func broadcastTags(d *Deps) {
	list, err := d.Tag.List(context.Background())
	if err != nil {
		return
	}
	d.Hub.Broadcast("tags", list)
}

// ---------------- 设置与系统 ----------------

// getSettings 返回全部设置。
func getSettings(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}
	all, err := c.Set.All(c.Ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": all}, nil
}

// setSettings 批量写入设置。入参 (data, currentPassword)。
//
// 官方签名是 socket.on("setSettings", (data, currentPassword, callback))，
// 且固定写进 type="general" 的分组。前端 Settings.vue 就是这么调的，
// 参数顺序写反会导致整页设置保存失败（且只返回一句莫名其妙的解析错误）。
func setSettings(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var data map[string]any
	if err := c.Arg(0, &data); err != nil {
		return nil, err
	}
	var currentPassword string
	_ = c.OptArg(1, &currentPassword)

	currentDisabledAuth := c.Set.GetBool(c.Ctx, "disableAuth", false)
	wantDisableAuth := asBool(data["disableAuth"])

	// 安全开关：从「已启用鉴权」改成「禁用鉴权」必须再验一次密码
	if !currentDisabledAuth && wantDisableAuth {
		uid := c.RequireUserID()
		if err := c.Auth.CheckPassword(c.Ctx, uid, currentPassword); err != nil {
			return nil, errors.New("Incorrect password")
		}
	}

	// 从「禁用鉴权」改成「启用鉴权」：把其它客户端踢下线（官方 GHSA-23q2-5gf8-gjpp）
	if currentDisabledAuth && !wantDisableAuth {
		uid := c.RequireUserID()
		current := c.Socket
		c.Hub.Each(func(s *socketio.Socket) {
			if s != current && s.UserID() == uid {
				s.Emit("refresh")
				s.Disconnect()
			}
		})
	}

	changed, err := c.Set.SetMany(c.Ctx, "general", data)
	if err != nil {
		return nil, err
	}
	logx.Debugf("handler", "设置已更新: %v", changed)

	// 时间区改动要立刻生效到全局（官方 server.setTimezone）
	if tz, ok := data["serverTimezone"]; ok {
		if s, _ := tz.(string); s != "" {
			if loc, err := time.LoadLocation(s); err == nil {
				time.Local = loc
			}
		}
	}

	// 官方保存成功后会重发 info 与维护窗口列表（entryPage、时区都影响 info）
	emitInfo(c.Socket, c.Deps, true)
	c.Socket.Emit("maintenanceList", c.MaintMgr.JSONList())

	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true}, nil
}

// getDatabaseSize 返回数据库文件大小（字节）。
func getDatabaseSize(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	// 官方 Database.getSize() 对非 SQLite 直接返回 0，这里保持一致：
	// MySQL 的库大小不该按"数据文件"算，前端也只拿它显示个数值。
	if c.DB.IsMySQL() {
		return int64(0), nil
	}

	st, err := os.Stat(c.DB.Path())
	if err != nil {
		return int64(0), nil
	}
	return st.Size(), nil
}

// shrinkDatabase 压缩数据库。
//
// 官方 Database.shrink() 只对 SQLite 执行 VACUUM，MariaDB 下是空操作并返回 ok；
// 无条件 VACUUM 在 MySQL 上是语法错误（真库实测会直接报 1064）。
func shrinkDatabase(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}
	if !c.DB.IsMySQL() {
		if _, err := c.DB.Exec(c.Ctx, "VACUUM"); err != nil {
			return map[string]any{"ok": false, "msg": err.Error()}, nil
		}
	}
	return map[string]any{"ok": true, "msg": "Database shrunk successfully."}, nil
}

// clearEvents 清空重要心跳。
func clearEvents(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if _, err := c.DB.Exec(c.Ctx, "DELETE FROM heartbeat WHERE monitor_id = ? AND important = 1", id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "msg": "Cleared Events."}, nil
}

// clearHeartbeats 清空某监控全部心跳。
func clearHeartbeats(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Beat.Clear(c.Ctx, id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "msg": "Cleared Heartbeats."}, nil
}

// clearStatistics 清空某监控的统计聚合。
func clearStatistics(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Stat.ClearForMonitor(c.Ctx, id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "msg": "Cleared Statistics."}, nil
}

// getPushExample 返回推送地址示例。
func getPushExample(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	m, err := c.Mon.Get(c.Ctx, id)
	if err != nil || m == nil {
		return nil, fmt.Errorf("monitor not found")
	}
	code := `curl -m 60 "https://example.com/api/push/key?status=up&msg=OK&ping="`
	return map[string]any{
		"code": code,
		"url":  fmt.Sprintf("%s/api/push/%s", c.Cfg.BaseURL(), m.PushToken),
	}, nil
}

// initServerTimezone 首次启动时用浏览器时区初始化服务器时区。
func initServerTimezone(c *Ctx) (any, error) {
	var tz string
	_ = c.OptArg(0, &tz)
	if strings.TrimSpace(tz) == "" {
		return nil, nil
	}

	if _, ok, _ := c.Set.Raw(c.Ctx, "initServerTimezone"); ok {
		return nil, nil
	}

	_ = c.Set.Set(c.Ctx, "serverTimezone", tz, "general")
	_ = c.Set.Set(c.Ctx, "initServerTimezone", true, "general")
	return map[string]any{"ok": true}, nil
}

// disconnectOtherSocketClients 踢掉同用户的其它连接。
// disconnectOtherSocketClients 对齐官方 disconnectAllSocketClients：
// 先推 refresh（前端 location.reload），再断开其它会话。
func disconnectOtherSocketClients(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	uid := c.RequireUserID()
	current := c.Socket
	c.Hub.Each(func(s *socketio.Socket) {
		if s != current && s.UserID() == uid {
			s.Emit("refresh")
			s.Disconnect()
		}
	})
	return map[string]any{"ok": true}, nil
}

// ---------------- 域名到期（RDAP） ----------------

// checkDomain 检查域名到期支持情况，对齐官方 server.js:721：
// checkLogin → DomainExpiry.checkSupport → {ok, domain, tld}；
// 失败回 {ok:false, msg:i18n key, msgi18n:true, meta}。
func checkDomain(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var partial map[string]any
	_ = c.OptArg(0, &partial)

	typ := asString(partial["type"])
	targetURL := asString(partial["url"])
	hostname := asString(partial["hostname"])
	grpcURL := asString(partial["grpcUrl"])

	domain, tld, serr := c.DomainSvc.CheckSupport(c.Ctx, typ, targetURL, hostname, grpcURL)
	if serr != nil {
		meta := serr.Meta
		if meta == nil {
			// 官方：e.meta ?? {} 兜底空对象
			meta = map[string]any{}
		}
		return map[string]any{"ok": false, "msg": serr.Msg, "msgi18n": true, "meta": meta}, nil
	}
	return map[string]any{"ok": true, "domain": domain, "tld": tld}, nil
}

// ---------------- 周边功能占位 ----------------
//
// 这些能力依赖外部组件（pm2、Chromium），P1 不实现，
// 但必须返回结构合法的结果，否则前端会弹错误提示甚至中断保存流程。

// getGameList 返回 gamedig 支持的协议列表，当前为空。
func getGameList(c *Ctx) (any, error) {
	return map[string]any{"ok": true, "data": []any{}}, nil
}

// getPM2ProcessList 返回 pm2 进程列表，当前为空。
func getPM2ProcessList(c *Ctx) (any, error) {
	return map[string]any{"ok": true, "data": map[string]any{}}, nil
}

// testChrome 测试内置浏览器可用性。
func testChrome(c *Ctx) (any, error) {
	return map[string]any{"ok": false, "msg": "Real-browser monitor is not supported in this build."}, nil
}
