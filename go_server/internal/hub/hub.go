// Package hub 管理所有已连接的 socket，并负责向它们广播。
package hub

import (
	"sync"

	"github.com/songbo/uptime-kuma/go_server/internal/socketio"
)

// Hub 是连接注册中心。
type Hub struct {
	mu    sync.RWMutex
	socks map[*socketio.Socket]struct{}
}

// New 构造。
func New() *Hub {
	return &Hub{socks: make(map[*socketio.Socket]struct{})}
}

// Add 注册连接。
func (h *Hub) Add(s *socketio.Socket) {
	h.mu.Lock()
	h.socks[s] = struct{}{}
	h.mu.Unlock()
}

// Remove 注销连接。
func (h *Hub) Remove(s *socketio.Socket) {
	h.mu.Lock()
	delete(h.socks, s)
	h.mu.Unlock()
}

// Count 返回在线连接数。
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.socks)
}

// Broadcast 向所有连接推送事件。args 会被序列化成 JSON 数组的后续元素。
func (h *Hub) Broadcast(event string, args ...any) {
	h.mu.RLock()
	targets := make([]*socketio.Socket, 0, len(h.socks))
	for s := range h.socks {
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	for _, s := range targets {
		s.Emit(event, args...)
	}
}

// BroadcastToUser 只推送给指定用户的连接。
// Uptime Kuma 当前只支持单用户，保留该能力是为了以后多用户扩展。
func (h *Hub) BroadcastToUser(userID string, event string, args ...any) {
	h.mu.RLock()
	targets := make([]*socketio.Socket, 0, len(h.socks))
	for s := range h.socks {
		if userID == "" || s.UserID() == userID {
			targets = append(targets, s)
		}
	}
	h.mu.RUnlock()

	for _, s := range targets {
		s.Emit(event, args...)
	}
}

// Each 遍历所有连接，回调里不要做耗时操作。
func (h *Hub) Each(fn func(*socketio.Socket)) {
	h.mu.RLock()
	targets := make([]*socketio.Socket, 0, len(h.socks))
	for s := range h.socks {
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	for _, s := range targets {
		fn(s)
	}
}
