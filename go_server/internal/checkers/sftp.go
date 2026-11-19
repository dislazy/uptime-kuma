package checkers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// SFTPChecker 连一次 SFTP 服务器，可选校验远端路径存在。
//
// 官方用 ssh2-sftp-client，字段是 sshUsername / sshPassword / sshPrivateKey /
// sshPassphrase / sshAuthMethod / sftpPath。这里用 x/crypto/ssh + pkg/sftp，
// 行为对齐：连上 → 可选 stat → 断开。
type SFTPChecker struct{}

// Type 实现 Checker。
func (SFTPChecker) Type() string { return "sftp" }

func init() { Register(SFTPChecker{}) }

// Check 实现 Checker。
func (SFTPChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}
	port := m.Port()
	if port <= 0 {
		port = 22
	}

	timeout := Timeout(m)
	user := m.Get("ssh_username")
	authMethod := m.Get("ssh_auth_method")
	if authMethod == "" {
		authMethod = "password"
	}

	var auth ssh.AuthMethod
	if authMethod == "privateKey" {
		key := m.Get("ssh_private_key")
		if key == "" {
			return Down("SSH private key is required for key-based authentication")
		}
		var signer ssh.Signer
		var err error
		if pass := m.Get("ssh_passphrase"); pass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(pass))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key))
		}
		if err != nil {
			if strings.Contains(err.Error(), "encrypted") {
				return Down("Failed to parse SSH private key: the key is encrypted, passphrase is required")
			}
			return Down("Failed to parse SSH private key: %v", err)
		}
		auth = ssh.PublicKeys(signer)
	} else {
		auth = ssh.Password(m.Get("ssh_password"))
	}

	clientCfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // 与官方一致，监控不做主机指纹校验
		Timeout:         timeout,
	}

	start := time.Now()
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Down("%s", formatSFTPError(err, host, port))
	}

	// 握手也要受 ctx 约束，否则 svc 卡住时 goroutine 会一直挂着
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(host, strconv.Itoa(port)), clientCfg)
	if err != nil {
		_ = conn.Close()
		return Down("%s", formatSFTPError(err, host, port))
	}
	sshClient := ssh.NewClient(sshConn, chans, reqs)
	defer sshClient.Close()
	_ = conn.SetDeadline(time.Time{})

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		return Down("%s", formatSFTPError(err, host, port))
	}
	defer sftpClient.Close()

	if path := m.Get("sftp_path"); path != "" {
		if _, err := sftpClient.Stat(path); err != nil {
			return Down("Path %q does not exist on the SFTP server", path)
		}
	}

	return Up("SFTP connection successful", time.Since(start).Milliseconds())
}

// formatSFTPError 把底层错误翻译成人话，行为对齐官方 formatSftpError。
func formatSFTPError(err error, host string, port int) string {
	target := fmt.Sprintf("%s:%d", host, port)

	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("Connection refused — %s actively rejected the connection", target)
	case errors.Is(err, syscall.ECONNRESET):
		return fmt.Sprintf("Connection reset by remote host %s", target)
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return fmt.Sprintf("Network unreachable — cannot reach %s", target)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf("Host not found — cannot resolve hostname %q", host)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("Connection timed out — %s did not respond in time", target)
	}

	raw := strings.TrimSpace(err.Error())
	// ssh2-sftp-client 的 "xxx: " 前缀在 Go 侧不存在，但错误里常带 "ssh: "，
	// 去掉它读起来更顺。
	raw = strings.TrimPrefix(raw, "ssh: ")
	if raw == "" || raw == "EOF" {
		return fmt.Sprintf("Connection failed — %s is unreachable or offline", target)
	}
	return raw
}
