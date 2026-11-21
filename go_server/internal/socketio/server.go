package socketio

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
)

// Server 是 Socket.IO 服务端，直接实现 http.Handler。
type Server struct {
	mu       sync.RWMutex
	sessions map[string]*Session

	// PingInterval / PingTimeout 与官方 socket.io 默认值一致。
	PingInterval time.Duration
	PingTimeout  time.Duration
	MaxPayload   int

	// OnConnect 在客户端完成 Socket.IO 握手（收到 "40"）后触发。
	OnConnect func(*Socket)
	// OnDisconnect 在连接断开后触发。
	OnDisconnect func(*Socket)
	// CheckOrigin 校验 WebSocket 来源，返回 false 将拒绝握手。
	CheckOrigin func(*http.Request) bool
}

// NewServer 构造服务端。
func NewServer() *Server {
	return &Server{
		sessions:     make(map[string]*Session),
		PingInterval: 25 * time.Second,
		PingTimeout:  20 * time.Second,
		MaxPayload:   1_000_000,
		CheckOrigin:  defaultOriginCheck,
	}
}

// ServeHTTP 处理 /socket.io/ 下的全部请求。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	transport := q.Get("transport")
	sid := q.Get("sid")

	switch transport {
	case "polling":
		s.servePolling(w, r, sid)
	case "websocket":
		s.serveWebsocket(w, r, sid)
	default:
		http.Error(w, `{"code":0,"message":"Transport unknown"}`, http.StatusBadRequest)
	}
}

// upgradesFor 返回当前传输下可升级到的传输列表。
// 已经从 websocket 建立的会话没有更高传输，返回空数组。
func (s *Server) upgradesFor(transport string) []string {
	if transport == "polling" {
		return []string{"websocket"}
	}
	return []string{}
}

// ---------------- polling ----------------

func (s *Server) servePolling(w http.ResponseWriter, r *http.Request, sid string) {
	switch r.Method {
	case http.MethodGet:
		if sid == "" {
			sess := s.newSession(r)
			_ = writeText(w, encodeOpen(sess.sid, s.upgradesFor("polling"), int(s.PingInterval/time.Millisecond),
				int(s.PingTimeout/time.Millisecond), s.MaxPayload))
			return
		}

		sess := s.get(sid)
		if sess == nil {
			http.Error(w, `{"code":1,"message":"Session ID unknown"}`, http.StatusBadRequest)
			return
		}
		sess.pollTo(w, r)

	case http.MethodPost:
		sess := s.get(sid)
		if sess == nil {
			http.Error(w, `{"code":1,"message":"Session ID unknown"}`, http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.MaxPayload)+1024))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if len(body) > s.MaxPayload {
			http.Error(w, `{"code":3,"message":"Payload too large"}`, http.StatusRequestEntityTooLarge)
			return
		}

		for _, pkt := range strings.Split(string(body), recordSeparator) {
			sess.handleIncoming(pkt)
		}
		_ = writeText(w, "ok")

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------- websocket ----------------

func (s *Server) serveWebsocket(w http.ResponseWriter, r *http.Request, sid string) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin:     s.CheckOrigin,
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logx.Debugf("socketio", "WebSocket 升级失败: %v", err)
		return
	}

	// 情况一：客户端直接以 websocket 建立新会话（transports: ["websocket"]）
	if sid == "" {
		sess := s.newSession(r)
		sess.attachWS(conn)
		sess.push(encodeOpen(sess.sid, s.upgradesFor("websocket"), int(s.PingInterval/time.Millisecond),
			int(s.PingTimeout/time.Millisecond), s.MaxPayload))
		go sess.readLoop(conn)
		return
	}

	// 情况二：从 polling 升级上来（socket.io-client 默认行为）
	sess := s.get(sid)
	if sess == nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"code":1,"message":"Session ID unknown"}`))
		_ = conn.Close()
		return
	}

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				// 升级失败不影响原 polling 连接
				_ = conn.Close()
				return
			}
			frame := string(msg)

			switch {
			case frame == "2probe":
				_ = conn.WriteMessage(websocket.TextMessage, []byte("3probe"))
			case frame == "5":
				sess.attachWS(conn)
				go sess.readLoop(conn)
				return
			default:
				sess.handleIncoming(frame)
			}
		}
	}()
}

// ---------------- session 管理 ----------------

func (s *Server) newSession(r *http.Request) *Session {
	sess := &Session{
		sid:      newSID(),
		srv:      s,
		notify:   make(chan struct{}, 1),
		upgraded: make(chan struct{}),
		done:     make(chan struct{}),
		lastPong: time.Now(),
	}
	if r != nil {
		sess.header = r.Header.Clone()
		sess.remoteAddr = r.RemoteAddr
	}

	s.mu.Lock()
	s.sessions[sess.sid] = sess
	s.mu.Unlock()

	go sess.keepalive()
	return sess
}

func (s *Server) get(sid string) *Session {
	s.mu.RLock()
	sess := s.sessions[sid]
	s.mu.RUnlock()
	return sess
}

func (s *Server) remove(sid string) {
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
}

// Count 返回当前会话数，用于监控指标。
func (s *Server) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// Session 是一次 Engine.IO 会话，可能经历 polling → websocket 升级。
type Session struct {
	sid string
	srv *Server

	mu         sync.Mutex
	ws         *websocket.Conn
	out        []string
	closed     bool
	sock       *Socket
	lastPong   time.Time
	header     http.Header
	remoteAddr string

	notify   chan struct{}
	upgraded chan struct{}
	done     chan struct{}

	upgradedOnce sync.Once
	pollMu       sync.Mutex // 同一时刻只允许一个 polling GET 挂起
	writeMu      sync.Mutex // gorilla 连接不支持并发写
}

// 单个会话最多缓存的待发包数量，防止掉线的 polling 客户端撑爆内存。
const maxOutbox = 512

func (sess *Session) push(pkt string) {
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	if sess.ws != nil {
		conn := sess.ws
		sess.mu.Unlock()
		sess.writeWS(conn, pkt)
		return
	}
	if len(sess.out) >= maxOutbox {
		// 丢掉最旧的，保证新心跳不被卡死
		sess.out = sess.out[1:]
	}
	sess.out = append(sess.out, pkt)
	sess.mu.Unlock()

	select {
	case sess.notify <- struct{}{}:
	default:
	}
}

func (sess *Session) writeWS(conn *websocket.Conn, pkt string) {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(pkt)); err != nil {
		logx.Debugf("socketio", "写入 WebSocket 失败: %v", err)
	}
}

// pollTo 阻塞等待待发数据并直接写入响应。
//
// 关键点：包只有在「确实写进 socket」之后才从队列移除。客户端在长轮询
// 超时或网络抖动下断连时，写会失败，此时保留队列等它重连再取，
// 避免通知/心跳在这类瞬时故障里被静默丢掉。
func (sess *Session) pollTo(w http.ResponseWriter, r *http.Request) {
	sess.pollMu.Lock()
	defer sess.pollMu.Unlock()

	timer := time.NewTimer(sess.srv.PingInterval + sess.srv.PingTimeout)
	defer timer.Stop()

	for {
		sess.mu.Lock()
		if len(sess.out) > 0 {
			sent := len(sess.out)
			payload := strings.Join(sess.out, recordSeparator)
			sess.mu.Unlock()

			if err := writeText(w, payload); err != nil {
				logx.Debugf("socketio", "polling 写出失败，保留 %d 条待发: %v", sent, err)
				return
			}

			// 只移出这次真正发出去的那几条，写的过程中新入队的要保留
			sess.mu.Lock()
			if sent > len(sess.out) {
				sent = len(sess.out)
			}
			sess.out = sess.out[sent:]
			sess.mu.Unlock()
			return
		}
		closed := sess.closed
		sess.mu.Unlock()

		if closed {
			_ = writeText(w, "6")
			return
		}

		select {
		case <-sess.notify:
			// 有新包，回到循环顶部取
		case <-sess.upgraded:
			// 已升级到 websocket，让 polling 请求尽快结束
			_ = writeText(w, "6")
			return
		case <-timer.C:
			_ = writeText(w, "6")
			return
		case <-sess.done:
			_ = writeText(w, "6")
			return
		case <-r.Context().Done():
			// 客户端断开（刷新页面、网络抖动、超时）：马上释放这次长轮询，
			// 否则这个 GET 会白占 pollMu，直到 ping 超时才放行下一个请求
			return
		}
	}
}

func (sess *Session) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			sess.close()
			return
		}
		sess.handleIncoming(string(msg))
	}
}

func (sess *Session) attachWS(conn *websocket.Conn) {
	sess.mu.Lock()
	sess.ws = conn
	pending := sess.out
	sess.out = nil
	sess.mu.Unlock()

	for _, p := range pending {
		sess.writeWS(conn, p)
	}

	sess.upgradedOnce.Do(func() { close(sess.upgraded) })
	select {
	case sess.notify <- struct{}{}:
	default:
	}
}

func (sess *Session) handleIncoming(raw string) {
	if raw == "" {
		return
	}

	switch raw[0] {
	case enginePong:
		sess.touch()
	case enginePing:
		sess.push("3")
	case engineClose:
		sess.close()
	case engineMessage:
		p, err := parseSocketPacket(raw[1:])
		if err != nil {
			logx.Debugf("socketio", "解析包失败: %v", err)
			return
		}
		sess.handleSocketPacket(p)
	case engineUpgrade:
		// 升级确认，无需额外处理
	default:
		logx.Debugf("socketio", "忽略未知 engine 包: %q", raw)
	}
}

func (sess *Session) handleSocketPacket(p *Packet) {
	switch p.Type {
	case sioConnect:
		sess.mu.Lock()
		if sess.sock == nil {
			sess.sock = newSocket(sess.sid, sess)
		}
		sock := sess.sock
		sess.mu.Unlock()

		sess.push(encodeConnect(sess.sid))

		if sess.srv.OnConnect != nil {
			// 必须同步调用：客户端收到 "40" 之后可能立刻 emit 第一个事件，
			// 事件处理器若在异步回调里注册就会漏掉它（实测表现为
			// "Unsupported event: add"）。耗时较长的首屏推送由 handlers
			// 自己放进 goroutine，这里不要替它决定。
			sess.srv.OnConnect(sock)
		}

	case sioEvent:
		sess.mu.Lock()
		sock := sess.sock
		sess.mu.Unlock()
		if sock != nil {
			sock.dispatch(p)
		}

	case sioDisconnect:
		sess.close()

	case sioConnectError, sioAck:
		// 未使用
	}
}

func (sess *Session) keepalive() {
	ticker := time.NewTicker(sess.srv.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sess.mu.Lock()
			dead := time.Since(sess.lastPong) > sess.srv.PingInterval+sess.srv.PingTimeout
			closed := sess.closed
			sess.mu.Unlock()

			if closed {
				return
			}
			if dead {
				logx.Debugf("socketio", "会话 %s 心跳超时，关闭", sess.sid)
				sess.close()
				return
			}
			sess.push(string(enginePing))
		case <-sess.done:
			return
		}
	}
}

func (sess *Session) touch() {
	sess.mu.Lock()
	sess.lastPong = time.Now()
	sess.mu.Unlock()
}

func (sess *Session) close() {
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	sess.closed = true
	sock := sess.sock
	conn := sess.ws
	sess.mu.Unlock()

	close(sess.done)
	if conn != nil {
		_ = conn.Close()
	}
	sess.srv.remove(sess.sid)

	if sock != nil && sess.srv.OnDisconnect != nil {
		sess.srv.OnDisconnect(sock)
	}
}

// ---------------- 工具函数 ----------------

func writeText(w http.ResponseWriter, body string) error {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, err := io.WriteString(w, body)
	return err
}

// newSID 生成与 socket.io 风格一致的短 ID。
func newSID() string {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func defaultOriginCheck(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 非浏览器客户端（curl / 探针）没有 Origin
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// AllowAllOrigins 用于 UPTIME_KUMA_WS_ORIGIN_CHECK=bypass 场景。
func AllowAllOrigins(*http.Request) bool { return true }
