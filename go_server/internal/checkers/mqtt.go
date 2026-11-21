package checkers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/conditions"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"github.com/gorilla/websocket"
)

// MQTTChecker 订阅一个主题并等待一条消息。
//
// 自写 MQTT 3.1.1 报文（CONNECT / SUBSCRIBE / 收 PUBLISH）而不是引 paho：
// 探测只需要这三个动作，且要精确控制"等多久没消息算失败"，
// 用完整客户端库反而要绕一圈关掉它的自动重连、会话保持等行为。
type MQTTChecker struct{}

// Type 实现 Checker。
func (MQTTChecker) Type() string { return "mqtt" }

func init() { Register(MQTTChecker{}) }

// Check 连上 broker，订阅主题，等第一条消息并按其内容判定。
func (MQTTChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := strings.TrimSpace(m.Hostname())
	if host == "" {
		return Down("Hostname is empty")
	}

	port := m.Port()
	if port <= 0 {
		port = 1883
	}

	topic := strings.TrimSpace(m.Get("mqtt_topic"))
	if topic == "" {
		return Down("Topic is empty")
	}

	start := time.Now()

	conn, err := dialMQTT(ctx, m, host, port)
	if err != nil {
		return Down("%s", err.Error())
	}
	defer conn.Close()

	// 整个探测（含等消息）共用一个截止时间，避免 broker 接了连接却一直不推消息
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = conn.SetDeadline(deadline)

	clientID := "gokuma-" + randomHex(8)
	if err := mqttConnect(conn, clientID, m.Get("mqtt_username"), m.Get("mqtt_password")); err != nil {
		return Down("%s", err.Error())
	}

	// QoS 0 订阅：不需要 PUBACK，也就没有第二个失败面
	if err := mqttSubscribe(conn, 1, topic); err != nil {
		return Down("%s", err.Error())
	}

	msg, err := mqttAwaitPublish(conn)
	if err != nil {
		return Down("%s", err.Error())
	}

	ping := time.Since(start).Milliseconds()

	// 条件优先于 check type：官方一旦检测到 conditions 就不再走 keyword / json-query
	if rawCond := m.Get("conditions"); conditions.Has(rawCond) {
		jsonValue := ""
		if path := strings.TrimSpace(m.Get("json_path")); path != "" {
			// 解析失败时官方保持空串，不视为探测失败
			if v, err := evalJSONPath([]byte(msg.payload), path); err == nil && v != nil {
				jsonValue = stringify(v)
			}
		}

		ok, err := conditions.Eval(rawCond, map[string]any{
			"topic":      msg.topic,
			"message":    msg.payload,
			"json_value": jsonValue,
		})
		if err != nil {
			return Down("%s", err.Error())
		}
		if ok {
			return Up(fmt.Sprintf("Topic: %s; Message: %s", msg.topic, msg.payload), ping)
		}
		return Down("Conditions not met - Topic: %s; Message: %s", msg.topic, msg.payload)
	}

	checkType := strings.TrimSpace(m.Get("mqtt_check_type"))
	if checkType == "" {
		checkType = "keyword"
	}

	switch checkType {
	case "keyword":
		success := m.Get("mqtt_success_message")
		if strings.Contains(msg.payload, success) {
			return Up(fmt.Sprintf("Topic: %s; Message: %s", msg.topic, msg.payload), ping)
		}
		return Down("Message Mismatch - Topic: %s; Message: %s", topic, msg.payload)

	case "json-query":
		actual, err := evalJSONPath([]byte(msg.payload), m.Get("json_path"))
		if err != nil {
			return Down("%s", err.Error())
		}

		op := strings.TrimSpace(m.Get("json_path_operator"))
		if op == "" {
			op = "=="
		}
		if compareJSON(actual, m.Get("expected_value"), op) {
			return Up("Message received, expected value is found", ping)
		}
		return Down("Message received but value is not equal to expected value, value was: [%s]", stringify(actual))

	default:
		return Down("Unknown MQTT Check Type")
	}
}

// ---------------- 连接建立 ----------------

// dialMQTT 按 monitor 配置选择 TCP 或 WebSocket 传输。
//
// 前端有 mqtt_websocket_path 这个字段：配了就走 mqtt over websocket，
// 常见于浏览器侧或只开放 80/443 的 broker。
func dialMQTT(ctx context.Context, m *model.Monitor, host string, port int) (net.Conn, error) {
	wsPath := strings.TrimSpace(m.Get("mqtt_websocket_path"))
	if wsPath == "" {
		d := &net.Dialer{}
		return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}

	if !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}

	scheme := "ws"
	if m.GetBool("ignore_tls") || strings.EqualFold(m.Get("mqtt_protocol"), "wss") {
		scheme = "wss"
	}

	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: wsPath}
	dialer := &websocket.Dialer{
		Subprotocols:     []string{"mqtt"},
		HandshakeTimeout: 10 * time.Second,
		NetDialContext:   (&net.Dialer{}).DialContext,
	}

	ws, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("websocket connect failed: %w", err)
	}
	return &wsConn{ws: ws}, nil
}

// wsConn 把 websocket 的二进制消息序列适配成 net.Conn 的字节流。
type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			_, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			c.reader = r
		}

		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error         { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	_ = c.ws.SetReadDeadline(t)
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// ---------------- MQTT 3.1.1 报文 ----------------

const (
	mqttTypeConnect   byte = 1
	mqttTypeConnack   byte = 2
	mqttTypePublish   byte = 3
	mqttTypeSubscribe byte = 8
	mqttTypeSuback    byte = 9
)

// mqttConnect 发送 CONNECT 并校验 CONNACK。
func mqttConnect(rw io.ReadWriter, clientID, username, password string) error {
	var payload bytes.Buffer
	writeMQTTString(&payload, clientID)
	if username != "" {
		writeMQTTString(&payload, username)
	}
	if password != "" {
		writeMQTTString(&payload, password)
	}

	var vh bytes.Buffer
	writeMQTTString(&vh, "MQTT")
	vh.WriteByte(0x04) // 协议级别 4 = 3.1.1

	var flags byte = 0x02 // clean session
	if username != "" {
		flags |= 0x80
	}
	if password != "" {
		flags |= 0x40
	}
	vh.WriteByte(flags)
	vh.Write([]byte{0x00, 0x3C}) // keepalive 60s

	body := append(vh.Bytes(), payload.Bytes()...)
	if err := writeMQTTPacket(rw, mqttTypeConnect<<4, body); err != nil {
		return fmt.Errorf("send CONNECT failed: %w", err)
	}

	hdr, resp, err := readMQTTPacket(rw)
	if err != nil {
		return fmt.Errorf("read CONNACK failed: %w", err)
	}
	if hdr>>4 != mqttTypeConnack {
		return fmt.Errorf("expected CONNACK, got packet type %d", hdr>>4)
	}
	if len(resp) < 2 {
		return fmt.Errorf("malformed CONNACK")
	}

	switch resp[1] {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("MQTT connect refused: unacceptable protocol version")
	case 2:
		return fmt.Errorf("MQTT connect refused: client identifier rejected")
	case 3:
		return fmt.Errorf("MQTT connect refused: server unavailable")
	case 4:
		return fmt.Errorf("MQTT connect refused: bad username or password")
	case 5:
		return fmt.Errorf("MQTT connect refused: not authorized")
	default:
		return fmt.Errorf("MQTT connect refused: code %d", resp[1])
	}
}

// mqttSubscribe 订阅一个主题并等待 SUBACK。
func mqttSubscribe(rw io.ReadWriter, packetID uint16, topic string) error {
	var body bytes.Buffer
	_ = binary.Write(&body, binary.BigEndian, packetID)
	writeMQTTString(&body, topic)
	body.WriteByte(0x00) // 请求 QoS 0

	// SUBSCRIBE 固定头低 4 位必须是 0b0010
	if err := writeMQTTPacket(rw, mqttTypeSubscribe<<4|0x02, body.Bytes()); err != nil {
		return fmt.Errorf("send SUBSCRIBE failed: %w", err)
	}

	for {
		hdr, resp, err := readMQTTPacket(rw)
		if err != nil {
			return fmt.Errorf("read SUBACK failed: %w", err)
		}
		if hdr>>4 != mqttTypeSuback {
			// 订阅前后可能穿插 broker 的其它包，忽略即可
			continue
		}
		if len(resp) >= 3 && resp[2] == 0x80 {
			return fmt.Errorf("subscribe rejected by broker: topic %q", topic)
		}
		return nil
	}
}

// mqttMessage 是一条收到的 PUBLISH。
type mqttMessage struct {
	topic   string
	payload string
}

// mqttAwaitPublish 一直读到 PUBLISH 为止，其它包（PINGRESP 等）跳过。
func mqttAwaitPublish(rw io.ReadWriter) (mqttMessage, error) {
	for {
		hdr, body, err := readMQTTPacket(rw)
		if err != nil {
			return mqttMessage{}, fmt.Errorf("waiting for message: %w", err)
		}
		if hdr>>4 != mqttTypePublish {
			continue
		}
		if len(body) < 2 {
			return mqttMessage{}, fmt.Errorf("malformed PUBLISH")
		}

		topicLen := int(binary.BigEndian.Uint16(body[:2]))
		if len(body) < 2+topicLen {
			return mqttMessage{}, fmt.Errorf("malformed PUBLISH topic")
		}
		topic := string(body[2 : 2+topicLen])

		rest := body[2+topicLen:]
		// QoS 在首字节的 bit1-bit2；>0 时紧跟两字节报文标识符
		qos := (hdr >> 1) & 0x03
		if qos > 0 {
			if len(rest) < 2 {
				return mqttMessage{}, fmt.Errorf("malformed PUBLISH packet id")
			}
			rest = rest[2:]
		}

		return mqttMessage{topic: topic, payload: string(rest)}, nil
	}
}

// readMQTTPacket 读一个完整报文，返回完整首字节（类型在高 4 位、flags 在低 4 位）。
//
// 返回完整字节而不是只返回类型，是因为 PUBLISH 的 QoS 在 flags 里——
// 早先只返回类型又对它右移，QoS 会被算错，载荷被多切两字节。
func readMQTTPacket(r io.Reader) (byte, []byte, error) {
	var hdr [1]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}

	length, err := readMQTTRemainingLength(r)
	if err != nil {
		return 0, nil, err
	}

	body := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, body); err != nil {
			return 0, nil, err
		}
	}
	return hdr[0], body, nil
}

// readMQTTRemainingLength 解析变长剩余长度（每字节 7 位有效，最大 4 字节）。
func readMQTTRemainingLength(r io.Reader) (int, error) {
	var (
		multiplier = 1
		value      int
		buf        [1]byte
	)
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		value += int(buf[0]&0x7F) * multiplier
		if buf[0]&0x80 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, fmt.Errorf("malformed remaining length")
}

// writeMQTTPacket 写一个报文：固定头 + 变长长度 + 载荷。
func writeMQTTPacket(w io.Writer, header byte, body []byte) error {
	buf := make([]byte, 0, len(body)+5)
	buf = append(buf, header)
	buf = append(buf, encodeMQTTRemainingLength(len(body))...)
	buf = append(buf, body...)

	_, err := w.Write(buf)
	return err
}

func encodeMQTTRemainingLength(n int) []byte {
	var out []byte
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

// writeMQTTString 写长度前缀字符串（2 字节大端长度 + UTF-8 内容）。
func writeMQTTString(buf *bytes.Buffer, s string) {
	_ = binary.Write(buf, binary.BigEndian, uint16(len(s)))
	buf.WriteString(s)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
