package checkers

import (
	"context"
	"crypto/md5" //nolint:gosec // RADIUS RFC 2865 规定必须用 MD5 做混淆，不是安全选型
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// RADIUS 报文类型（RFC 2865 §3）。
const (
	radiusAccessRequest   = 1
	radiusAccessAccept    = 2
	radiusAccessReject    = 3
	radiusAccessChallenge = 11
)

// RADIUS 属性类型（RFC 2865 §5）。
const (
	radiusAttrUserName         = 1
	radiusAttrUserPassword     = 2
	radiusAttrCalledStationID  = 30
	radiusAttrCallingStationID = 31
)

// radiusCodeName 把报文类型翻成官方返回给前端的文案。
//
// 官方的 bean.msg = resp.code，而 radius.decode 返回的是字典里的名字，
// 所以成功时消息就是 "Access-Accept"，挑战时是 "Access-Challenge"。
func radiusCodeName(code byte) string {
	switch code {
	case radiusAccessAccept:
		return "Access-Accept"
	case radiusAccessReject:
		return "Access-Reject"
	case radiusAccessChallenge:
		return "Access-Challenge"
	default:
		return "Code-" + strconv.Itoa(int(code))
	}
}

// RadiusChecker 发一个 RADIUS Access-Request，按响应码判定状态。
//
// 官方走 server/radius-client.js + node radius 库：UDP 单包、超时重试 1 次、
// Access-Reject 抛错。这里按 RFC 2865 自写报文，行为对齐：
//
//	Access-Accept / Access-Challenge → UP，消息为响应码名
//	Access-Reject                    → DOWN "RADIUS Access-Reject from host:port"
//	无响应                           → DOWN "RADIUS request timeout after 2 attempts"
type RadiusChecker struct{}

// Type 实现 Checker。
func (RadiusChecker) Type() string { return "radius" }

func init() { Register(RadiusChecker{}) }

// Check 实现 Checker。
func (RadiusChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}
	port := m.Port()
	if port <= 0 {
		port = 1812 // 官方对老监控（port 为空）兜底 1812
	}
	secret := m.Get("radius_secret")
	if secret == "" {
		return Down("Radius Secret is required")
	}

	// 官方超时 = interval * 1000 * 0.4（毫秒），单次尝试；总共 2 次尝试。
	attemptTimeout := time.Duration(float64(m.Interval) * 0.4 * float64(time.Second))
	if attemptTimeout <= 0 {
		attemptTimeout = 2500 * time.Millisecond
	}
	if d := Timeout(m); d > 0 && d < attemptTimeout {
		attemptTimeout = d
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	attrs := [][2]string{
		{strconv.Itoa(radiusAttrUserName), m.Get("radius_username")},
		{strconv.Itoa(radiusAttrUserPassword), m.Get("radius_password")},
		{strconv.Itoa(radiusAttrCallingStationID), m.Get("radius_calling_station_id")},
		{strconv.Itoa(radiusAttrCalledStationID), m.Get("radius_called_station_id")},
	}

	const attempts = 2 // 官方 retries=1，即首次 + 1 次重试
	start := time.Now()
	for i := 0; i < attempts; i++ {
		code, msg := radiusExchange(ctx, addr, []byte(secret), attrs, attemptTimeout)
		if msg != "" {
			// 只有超时才重试（官方 radius-client 也是这个语义），
			// 报文/套接字错误直接失败，免得错密钥要白等两个超时周期。
			if msg != "timeout" {
				return Down("RADIUS authentication failed for %s: %s", addr, msg)
			}
			continue
		}

		switch code {
		case radiusAccessAccept, radiusAccessChallenge:
			return Up(radiusCodeName(code), time.Since(start).Milliseconds())
		case radiusAccessReject:
			// 官方把 Reject 当错误抛出，文案带 host:port，且不重试
			return Down("RADIUS %s from %s", radiusCodeName(code), addr)
		default:
			// 其它码官方原样返回 resp.code，即视为可达
			return Up(radiusCodeName(code), time.Since(start).Milliseconds())
		}
	}

	// 官方包装：RADIUS request timeout after N attempts
	return Down("RADIUS request timeout after %d attempts", attempts)
}

// radiusExchange 发一次请求并等一个响应。
// 返回 (响应码, 错误文案)；成功时错误文案为空。
func radiusExchange(
	ctx context.Context,
	addr string,
	secret []byte,
	attrs [][2]string,
	timeout time.Duration,
) (byte, string) {
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := d.DialContext(dialCtx, "udp", addr)
	if err != nil {
		return 0, "RADIUS socket error: " + err.Error()
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	reqID, pkt, err := buildRadiusRequest(secret, attrs)
	if err != nil {
		return 0, "RADIUS packet encoding failed: " + err.Error()
	}

	if _, err := conn.Write(pkt); err != nil {
		return 0, "Failed to send RADIUS request: " + err.Error()
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return 0, "timeout"
		}
		return 0, "RADIUS socket error: " + err.Error()
	}

	return parseRadiusResponse(buf[:n], reqID, pkt[4:20], secret)
}

// buildRadiusRequest 组一个 Access-Request。
// 返回请求 ID 与完整报文；调用方需要用 pkt[4:20]（Request Authenticator）校验响应。
func buildRadiusRequest(secret []byte, attrs [][2]string) (byte, []byte, error) {
	var idBuf [1]byte
	if _, err := rand.Read(idBuf[:]); err != nil {
		return 0, nil, err
	}
	id := idBuf[0]

	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		return 0, nil, err
	}

	body := make([]byte, 0, 128)
	for _, kv := range attrs {
		typ, err := strconv.Atoi(kv[0])
		if err != nil || typ <= 0 || typ > 255 {
			continue
		}
		var value []byte
		if typ == radiusAttrUserPassword {
			value = encodeRadiusPassword(kv[1], secret, auth)
		} else {
			if kv[1] == "" {
				continue // 空属性对某些服务端是非法的，直接不发
			}
			value = []byte(kv[1])
		}
		if len(value) == 0 || len(value) > 253 {
			continue
		}
		body = append(body, byte(typ), byte(len(value)+2))
		body = append(body, value...)
	}

	total := 20 + len(body)
	if total > 4096 {
		return 0, nil, fmt.Errorf("packet too large: %d bytes", total)
	}

	pkt := make([]byte, 0, total)
	pkt = append(pkt, radiusAccessRequest, id, 0, 0)
	pkt = append(pkt, auth...)
	pkt = append(pkt, body...)
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))

	return id, pkt, nil
}

// encodeRadiusPassword 按 RFC 2865 §5.2 做 User-Password 混淆。
// 密码补零到 16 的整数倍，然后逐块 c_i = p_i XOR MD5(secret + c_{i-1})，首块用 Request Authenticator。
func encodeRadiusPassword(password string, secret, requestAuth []byte) []byte {
	if password == "" {
		return nil
	}

	padded := make([]byte, ((len(password)+15)/16)*16)
	copy(padded, password)

	out := make([]byte, 0, len(padded))
	prev := requestAuth
	for off := 0; off < len(padded); off += 16 {
		h := md5.New() //nolint:gosec // 协议规定
		h.Write(secret)
		h.Write(prev)
		sum := h.Sum(nil)

		block := make([]byte, 16)
		for i := 0; i < 16; i++ {
			block[i] = padded[off+i] ^ sum[i]
		}
		out = append(out, block...)
		prev = block
	}
	return out
}

// parseRadiusResponse 校验长度与 Response Authenticator，返回响应码。
func parseRadiusResponse(buf []byte, reqID byte, requestAuth, secret []byte) (byte, string) {
	if len(buf) < 20 {
		return 0, "RADIUS response decoding failed: packet too short"
	}
	code := buf[0]
	if buf[1] != reqID {
		return 0, "RADIUS response decoding failed: identifier mismatch"
	}
	length := int(binary.BigEndian.Uint16(buf[2:4]))
	if length < 20 || length > len(buf) {
		return 0, "RADIUS response decoding failed: invalid length"
	}

	// ResponseAuth = MD5(Code + ID + Length + RequestAuth + Attributes + Secret)
	h := md5.New() //nolint:gosec // 协议规定
	h.Write(buf[0:4])
	h.Write(requestAuth)
	h.Write(buf[20:length])
	h.Write(secret)
	want := h.Sum(nil)

	for i := 0; i < 16; i++ {
		if want[i] != buf[4+i] {
			return 0, "RADIUS response decoding failed: invalid response authenticator (wrong shared secret?)"
		}
	}

	return code, ""
}
