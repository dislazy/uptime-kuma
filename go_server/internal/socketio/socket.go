package socketio

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/songbo/uptime-kuma/go_server/internal/logx"
)

// Handler 是一个 socket 事件处理器。
// 返回值会被自动包装成 ack 回给前端；返回 error 时回 `{ok:false,msg:...}`。
// 返回 nil 且事件带 ack 时，按官方约定回 `{ok:true}`。
type Handler func(args []json.RawMessage) (any, error)

// Socket 是一个已连接的客户端（默认命名空间）。
//
// 生命周期：收到 "40" CONNECT → 创建 → OnConnect → 若干事件 → 断开 → OnDisconnect。
type Socket struct {
	id   string
	sess *Session

	mu       sync.RWMutex
	handlers map[string]Handler
	data     map[string]any
	closed   bool
}

func newSocket(id string, sess *Session) *Socket {
	return &Socket{
		id:       id,
		sess:     sess,
		handlers: make(map[string]Handler, 64),
		data:     make(map[string]any, 4),
	}
}

// On 注册事件处理器。重复注册会覆盖。
func (s *Socket) On(event string, h Handler) {
	s.mu.Lock()
	s.handlers[event] = h
	s.mu.Unlock()
}

// ID 返回 socket id。
func (s *Socket) ID() string { return s.id }

// Emit 向客户端推送事件。
func (s *Socket) Emit(event string, args ...any) {
	pkt, err := encodeEvent(event, args)
	if err != nil {
		logx.Warnf("socketio", "序列化事件 %s 失败: %v", event, err)
		return
	}
	s.sess.push(pkt)
}

// EmitRaw 直接推送已编码好的包，用于批量拼接场景。
func (s *Socket) EmitRaw(pkt string) { s.sess.push(pkt) }

// Set 存放连接级数据（userID、userName 等）。
func (s *Socket) Set(key string, val any) {
	s.mu.Lock()
	s.data[key] = val
	s.mu.Unlock()
}

// Get 读取连接级数据。
func (s *Socket) Get(key string) (any, bool) {
	s.mu.RLock()
	v, ok := s.data[key]
	s.mu.RUnlock()
	return v, ok
}

// Header 返回握手时的 HTTP 头（鉴权需要读取 Cookie）。
func (s *Socket) Header() http.Header {
	s.sess.mu.Lock()
	defer s.sess.mu.Unlock()
	if s.sess.header == nil {
		return http.Header{}
	}
	return s.sess.header
}

// RemoteAddr 返回客户端地址。
func (s *Socket) RemoteAddr() string {
	s.sess.mu.Lock()
	defer s.sess.mu.Unlock()
	return s.sess.remoteAddr
}

// UserID 返回已登录用户 ID（better-auth 用字符串主键），未登录为空串。
func (s *Socket) UserID() string {
	v, ok := s.Get("userID")
	if !ok {
		return ""
	}
	if id, ok := v.(string); ok {
		return id
	}
	return ""
}

// SetUserID 标记该连接已登录。
func (s *Socket) SetUserID(id string) { s.Set("userID", id) }

// IsClosed 是否已断开。
func (s *Socket) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// Disconnect 主动断开连接。
func (s *Socket) Disconnect() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.sess.close()
}

// dispatch 执行一个事件包。
func (s *Socket) dispatch(p *Packet) {
	s.mu.RLock()
	h, ok := s.handlers[p.Event]
	s.mu.RUnlock()

	if !ok {
		logx.Debugf("socketio", "收到未注册事件: %s", p.Event)
		if p.HasAck {
			s.ack(p.AckID, map[string]any{"ok": false, "msg": "Unsupported event: " + p.Event})
		}
		return
	}

	// 事件可能在 handler 里阻塞（数据库查询、外部请求），
	// 一律放到 goroutine，避免堵住读循环。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("socketio", "事件 %s 处理 panic: %v", p.Event, r)
				if p.HasAck {
					s.ack(p.AckID, map[string]any{"ok": false, "msg": fmt.Sprint(r)})
				}
			}
		}()

		res, err := h(p.Args)
		if !p.HasAck {
			return
		}
		if err != nil {
			s.ack(p.AckID, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		if res == nil {
			res = map[string]any{"ok": true}
		}
		s.ack(p.AckID, res)
	}()
}

func (s *Socket) ack(id int64, data any) {
	pkt, err := encodeAck(id, data)
	if err != nil {
		logx.Warnf("socketio", "序列化 ack 失败: %v", err)
		return
	}
	s.sess.push(pkt)
}
