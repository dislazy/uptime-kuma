package checkers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"github.com/gorilla/websocket"
)

// Playwright 浏览器服务器协议的极小客户端。
//
// 官方 real-browser 探测走 playwright-core 的 chromium.connect(remoteBrowser.url)，
// 与远程 Chromium 的通信是 Playwright 自有的 WS JSON 协议（非 CDP）。
// 这里按官方 1.5x 客户端的实测报文（tap 录制 launchServer 全流程）实现最小子集：
//
//	initialize → __create__(BrowserType/…/Browser/Playwright) → result
//	browser.newContext → __create__(BrowserContext/Page/Frame) → result{context}
//	context.newPage   → result{page}（Page 的 mainFrame 由 __create__ 先行下发）
//	frame.goto        → __create__(Response) → result{response}（status 在 Response initializer）
//	context.close
//
// 与官方一致：每次探测新建一条连接，探测完关闭（官方 getRemoteBrowser 也是
// 每次 check 都 connect，不缓存连接）。

// Raw 注入键：manager 在每次探测前把 remote_browser 解析成 URL 写进 monitor.Raw。
const RawRemoteBrowserURL = "_remote_browser_url"

// SetRemoteBrowser 把远程浏览器端点写进 monitor.Raw（仿 SetProxy 的注入模式）。
func SetRemoteBrowser(m *model.Monitor, url string) {
	if m == nil || m.Raw == nil {
		return
	}
	m.Raw[RawRemoteBrowserURL] = url
}

// ClearRemoteBrowser 清掉注入的端点（remote_browser 被删/置空时调用）。
func ClearRemoteBrowser(m *model.Monitor) {
	if m == nil || m.Raw == nil {
		return
	}
	delete(m.Raw, RawRemoteBrowserURL)
}

// RemoteBrowserURL 从 monitor.Raw 取出端点。
func RemoteBrowserURL(m *model.Monitor) string {
	if m == nil {
		return ""
	}
	return m.Get(RawRemoteBrowserURL)
}

// ---------------- 协议客户端 ----------------

// pwClient 是一条到 Playwright 浏览器服务器的 WS 连接。
type pwClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu        sync.Mutex
	nextID    int
	browser   string            // 预启动 Browser 的 guid
	version   string            // Browser.initializer.version
	frames    map[string]string // page guid → mainFrame guid
	responses map[string]pwResponse
	pending   map[string]chan pwResult
	closed    bool
}

type pwResponse struct {
	Status int
	URL    string
}

type pwResult struct {
	Result json.RawMessage
	Err    string
}

// pwConnect 建立连接并完成 initialize 握手，返回可用客户端。
func pwConnect(ctx context.Context, endpoint string) (*pwClient, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("Failed to connect: %v", err)
	}

	c := &pwClient{
		conn:      conn,
		frames:    map[string]string{},
		responses: map[string]pwResponse{},
		pending:   map[string]chan pwResult{},
	}
	go c.readLoop()

	// 握手：initialize。__create__(Browser) 在 result 之前到达，
	// reader 按序处理，result 送达时 browser/version 已就位。
	if _, err := c.call(ctx, "", "initialize", map[string]any{
		"sdkLanguage":            "javascript",
		"supportsLoneSurrogates": true,
	}, 0); err != nil {
		_ = conn.Close()
		return nil, err
	}

	c.mu.Lock()
	browser, closed := c.browser, c.closed
	c.mu.Unlock()
	if closed || browser == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("Unexpected server response: no pre-launched browser")
	}
	return c, nil
}

// Version 返回远程 Chromium 版本（官方 testRemoteBrowser 用 browser.version()）。
func (c *pwClient) Version() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// Close 关闭连接（server 侧会随之回收本连接派生的全部对象）。
func (c *pwClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.conn.Close()
	c.failPending("connection closed")
}

// checkPage 走完整流程：newContext → newPage → goto → 读状态码 → 关闭 context。
// 返回 HTTP 状态码与最终 URL（官方 res.status()；≥400 时官方抛错判 DOWN）。
func (c *pwClient) checkPage(ctx context.Context, target string, timeout time.Duration) (int, string, error) {
	// context
	var nc struct {
		Context struct {
			GUID string `json:"guid"`
		} `json:"context"`
	}
	if err := c.callInto(ctx, &nc, c.browser, "newContext", map[string]any{
		"noDefaultViewport": false,
		"selectorEngines":   []any{},
	}, 0); err != nil {
		return 0, "", err
	}
	defer func() {
		if nc.Context.GUID != "" {
			callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = c.call(callCtx, nc.Context.GUID, "close", map[string]any{}, 0)
			cancel()
		}
	}()

	// page
	var np struct {
		Page struct {
			GUID string `json:"guid"`
		} `json:"page"`
	}
	if err := c.callInto(ctx, &np, nc.Context.GUID, "newPage", map[string]any{}, 0); err != nil {
		return 0, "", err
	}
	c.mu.Lock()
	frame := c.frames[np.Page.GUID]
	c.mu.Unlock()
	if frame == "" {
		return 0, "", fmt.Errorf("no main frame for page %s", np.Page.GUID)
	}

	// goto（官方 timeout 语义：interval * 1000 * 0.8，这里由调用方折算传入）
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var gt struct {
		Response struct {
			GUID string `json:"guid"`
		} `json:"response"`
	}
	if err := c.callInto(ctx, &gt, frame, "goto", map[string]any{
		"url":       target,
		"waitUntil": "domcontentloaded",
	}, int(timeout.Milliseconds())); err != nil {
		return 0, "", err
	}

	if gt.Response.GUID == "" {
		// 某些跳转边界 result 不带 response；按官方语义无状态码可判
		return 0, "", fmt.Errorf("no response for goto %s", target)
	}
	c.mu.Lock()
	resp := c.responses[gt.Response.GUID]
	c.mu.Unlock()
	return resp.Status, resp.URL, nil
}

// ---------------- 报文与读写 ----------------

// call 发起一次远端调用并等待 result；result 原文回传。
func (c *pwClient) call(ctx context.Context, guid, method string, params map[string]any, timeoutMS int) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan pwResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(id, guid, method, params, timeoutMS); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("Timeout while loading (context done)")
	case res := <-ch:
		if res.Err != "" {
			return nil, fmt.Errorf("%s", res.Err)
		}
		return res.Result, nil
	}
}

// callInto 是 call 的 JSON 解码便捷封装。
func (c *pwClient) callInto(ctx context.Context, dst any, guid, method string, params map[string]any, timeoutMS int) error {
	raw, err := c.call(ctx, guid, method, params, timeoutMS)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("Unexpected server response for %s: %v", method, err)
	}
	return nil
}

// send 序列化并写出一帧（client → server 帧必须带掩码，gorilla 自动处理）。
func (c *pwClient) send(id, guid, method string, params map[string]any, timeoutMS int) error {
	msg := map[string]any{
		"id":     id,
		"guid":   guid,
		"method": method,
		"params": params,
	}
	if timeoutMS > 0 {
		msg["metadata"] = map[string]any{"internal": false, "timeout": timeoutMS}
	} else {
		msg["metadata"] = map[string]any{"internal": true, "timeout": 0}
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// readLoop 是唯一的读帧 goroutine：解析 __create__、分发 result。
func (c *pwClient) readLoop() {
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			c.failPending(err.Error())
			return
		}
		if mt != websocket.TextMessage {
			continue
		}

		var msg struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Inner *struct {
					Message string `json:"message"`
					Name    string `json:"name"`
				} `json:"error"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch {
		case msg.Method == "__create__":
			c.handleCreate(msg.Params)

		case msg.Method == "__dispose__", msg.Method == "__adopt__":
			// 生命周期事件，无需处理

		case msg.ID != "":
			res := pwResult{Result: msg.Result}
			if msg.Error != nil {
				res.Err = msg.Error.Message
				if msg.Error.Inner != nil && msg.Error.Inner.Message != "" {
					res.Err = msg.Error.Inner.Message
				}
			}
			c.deliver(msg.ID, res)
		}
	}
}

// handleCreate 记录探测需要的对象投影：Browser 版本、Page 主帧、Response 状态码。
func (c *pwClient) handleCreate(params json.RawMessage) {
	var p struct {
		Type        string          `json:"type"`
		GUID        string          `json:"guid"`
		Initializer json.RawMessage `json:"initializer"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.GUID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	switch p.Type {
	case "Browser":
		var init struct {
			Version string `json:"version"`
		}
		_ = json.Unmarshal(p.Initializer, &init)
		if c.browser == "" { // 只记第一个（预启动的那个）
			c.browser = p.GUID
			c.version = init.Version
		}
	case "Page":
		var init struct {
			MainFrame struct {
				GUID string `json:"guid"`
			} `json:"mainFrame"`
		}
		_ = json.Unmarshal(p.Initializer, &init)
		if init.MainFrame.GUID != "" {
			c.frames[p.GUID] = init.MainFrame.GUID
		}
	case "Response":
		var init struct {
			Status int    `json:"status"`
			URL    string `json:"url"`
		}
		_ = json.Unmarshal(p.Initializer, &init)
		c.responses[p.GUID] = pwResponse{Status: init.Status, URL: init.URL}
	}
}

func (c *pwClient) deliver(id string, res pwResult) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- res
	}
}

func (c *pwClient) failPending(errMsg string) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan pwResult{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- pwResult{Err: errMsg}
	}
}

// ---------------- 对外入口 ----------------

// TestRemoteBrowser 对应官方 testRemoteBrowser：连上端点并确认预启动浏览器。
func TestRemoteBrowser(ctx context.Context, endpoint string) error {
	c, err := pwConnect(ctx, endpoint)
	if err != nil {
		return err
	}
	defer c.Close()
	if c.Version() == "" {
		return fmt.Errorf("Connected, but no browser version reported")
	}
	return nil
}

// CheckRemoteBrowserPage 用远程浏览器加载页面，返回 HTTP 状态码。
// 官方 real-browser-monitor-type.js:264 语义：200-399 为 UP（msg=状态码），
// 其余抛错判 DOWN；timeout = interval * 0.8 由调用方折算。
func CheckRemoteBrowserPage(ctx context.Context, endpoint, target string, timeout time.Duration) (int, string, error) {
	c, err := pwConnect(ctx, endpoint)
	if err != nil {
		return 0, "", err
	}
	defer c.Close()
	return c.checkPage(ctx, target, timeout)
}
