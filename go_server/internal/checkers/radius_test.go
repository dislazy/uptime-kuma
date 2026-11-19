package checkers

import (
	"context"
	"crypto/md5" //nolint:gosec // 仅测试里复算协议规定的 MD5
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// fakeRadiusServer 起一个只回单个响应的 UDP 服务端，返回监听端口。
func fakeRadiusServer(t *testing.T, secret []byte, code byte, delay time.Duration) int {
	t.Helper()

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 4096)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 20 {
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}

		resp := make([]byte, 20)
		resp[0] = code
		resp[1] = buf[1] // 同一个 Identifier
		binary.BigEndian.PutUint16(resp[2:4], 20)

		h := md5.New() //nolint:gosec // 协议规定
		h.Write(resp[0:4])
		h.Write(buf[4:20]) // Request Authenticator
		h.Write(secret)
		copy(resp[4:20], h.Sum(nil))

		_, _ = pc.WriteTo(resp, addr)
	}()

	return pc.LocalAddr().(*net.UDPAddr).Port
}

func radiusMonitor(port int) *model.Monitor {
	return &model.Monitor{
		Interval: 10,
		Raw: map[string]any{
			"hostname":                  "127.0.0.1",
			"port":                      int64(port),
			"radius_username":           "monitor",
			"radius_password":           "secret-pass",
			"radius_secret":             "testing123",
			"radius_calling_station_id": "00-11-22-33-44-55",
			"radius_called_station_id":  "ssid-test",
		},
	}
}

func TestRadiusAccept(t *testing.T) {
	secret := []byte("testing123")
	port := fakeRadiusServer(t, secret, radiusAccessAccept, 0)

	res := RadiusChecker{}.Check(context.Background(), radiusMonitor(port))
	if res.Status != model.StatusUp {
		t.Fatalf("状态 = %d，期望 UP（消息 %q）", res.Status, res.Msg)
	}
	if res.Msg != "Access-Accept" {
		t.Fatalf("消息 = %q，期望 Access-Accept", res.Msg)
	}
}

func TestRadiusChallenge(t *testing.T) {
	secret := []byte("testing123")
	port := fakeRadiusServer(t, secret, radiusAccessChallenge, 0)

	res := RadiusChecker{}.Check(context.Background(), radiusMonitor(port))
	if res.Status != model.StatusUp || res.Msg != "Access-Challenge" {
		t.Fatalf("得到 %d / %q，期望 UP / Access-Challenge", res.Status, res.Msg)
	}
}

func TestRadiusReject(t *testing.T) {
	secret := []byte("testing123")
	port := fakeRadiusServer(t, secret, radiusAccessReject, 0)

	res := RadiusChecker{}.Check(context.Background(), radiusMonitor(port))
	if res.Status != model.StatusDown {
		t.Fatalf("状态 = %d，期望 DOWN", res.Status)
	}
	if res.Msg != "RADIUS Access-Reject from 127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("消息 = %q", res.Msg)
	}
}

// TestRadiusWrongSecret 校验 Response Authenticator 会把错共享密钥识别出来。
func TestRadiusWrongSecret(t *testing.T) {
	port := fakeRadiusServer(t, []byte("another-secret"), radiusAccessAccept, 0)

	res := RadiusChecker{}.Check(context.Background(), radiusMonitor(port))
	if res.Status != model.StatusDown {
		t.Fatalf("状态 = %d，期望 DOWN", res.Status)
	}
}

func TestRadiusTimeout(t *testing.T) {
	// 不启服务端，直接打一个没人监听的端口 → 两次尝试都超时
	m := radiusMonitor(1)
	m.Interval = 1 // 单次超时 400ms，两次共 800ms

	start := time.Now()
	res := RadiusChecker{}.Check(context.Background(), m)
	if res.Status != model.StatusDown {
		t.Fatalf("状态 = %d，期望 DOWN", res.Status)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("超时判定太慢: %v", time.Since(start))
	}
}

// TestEncodeRadiusPassword 校验密码混淆长度与零填充规则（RFC 2865 §5.2）。
func TestEncodeRadiusPassword(t *testing.T) {
	secret := []byte("s3cr3t")
	auth := make([]byte, 16)

	if got := encodeRadiusPassword("", secret, auth); got != nil {
		t.Fatalf("空密码应返回 nil，得到 %d 字节", len(got))
	}
	for _, pw := range []string{"a", "0123456789abcdef", "0123456789abcdefg", "a-much-longer-password-value-here"} {
		out := encodeRadiusPassword(pw, secret, auth)
		if len(out)%16 != 0 {
			t.Fatalf("%q → %d 字节，应为 16 的整数倍", pw, len(out))
		}
		if len(out) < len(pw) {
			t.Fatalf("%q → %d 字节，短于原文", pw, len(out))
		}
	}
}
