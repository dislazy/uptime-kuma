// Package checkers 实现各种监控类型的探测逻辑。
//
// 每个检查器只负责"探一次并给出结论"，不关心调度、重试、通知——
// 那些属于 monitor 包的职责。这样新增监控类型只需要写一个文件 + 一行注册。
package checkers

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// Result 是一次探测的结果。
type Result struct {
	// Status 取值 model.StatusUp / StatusDown / StatusPending。
	Status int
	// Msg 展示给用户的说明，前端会直接显示。
	Msg string
	// Ping 毫秒级响应时间，nil 表示不适用（如 push 类型）。
	Ping *int64
	// DNSResult 仅 dns 类型使用，会写回 monitor.dns_last_result。
	DNSResult string
	// TLS https 探测握手成功后非 nil；由 manager 采集证书信息
	// （官方 readTlsInfo → handleTlsInfo 链路）。非 https 类型恒为 nil。
	TLS *tls.ConnectionState
}

// Checker 是监控类型探测器的接口。
type Checker interface {
	// Check 执行一次探测。ctx 已带超时，实现里直接复用即可。
	Check(ctx context.Context, m *model.Monitor) Result
	// Type 返回注册名，需与 monitor.type 一致。
	Type() string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Checker{}
)

// Register 注册检查器。应在 init 阶段调用。
func Register(c Checker) {
	registryMu.Lock()
	registry[c.Type()] = c
	registryMu.Unlock()
}

// Get 按类型取检查器。
func Get(typ string) (Checker, bool) {
	registryMu.RLock()
	c, ok := registry[typ]
	registryMu.RUnlock()
	return c, ok
}

// Types 返回已支持的类型列表（有序），用于日志与自检。
func Types() []string {
	registryMu.RLock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	registryMu.RUnlock()
	sort.Strings(out)
	return out
}

// Timeout 计算本次探测的超时时间。
//
// 官方规则：监控设置了 timeout 就用它，否则用 interval 的一定比例，
// 但不超过 60 秒，避免 20 秒间隔的监控卡 5 分钟。
func Timeout(m *model.Monitor) time.Duration {
	if d := m.Timeout(); d > 0 {
		return d
	}
	d := time.Duration(m.Interval) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// Up 构造成功结果。
func Up(msg string, pingMS int64) Result {
	return Result{Status: model.StatusUp, Msg: msg, Ping: &pingMS}
}

// Down 构造失败结果。
func Down(format string, args ...any) Result {
	return Result{Status: model.StatusDown, Msg: fmt.Sprintf(format, args...)}
}

// Pending 构造"暂不确定"结果。
func Pending(msg string) Result {
	return Result{Status: model.StatusPending, Msg: msg}
}
