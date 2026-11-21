// Package notify 负责告警投递：把一次状态变化分发给该监控绑定的所有渠道。
package notify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// Provider 是一个通知渠道实现。
// 返回值是展示给用户的文案（官方约定成功时返回 "Sent Successfully."）。
type Provider interface {
	Type() string
	Send(ctx context.Context, cfg map[string]any, msg string) (string, error)
}

// Message 是一次投递的完整上下文，对应官方的 monitorJSON / heartbeatJSON。
//
// 大多数渠道只关心拼好的文本（Msg），但企业微信要按 UP/DOWN 改标题、
// PagerDuty 要按状态决定 trigger 还是 resolve，这些都需要结构化信息。
type Message struct {
	// Monitor 为 nil 表示这是"测试"按钮触发的投递
	Monitor *model.Monitor
	Beat    model.Beat
	// Msg 是已拼好的告警文案，形如 "[站点] [✅ Up] 200 - OK"
	Msg string
	// Test 表示测试消息，渠道应当走"无监控"的简化模板
	Test bool
}

// Status 返回心跳状态；测试投递时返回 -1。
//
// 这里不能返回 0：model.StatusDown 就是 0，测试消息会被误判成"服务挂了"，
// 于是企业微信/PagerDuty/Teams 会把一条测试消息渲染成 Down 告警。
const statusNone = -1

func (m Message) Status() int {
	if m.Monitor == nil {
		return statusNone
	}
	return m.Beat.Status
}

// MonitorName 返回监控名；测试投递时返回空串。
func (m Message) MonitorName() string {
	if m.Monitor == nil {
		return ""
	}
	return m.Monitor.Name
}

// ContextProvider 是需要结构化上下文才能生成消息体的渠道。
//
// 只实现 Send 的老渠道不受影响，引擎会自动回退。
type ContextProvider interface {
	Provider
	SendMessage(ctx context.Context, cfg map[string]any, m Message) (string, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Provider{}
)

// Register 注册渠道实现。
func Register(p Provider) {
	regMu.Lock()
	registry[p.Type()] = p
	regMu.Unlock()
}

// Get 取渠道实现。
func Get(typ string) (Provider, bool) {
	regMu.RLock()
	p, ok := registry[typ]
	regMu.RUnlock()
	return p, ok
}

// Types 返回已支持的渠道类型。
func Types() []string {
	regMu.RLock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	regMu.RUnlock()
	return out
}

// Engine 是投递引擎，实现 monitor.Notifier。
type Engine struct {
	notifications *store.NotificationStore
	settings      *store.Settings

	sendTimeout time.Duration
}

// NewEngine 构造。
func NewEngine(n *store.NotificationStore, s *store.Settings) *Engine {
	return &Engine{notifications: n, settings: s, sendTimeout: 20 * time.Second}
}

// Notify 在状态发生重要变化时投递告警。
func (e *Engine) Notify(ctx context.Context, m *model.Monitor, beat model.Beat, prevStatus int) {
	list, err := e.notifications.ForMonitor(ctx, m.ID)
	if err != nil {
		logx.Errorf("notify", "读取通知列表失败 monitor #%d: %v", m.ID, err)
		return
	}
	if len(list) == 0 {
		return
	}

	msg := buildMessage(m, beat)
	payload := Message{Monitor: m, Beat: beat, Msg: msg}

	// 各渠道并行发送，任何一个慢都不拖累其它
	var wg sync.WaitGroup
	for _, n := range list {
		wg.Add(1)
		go func(n *store.Notification) {
			defer wg.Done()
			if _, err := e.deliver(ctx, n, payload); err != nil {
				logx.Warnf("notify", "渠道 %s(%s) 发送失败: %v", n.Name, n.Type, err)
			}
		}(n)
	}
	wg.Wait()
}

// SendTest 用于前端"测试"按钮。
func (e *Engine) SendTest(ctx context.Context, n *store.Notification) (string, error) {
	return e.deliver(ctx, n, Message{Msg: n.Name + " Testing", Test: true})
}

// NotifyCertificate 对齐官方 Notification.send(config, msg)（证书到期提醒）：
// 给绑定该监控的所有渠道发一条纯文本，不携带心跳上下文。
// 返回是否至少有一个渠道发送成功（官方以 sent 标志记录发送历史）。
func (e *Engine) NotifyCertificate(ctx context.Context, monitorID int64, msg string) bool {
	list, err := e.notifications.ForMonitor(ctx, monitorID)
	if err != nil {
		logx.Errorf("notify", "读取通知列表失败 monitor #%d: %v", monitorID, err)
		return false
	}

	sent := false
	for _, n := range list {
		if _, err := e.deliver(ctx, n, Message{Msg: msg}); err != nil {
			logx.Warnf("notify", "证书提醒发送失败 %s(%s): %v", n.Name, n.Type, err)
			continue
		}
		sent = true
	}
	return sent
}

// HasNotifications 快速判断某监控是否绑定了通知渠道（证书提醒的 fail-fast 优化）。
func (e *Engine) HasNotifications(ctx context.Context, monitorID int64) bool {
	list, err := e.notifications.ForMonitor(ctx, monitorID)
	if err != nil {
		return false
	}
	return len(list) > 0
}

func (e *Engine) deliver(ctx context.Context, n *store.Notification, m Message) (string, error) {
	p, ok := Get(n.Type)
	if !ok {
		return "", fmt.Errorf("通知类型 %q 尚未实现", n.Type)
	}

	// 单个渠道的发送必须有上限，否则一个挂掉的 webhook 会卡住整轮通知
	sendCtx, cancel := context.WithTimeout(ctx, e.sendTimeout)
	defer cancel()

	var (
		res string
		err error
	)
	if cp, ok := p.(ContextProvider); ok {
		res, err = cp.SendMessage(sendCtx, n.Config, m)
	} else {
		res, err = p.Send(sendCtx, n.Config, m.Msg)
	}
	if err != nil {
		return "", err
	}
	if res == "" {
		res = "Sent Successfully."
	}
	return res, nil
}

// buildMessage 生成与官方一致的告警文案。
//
//	[My Site] [✅ Up] 200 - OK
//	[My Site] [🔴 Down] ECONNREFUSED
func buildMessage(m *model.Monitor, beat model.Beat) string {
	text := "🔴 Down"
	if beat.Status == model.StatusUp {
		text = "✅ Up"
	}
	msg := beat.Msg
	if msg == "" {
		msg = "N/A"
	}
	return fmt.Sprintf("[%s] [%s] %s", m.Name, text, msg)
}

// ---------------- 配置读取工具 ----------------

func cfgStr(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON 数字解析后是 float64，整数值去掉小数点
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func cfgBool(cfg map[string]any, key string) bool {
	v, ok := cfg[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	default:
		return false
	}
}

func cfgInt(cfg map[string]any, key string) int64 {
	v, ok := cfg[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		var n int64
		_, _ = fmt.Sscan(t, &n)
		return n
	default:
		return 0
	}
}
