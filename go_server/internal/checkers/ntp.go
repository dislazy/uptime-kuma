package checkers

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// NTPChecker 通过 SNTP（RFC 4330 / RFC 5905 简化版）查询时间服务器。
//
// 官方实现走 dgram 发一个 48 字节的 v3 客户端包，再把 T1..T4 代入
// RFC 5905 的 offset / delay 公式。这里完全照搬，包括三个阈值的默认值，
// 因为前端表单里的占位符就是 5 / 1000 / 500。
type NTPChecker struct{}

// Type 实现 Checker。
func (NTPChecker) Type() string { return "ntp" }

func init() { Register(NTPChecker{}) }

// ntpEpochOffsetMS 是 1900-01-01 到 1970-01-01 的毫秒数。
const ntpEpochOffsetMS = 2208988800000

// Check 实现 Checker。
func (NTPChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}

	port := m.Port()
	if port <= 0 {
		port = 123
	}

	start := time.Now()
	resp, t1, t4, err := queryNTP(ctx, host, port)
	ping := time.Since(start).Milliseconds()
	if err != nil {
		return Down("%s", err.Error())
	}

	stratum := int(resp[1])
	rootDispersionRaw := binary.BigEndian.Uint32(resp[8:12])
	rootDispersion := float64(rootDispersionRaw) / 65536 * 1000

	var refID string
	if stratum <= 1 {
		refID = trimRefID(resp[12:16])
	} else {
		refID = fmt.Sprintf("%d.%d.%d.%d", resp[12], resp[13], resp[14], resp[15])
	}

	t2 := readNTPTimestamp(resp, 32)
	t3 := readNTPTimestamp(resp, 40)

	offset := (t2 - t1 + (t3 - t4)) / 2
	roundTrip := t4 - t1 - (t3 - t2)

	// 官方在阈值判断之前就拼好了这段文案，DOWN 时被错误信息覆盖，
	// 但 UP 时展示的就是它。
	summary := fmt.Sprintf("Stratum: %d, RefID: %s, Offset: %.3fms, Delay: %.3fms, Dispersion: %.3fms",
		stratum, refID, offset, roundTrip, rootDispersion)

	if stratum == 16 {
		return Down("NTP server is unsynchronized (stratum 16)")
	}

	stratumThreshold := int(m.GetInt("ntp_stratum_threshold"))
	if stratumThreshold <= 0 {
		stratumThreshold = 5
	}
	if stratum >= stratumThreshold {
		return Down("Stratum %d meets or exceeds threshold %d", stratum, stratumThreshold)
	}

	offsetThreshold := float64(m.GetFloat("ntp_time_offset_threshold"))
	if offsetThreshold <= 0 {
		offsetThreshold = 1000
	}
	if math.Abs(offset) >= offsetThreshold {
		return Down("Time offset %.3fms exceeds threshold %.0fms", offset, offsetThreshold)
	}

	dispersionThreshold := float64(m.GetFloat("ntp_root_dispersion_threshold"))
	if dispersionThreshold <= 0 {
		dispersionThreshold = 500
	}
	if rootDispersion >= dispersionThreshold {
		return Down("Root dispersion %.3fms exceeds threshold %.0fms", rootDispersion, dispersionThreshold)
	}

	return Up(summary, ping)
}

// queryNTP 发一次 SNTP 请求并返回原始响应与本地收发的 NTP 时间戳（毫秒）。
func queryNTP(ctx context.Context, host string, port int) (resp []byte, t1, t4 float64, err error) {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("DNS lookup failed for %s: %v", host, err)
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", addr.String())
	if err != nil {
		return nil, 0, 0, fmt.Errorf("UDP socket error: %v", err)
	}
	defer conn.Close()

	// LI=0, VN=3, Mode=3(client)
	req := make([]byte, 48)
	req[0] = 0x1b

	t1 = float64(time.Now().UnixMilli() + ntpEpochOffsetMS)
	if _, err := conn.Write(req); err != nil {
		return nil, 0, 0, fmt.Errorf("Failed to send NTP request: %v", err)
	}

	// ctx 已带超时，SetDeadline 只是为了在 ctx 取消时也能立刻退出
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	t4 = float64(time.Now().UnixMilli() + ntpEpochOffsetMS)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil, 0, 0, fmt.Errorf("NTP request timed out")
		}
		return nil, 0, 0, fmt.Errorf("UDP socket error: %v", err)
	}
	if n < 48 {
		return nil, 0, 0, fmt.Errorf("Invalid NTP response: expected 48+ bytes, got %d", n)
	}
	return buf[:n], t1, t4, nil
}

// readNTPTimestamp 读取 64 位 NTP 时间戳（秒 + 小数），返回 NTP 纪元毫秒。
func readNTPTimestamp(buf []byte, off int) float64 {
	sec := binary.BigEndian.Uint32(buf[off : off+4])
	frac := binary.BigEndian.Uint32(buf[off+4 : off+8])
	return float64(sec)*1000 + float64(frac)*1000/0x100000000
}

// trimRefID 把 stratum 0/1 的 ASCII RefID 去掉补零与空白。
func trimRefID(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			continue
		}
		out = append(out, c)
	}
	return string(trimSpaceBytes(out))
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
