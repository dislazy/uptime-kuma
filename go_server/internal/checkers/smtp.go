package checkers

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// SMTPChecker 完成一次 SMTP 会话握手来判断邮件服务是否可用。
//
// 对应官方的 transporter.verify()：连接 → EHLO →（可选 STARTTLS）→ 断开。
// 这里没有引 SMTP 库——整个交互只有四五行命令，自己写反而能精确控制
// 加密策略（secure / starttls / nostarttls 三种模式）。
type SMTPChecker struct{}

// Type 实现 Checker。
func (SMTPChecker) Type() string { return "smtp" }

func init() { Register(SMTPChecker{}) }

// Check 实现 Checker。
func (SMTPChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := strings.TrimSpace(m.Hostname())
	if host == "" {
		return Down("Hostname is empty")
	}

	port := m.Port()
	if port <= 0 {
		port = 25
	}

	// 官方三种取值：secure（直连 TLS）、starttls（强制 STARTTLS）、nostarttls（明文）
	security := strings.ToLower(strings.TrimSpace(m.Get("smtp_security")))
	insecure := m.GetBool("ignore_tls")

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{}

	start := time.Now()

	var conn net.Conn
	var err error
	if security == "secure" {
		td := &tls.Dialer{
			NetDialer: dialer,
			Config: &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: insecure,
			},
		}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return Down("SMTP connection doesn't verify: %s", simplifyNetErr(err).Error())
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	r := bufio.NewReader(conn)

	// 1) 服务端问候
	code, lines, err := readSMTPResponse(r)
	if err != nil {
		return Down("SMTP connection doesn't verify: %s", err.Error())
	}
	if code != 220 {
		return Down("SMTP connection doesn't verify: %d %s", code, lastSMTPLine(lines))
	}

	// 2) EHLO（新服务器一律支持，HELO 是老协议不再使用）
	code, lines, err = smtpCommand(conn, r, "EHLO gokuma.local")
	if err != nil {
		return Down("SMTP connection doesn't verify: %s", err.Error())
	}
	if code != 250 {
		// 有些内网设备只认 HELO
		code, lines, err = smtpCommand(conn, r, "HELO gokuma.local")
		if err != nil {
			return Down("SMTP connection doesn't verify: %s", err.Error())
		}
		if code != 250 {
			return Down("SMTP connection doesn't verify: %d %s", code, lastSMTPLine(lines))
		}
	}

	// 3) 按策略处理 STARTTLS
	if security != "nostarttls" {
		supportsStartTLS := smtpSupports(lines, "STARTTLS")

		if supportsStartTLS {
			code, lines, err = smtpCommand(conn, r, "STARTTLS")
			if err != nil {
				return Down("SMTP connection doesn't verify: %s", err.Error())
			}
			if code != 220 {
				return Down("SMTP connection doesn't verify: STARTTLS rejected with %d %s", code, lastSMTPLine(lines))
			}

			tlsConn := tls.Client(conn, &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: insecure,
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				return Down("SMTP connection doesn't verify: TLS handshake failed: %s", err.Error())
			}

			// TLS 建立后会话状态重置，必须重新 EHLO
			conn = tlsConn
			r = bufio.NewReader(conn)
			if code, lines, err = smtpCommand(conn, r, "EHLO gokuma.local"); err != nil {
				return Down("SMTP connection doesn't verify: %s", err.Error())
			} else if code != 250 {
				return Down("SMTP connection doesn't verify: %d %s", code, lastSMTPLine(lines))
			}
		} else if security == "starttls" {
			// 用户明确要求 STARTTLS，服务端不支持就必须判失败
			return Down("SMTP connection doesn't verify: STARTTLS is required but the server does not support it")
		}
	}

	// 4) 礼貌退出。QUIT 失败不影响判定
	_, _, _ = smtpCommand(conn, r, "QUIT")

	ping := time.Since(start).Milliseconds()
	return Up("SMTP connection verifies successfully", ping)
}

// smtpCommand 发送一条命令并读回响应。
func smtpCommand(w io.Writer, r *bufio.Reader, cmd string) (int, []string, error) {
	if _, err := io.WriteString(w, cmd+"\r\n"); err != nil {
		return 0, nil, fmt.Errorf("send %s failed: %w", strings.Fields(cmd)[0], err)
	}
	code, lines, err := readSMTPResponse(r)
	if err != nil {
		return 0, lines, fmt.Errorf("%s failed: %w", strings.Fields(cmd)[0], err)
	}
	return code, lines, nil
}

// readSMTPResponse 读一条（可能多行的）SMTP 响应。
//
// 多行格式：前几行以 "250-" 开头，最后一行以 "250 " 开头。
func readSMTPResponse(r *bufio.Reader) (int, []string, error) {
	var lines []string

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, lines, fmt.Errorf("read response failed: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return 0, lines, fmt.Errorf("malformed response %q", line)
		}

		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return 0, lines, fmt.Errorf("malformed status code in %q", line)
		}

		lines = append(lines, line)

		// 第 4 个字符不是 '-' 表示这是最后一行
		if len(line) < 4 || line[3] != '-' {
			return code, lines, nil
		}
	}
}

// smtpSupports 检查 EHLO 响应里是否声明了某个扩展。
func smtpSupports(lines []string, feature string) bool {
	feature = strings.ToUpper(feature)
	for _, line := range lines {
		if len(line) < 5 {
			continue
		}
		// 跳过 "250-" / "250 " 前缀
		payload := strings.ToUpper(strings.TrimSpace(line[4:]))
		if payload == feature || strings.HasPrefix(payload, feature+" ") {
			return true
		}
	}
	return false
}

// lastSMTPLine 取最后一行响应用于错误展示。
func lastSMTPLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	last := lines[len(lines)-1]
	if len(last) > 4 {
		return last[4:]
	}
	return last
}
