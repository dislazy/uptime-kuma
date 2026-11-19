package checkers

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// SIPOptionsChecker 向 SIP 服务器发 OPTIONS，判断是否回 200 OK。
//
// 官方是 fork 一个 sipsak 子进程再 grep "200 OK"。这里直接手写 OPTIONS 报文：
// 少一个外部二进制依赖（sipsak 在精简镜像里通常没有），并且能同时支持
// UDP 与 TCP 两种传输——sipsak 默认只走 UDP，很多 SBC 只开 TCP。
type SIPOptionsChecker struct{}

// Type 实现 Checker。
func (SIPOptionsChecker) Type() string { return "sip-options" }

func init() { Register(SIPOptionsChecker{}) }

// Check 实现 Checker。
func (SIPOptionsChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}
	port := m.Port()
	if port <= 0 {
		port = 5060
	}

	start := time.Now()
	statusLine, err := sipOptions(ctx, "udp", host, port)
	if err != nil {
		// UDP 没回应时再试一次 TCP：注册到 SBC 的场景多为 TCP 传输
		if statusLine2, err2 := sipOptions(ctx, "tcp", host, port); err2 == nil {
			statusLine, err = statusLine2, nil
		} else {
			return Down("%s", err.Error())
		}
	}
	ping := time.Since(start).Milliseconds()

	code := sipStatusCode(statusLine)
	if code == 200 {
		return Up(statusLine, ping)
	}
	return Down("%s", statusLine)
}

// sipOptions 在指定传输上完成一次 OPTIONS 事务，返回应答的状态行。
func sipOptions(ctx context.Context, network, host string, port int) (string, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, network, target)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return "", fmt.Errorf("Timeout")
		}
		return "", fmt.Errorf("%s", err.Error())
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	localHost, localPort := splitHostPort(conn.LocalAddr().String())
	branch := "z9hG4bK" + sipTag(10)
	callID := sipTag(16) + "@" + localHost
	fromTag := sipTag(6)
	uriHost := host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		uriHost = "[" + host + "]"
	}
	uri := fmt.Sprintf("sip:%s:%d", uriHost, port)

	var b strings.Builder
	fmt.Fprintf(&b, "OPTIONS %s SIP/2.0\r\n", uri)
	fmt.Fprintf(&b, "Via: SIP/2.0/%s %s:%d;branch=%s;rport\r\n", strings.ToUpper(network), localHost, localPort, branch)
	b.WriteString("Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "From: <sip:sipsak@%s>;tag=%s\r\n", host, fromTag)
	fmt.Fprintf(&b, "To: <%s>\r\n", uri)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", callID)
	b.WriteString("CSeq: 1 OPTIONS\r\n")
	fmt.Fprintf(&b, "Contact: <sip:sipsak@%s:%d;transport=%s>\r\n", localHost, localPort, network)
	b.WriteString("User-Agent: Uptime-Kuma-Go\r\n")
	b.WriteString("Accept: application/sdp\r\n")
	b.WriteString("Content-Length: 0\r\n\r\n")

	if _, err := conn.Write([]byte(b.String())); err != nil {
		return "", fmt.Errorf("%s", err.Error())
	}

	line, err := readSIPStatusLine(conn)
	if err != nil {
		return "", err
	}
	return line, nil
}

// readSIPStatusLine 读到第一个以 SIP/2.0 开头的行；收到 100 Trying 时继续等终态。
func readSIPStatusLine(conn net.Conn) (string, error) {
	reader := bufio.NewReader(conn)
	var last string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if last != "" {
				return last, nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return "", fmt.Errorf("Timeout waiting for SIP response")
			}
			return "", fmt.Errorf("No response from SIP server: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "SIP/2.0") {
			continue
		}
		if sipStatusCode(line) == 100 {
			last = line
			continue
		}
		return line, nil
	}
}

// sipStatusCode 从 "SIP/2.0 200 OK" 里取 200。
func sipStatusCode(statusLine string) int {
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return n
}

// sipTag 生成随机 token。
func sipTag(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)[:n]
}

// splitHostPort 拆分 "1.2.3.4:5678"，失败时原样返回并给一个占位端口。
func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}
