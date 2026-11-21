package handlers

import (
	"context"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// Proxy 管理。
//
// 前端 components/settings/Proxies.vue + ProxyDialog.vue 用
// proxyList / addProxy / deleteProxy 三个事件，官方在
// server/socket-handlers/proxy-socket-handler.js 里实现。

// addProxy 新建或更新一个代理。
//
// 官方参数：(proxy{protocol,host,port,auth,username,password,active,default,applyExisting},
// proxyID, callback)。proxyID 存在即更新。
func addProxy(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}
	var id int64
	_ = c.OptArg(1, &id)

	p := store.Proxy{
		Protocol:      asString(payload["protocol"]),
		Host:          asString(payload["host"]),
		Port:          int64(numberOf(payload["port"])),
		Auth:          asBool(payload["auth"]),
		Username:      asString(payload["username"]),
		Password:      asString(payload["password"]),
		Active:        asBool(payload["active"]),
		Default:       asBool(payload["default"]),
		ApplyExisting: asBool(payload["applyExisting"]),
	}

	newID, err := c.Mon.SaveProxy(c.Ctx, id, numericUserID(c), p)
	if err != nil {
		return nil, err
	}

	c.sendProxyList()

	// 官方：勾选 applyExisting 时 reloadProxy + sendMonitorList
	// （applyExisting 已把全库 monitor.proxy_id 指到本代理，需刷新内存并通知前端）
	if p.ApplyExisting {
		if err := c.Mgr.Reload(c.Ctx); err != nil {
			logx.Warnf("proxy", "重载监控失败: %v", err)
		}
		broadcastMonitorList(c.Deps)
	}

	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "id": newID}, nil
}

// deleteProxy 删除代理（级联置空 monitor.proxy_id）。
func deleteProxy(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Mon.DeleteProxy(c.Ctx, id); err != nil {
		return nil, err
	}

	c.sendProxyList()

	// 官方删除后 reloadProxy：刷新内存中监控对已删代理的引用
	if err := c.Mgr.Reload(c.Ctx); err != nil {
		logx.Warnf("proxy", "重载监控失败: %v", err)
	}

	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// sendProxyList 把整张 proxy 表推给当前连接。
func (c *Ctx) sendProxyList() {
	c.Socket.Emit("proxyList", buildProxyList(c.Ctx, c.Deps))
}

// buildProxyList 组装 proxyList 数组（官方 bean.toJSON 字段名，见 store.Proxy.JSON）。
func buildProxyList(ctx context.Context, d *Deps) []map[string]any {
	list, err := d.Mon.Proxies(ctx)
	if err != nil {
		logx.Warnf("proxy", "读取代理列表失败: %v", err)
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, p.JSON())
	}
	return out
}
