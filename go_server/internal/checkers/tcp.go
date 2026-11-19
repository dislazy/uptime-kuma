package checkers

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// TCPChecker 通过 TCP 连接判断端口是否可用。
// 对应前端的 "TCP Port" 类型（官方内部叫 port，这里统一用 tcp）。
type TCPChecker struct{}

// Type 实现 Checker。
func (TCPChecker) Type() string { return "tcp" }

func init() {
	c := TCPChecker{}
	Register(c)
	// 官方历史类型名是 port，两者等价
	Register(aliasChecker{base: c, name: "port"})
}

// Check 尝试建立 TCP 连接。
func (TCPChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	port := m.Port()
	if host == "" {
		return Down("Hostname is empty")
	}
	if port <= 0 {
		return Down("Port is invalid")
	}

	dialer := &net.Dialer{}
	start := time.Now()

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	ping := time.Since(start).Milliseconds()
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	_ = conn.Close()

	return Up(fmt.Sprintf("Connected to %s:%d", host, port), ping)
}

func cleanDialError(err error) string {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "Timeout"
	}
	return err.Error()
}
