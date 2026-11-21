package handlers

import (
	"context"
	"errors"
	"strings"

	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
)

// Remote Browser 管理。
//
// 前端 components/settings/RemoteBrowsers.vue + RemoteBrowserDialog.vue 用
// remoteBrowserList / addRemoteBrowser / deleteRemoteBrowser / testRemoteBrowser
// 四个事件，官方在 server/socket-handlers/remote-browser-socket-handler.js。

// addRemoteBrowser 新建或更新一个远程浏览器。
//
// 官方参数：(remoteBrowser{name,url}, remoteBrowserID, callback)。
func addRemoteBrowser(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}
	var id int64
	_ = c.OptArg(1, &id)

	name := strings.TrimSpace(asString(payload["name"]))
	url := strings.TrimSpace(asString(payload["url"]))

	// 官方没做空值校验，但空 URL 的远程浏览器永远连不上，早点报错更友好
	if url == "" {
		return nil, errors.New("Remote browser URL is required")
	}

	newID, err := c.Mon.SaveRemoteBrowser(c.Ctx, id, c.RequireUserID(), name, url)
	if err != nil {
		return nil, err
	}

	c.sendRemoteBrowserList()
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "id": newID}, nil
}

// deleteRemoteBrowser 删除一个远程浏览器，并解除监控引用。
func deleteRemoteBrowser(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Mon.DeleteRemoteBrowser(c.Ctx, id); err != nil {
		return nil, err
	}
	c.sendRemoteBrowserList()

	// 官方删除后靠下次探测时 RemoteBrowser.get 抛错兜底（级联置空在 store 层已做），
	// 这里刷新内存中的监控副本，让引用立即消失。
	if err := c.Mgr.Reload(c.Ctx); err != nil {
		logx.Warnf("remote-browser", "重载监控失败: %v", err)
	}

	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// testRemoteBrowser 实测端点连通性（官方 chromium.connect + version + close）。
func testRemoteBrowser(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}
	url := strings.TrimSpace(asString(payload["url"]))
	if url == "" {
		return nil, errors.New("Remote browser URL is required")
	}

	if err := checkers.TestRemoteBrowser(c.Ctx, url); err != nil {
		logx.Warnf("remote-browser", "测试远程浏览器失败: %v", err)
		return map[string]any{"ok": false, "msg": err.Error()}, nil
	}
	logx.Infof("remote-browser", "测试远程浏览器成功: %s", url)
	return map[string]any{"ok": true, "msg": "Connected Successfully."}, nil
}

// sendRemoteBrowserList 把整张 remote_browser 表推给当前连接。
func (c *Ctx) sendRemoteBrowserList() {
	c.Socket.Emit("remoteBrowserList", buildRemoteBrowserList(c.Ctx, c.Deps))
}

// buildRemoteBrowserList 组装 remoteBrowserList 数组（官方 bean.toJSON 字段名）。
func buildRemoteBrowserList(ctx context.Context, d *Deps) []map[string]any {
	list, err := d.Mon.RemoteBrowsers(ctx)
	if err != nil {
		logx.Warnf("remote-browser", "读取远程浏览器列表失败: %v", err)
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, rb := range list {
		out = append(out, rb.JSON())
	}
	return out
}
