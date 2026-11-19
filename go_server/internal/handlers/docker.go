package handlers

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/songbo/uptime-kuma/go_server/internal/checkers"
	"github.com/songbo/uptime-kuma/go_server/internal/logx"
)

// Docker Host 管理。
//
// 前端 components/settings/Docker.vue + DockerHostDialog.vue 用的是
// dockerHostList / addDockerHost / deleteDockerHost / testDockerHost 四个事件，
// 官方在 server/socket-handlers/docker-socket-handler.js 里实现。

// addDockerHost 新建或更新一台 Docker 主机。
//
// 官方参数：(dockerHost{dockerDaemon,dockerType,name}, dockerHostID, callback)
func addDockerHost(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}
	var id int64
	_ = c.OptArg(1, &id)

	daemon := strings.TrimSpace(asString(payload["dockerDaemon"]))
	typ := strings.TrimSpace(asString(payload["dockerType"]))
	name := strings.TrimSpace(asString(payload["name"]))

	// 官方没做严格校验，但空 daemon 的 Docker 主机永远连不上，早点报错更友好
	if daemon == "" {
		return nil, errors.New("Docker Daemon is required")
	}
	if typ == "" {
		typ = "socket"
	}

	saved, err := c.Mon.SaveDockerHost(c.Ctx, id, c.RequireUserID(), name, daemon, typ)
	if err != nil {
		return nil, err
	}

	c.sendDockerHostList()
	return map[string]any{"ok": true, "msg": "Saved.", "msgi18n": true, "id": saved}, nil
}

// deleteDockerHost 删除一台 Docker 主机，并解除监控引用。
func deleteDockerHost(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var id int64
	if err := c.Arg(0, &id); err != nil {
		return nil, err
	}

	if err := c.Mon.DeleteDockerHost(c.Ctx, id); err != nil {
		return nil, err
	}
	c.sendDockerHostList()
	return map[string]any{"ok": true, "msg": "successDeleted", "msgi18n": true}, nil
}

// testDockerHost 测试连接，返回容器数量。
func testDockerHost(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := c.Arg(0, &payload); err != nil {
		return nil, err
	}

	info := checkers.DockerHostInfo{
		Daemon: strings.TrimSpace(asString(payload["dockerDaemon"])),
		Type:   strings.TrimSpace(asString(payload["dockerType"])),
	}
	if info.Type == "" {
		info.Type = "socket"
	}

	amount, err := checkers.TestDockerHost(c.Ctx, info)
	if err != nil {
		logx.Warnf("docker", "测试 Docker 主机失败: %v", err)
		return map[string]any{"ok": false, "msg": err.Error()}, nil
	}

	msg := "Connected Successfully, but there are no containers?"
	if amount >= 1 {
		msg = "Connected Successfully. Amount of containers: " + strconv.Itoa(amount)
	}
	return map[string]any{"ok": true, "msg": msg}, nil
}

// sendDockerHostList 把整张 docker_host 表推给当前用户。
func (c *Ctx) sendDockerHostList() {
	c.Socket.Emit("dockerHostList", buildDockerHostList(c.Ctx, c.Deps))
}

// buildDockerHostList 组装 dockerHostList 的数组形态（官方 bean.toJSON() 字段名）。
func buildDockerHostList(ctx context.Context, d *Deps) []map[string]any {
	list, err := d.Mon.DockerHosts(ctx)
	if err != nil {
		logx.Warnf("docker", "读取 Docker 主机列表失败: %v", err)
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, h := range list {
		out = append(out, h.JSON())
	}
	return out
}
