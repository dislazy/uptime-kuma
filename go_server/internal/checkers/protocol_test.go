package checkers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// newMonitor 构造一个带 Raw 的监控，方便直接测 Check。
func newMonitor(typ string, raw map[string]any) *model.Monitor {
	raw["type"] = typ
	return &model.Monitor{ID: 1, Name: "t", Type: typ, Interval: 20, Active: true, Raw: raw}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------- NTP ----------

func TestNTPUpAndThresholds(t *testing.T) {
	addr, stop := startFakeNTP(t, 2, 0)
	defer stop()

	ctx := testCtx(t)
	res := NTPChecker{}.Check(ctx, newMonitor("ntp", map[string]any{
		"hostname": "127.0.0.1",
		"port":     portOf(addr),
	}))
	if res.Status != model.StatusUp {
		t.Fatalf("want UP, got %v %s", res.Status, res.Msg)
	}
	if !strings.Contains(res.Msg, "Stratum: 2") || !strings.Contains(res.Msg, "RefID:") {
		t.Fatalf("unexpected msg: %s", res.Msg)
	}
}

func TestNTPUnsynchronized(t *testing.T) {
	addr, stop := startFakeNTP(t, 16, 0)
	defer stop()

	res := NTPChecker{}.Check(testCtx(t), newMonitor("ntp", map[string]any{
		"hostname": "127.0.0.1",
		"port":     portOf(addr),
	}))
	if res.Status != model.StatusDown || !strings.Contains(res.Msg, "unsynchronized") {
		t.Fatalf("want down/unsynchronized, got %v %s", res.Status, res.Msg)
	}
}

func TestNTPOffsetThreshold(t *testing.T) {
	// 服务器时间故意偏 5000ms，阈值设 1000ms
	addr, stop := startFakeNTP(t, 2, 5000)
	defer stop()

	res := NTPChecker{}.Check(testCtx(t), newMonitor("ntp", map[string]any{
		"hostname":                      "127.0.0.1",
		"port":                          portOf(addr),
		"ntp_time_offset_threshold":     1000,
		"ntp_stratum_threshold":         5,
		"ntp_root_dispersion_threshold": 500,
	}))
	if res.Status != model.StatusDown || !strings.Contains(res.Msg, "Time offset") {
		t.Fatalf("want down/time offset, got %v %s", res.Status, res.Msg)
	}
}

// startFakeNTP 起一个 UDP 假 NTP 服务器；offsetMS 是它相对本机的时间偏移。
func startFakeNTP(t *testing.T, stratum byte, offsetMS int64) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			resp := make([]byte, 48)
			resp[0] = 0x1c // LI=0 VN=3 Mode=4(server)
			resp[1] = stratum
			// root dispersion = 0.001s
			binary.BigEndian.PutUint32(resp[8:12], 65)
			copy(resp[12:16], []byte{127, 0, 0, 1})

			now := time.Now().UnixMilli() + ntpEpochOffsetMS + offsetMS
			// t2 = 收到请求的时间，t3 = 发回时间，两者相同
			putNTPTime(resp[32:40], now)
			putNTPTime(resp[40:48], now)

			select {
			case <-done:
				return
			default:
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()

	return pc.LocalAddr().String(), func() {
		close(done)
		_ = pc.Close()
	}
}

func putNTPTime(dst []byte, ms int64) {
	sec := uint32(ms / 1000)
	frac := uint32((ms % 1000) * 0x100000000 / 1000)
	binary.BigEndian.PutUint32(dst[0:4], sec)
	binary.BigEndian.PutUint32(dst[4:8], frac)
}

// ---------- SIP ----------

func TestSIPOptionsOK(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	go func() {
		buf := make([]byte, 4096)
		_, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = pc.WriteTo([]byte("SIP/2.0 200 OK\r\nCall-ID: x\r\nContent-Length: 0\r\n\r\n"), addr)
	}()

	res := SIPOptionsChecker{}.Check(testCtx(t), newMonitor("sip-options", map[string]any{
		"hostname": "127.0.0.1",
		"port":     portOf(pc.LocalAddr().String()),
	}))
	if res.Status != model.StatusUp || !strings.Contains(res.Msg, "200 OK") {
		t.Fatalf("want UP/200 OK, got %v %s", res.Status, res.Msg)
	}
}

// ---------- Minecraft ----------

func TestMinecraftStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeMinecraft(conn)
		}
	}()

	res := GameDigChecker{}.Check(testCtx(t), newMonitor("gamedig", map[string]any{
		"hostname": "127.0.0.1",
		"port":     portOf(ln.Addr().String()),
		"game":     "minecraft",
	}))
	if res.Status != model.StatusUp {
		t.Fatalf("want UP, got %v %s", res.Status, res.Msg)
	}
	if !strings.Contains(res.Msg, "Fake Server") || !strings.Contains(res.Msg, "3/20") {
		t.Fatalf("unexpected msg: %s", res.Msg)
	}
}

func serveFakeMinecraft(conn net.Conn) {
	defer conn.Close()
	// 读握手：varint 长度 + 包体
	br := bufio.NewReader(conn)
	length, err := readVarInt(br)
	if err != nil {
		return
	}
	if _, err := readN(br, int(length)); err != nil {
		return
	}
	// 读 status 请求（2 字节）
	if _, err := readN(br, 2); err != nil {
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"version":     map[string]any{"name": "1.20.4"},
		"players":     map[string]any{"online": 3, "max": 20},
		"description": map[string]any{"text": "Fake Server"},
	})

	var body bytes.Buffer
	writeVarInt(&body, 0)
	writeMCString(&body, string(payload))

	var pkt bytes.Buffer
	writeVarInt(&pkt, int32(body.Len()))
	pkt.Write(body.Bytes())
	_, _ = conn.Write(pkt.Bytes())
}

func readN(conn io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := ioReadFull(conn, buf)
	return buf, err
}

func ioReadFull(conn io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---------- gRPC ----------

const testProto = `
syntax = "proto3";
package demo;

service Greeter {
  rpc SayHello (HelloRequest) returns (HelloReply) {}
}

message HelloRequest { string name = 1; }
message HelloReply { string message = 1; }
`

func TestGRPCProtoParse(t *testing.T) {
	files, err := parseProto(testProto)
	if err != nil {
		t.Fatal(err)
	}
	svc := findService(files, "Greeter")
	if svc == nil {
		t.Fatal("service not found by short name")
	}
	if svc.GetFullyQualifiedName() != "demo.Greeter" {
		t.Fatalf("unexpected fqn: %s", svc.GetFullyQualifiedName())
	}
	if findMethod(svc, "sayhello") == nil {
		t.Fatal("method lookup should be case-insensitive")
	}
}

// ---------- 工具 ----------

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}
