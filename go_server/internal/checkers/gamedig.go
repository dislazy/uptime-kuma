package checkers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// GameDigChecker 查询游戏服务器信息。
//
// 官方直接依赖 gamedig 这个支持 200+ 游戏的库；Go 侧没有等价实现，
// 这里覆盖使用量最大的两类协议，其它类型降级为 TCP 连通性判断：
//
//	minecraft  → Java 版 Server List Ping（握手 + status），bedrock 走 RakNet
//	source     → A2S_INFO（CS/TF2/Rust 等 Source 引擎游戏）
type GameDigChecker struct{}

// Type 实现 Checker。
func (GameDigChecker) Type() string { return "gamedig" }

func init() { Register(GameDigChecker{}) }

// Check 实现 Checker。
func (GameDigChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}
	game := strings.ToLower(strings.TrimSpace(m.Get("game")))
	port := m.Port()
	if port <= 0 {
		port = defaultGamePort(game)
	}

	switch {
	case strings.HasPrefix(game, "minecraft") && strings.Contains(game, "bedrock"):
		return queryMinecraftBedrock(ctx, host, port)
	case strings.HasPrefix(game, "minecraft"):
		return queryMinecraft(ctx, host, port)
	case game == "source" || game == "" || isSourceGame(game):
		if r := querySource(ctx, host, port); r.Status == model.StatusUp {
			return r
		}
		return tcpFallback(ctx, host, port)
	default:
		return tcpFallback(ctx, host, port)
	}
}

func defaultGamePort(game string) int {
	switch {
	case strings.HasPrefix(game, "minecraft"):
		if strings.Contains(game, "bedrock") {
			return 19132
		}
		return 25565
	case game == "source", isSourceGame(game):
		return 27015
	default:
		return 1
	}
}

func isSourceGame(game string) bool {
	switch game {
	case "csgo", "cs2", "tf2", "rust", "garrysmod", "l4d2", "dayz", "ark":
		return true
	}
	return false
}

// tcpFallback 只验证端口可连，消息里说明是降级结果。
func tcpFallback(ctx context.Context, host string, port int) Result {
	dialer := &net.Dialer{}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	_ = conn.Close()
	return Up(fmt.Sprintf("TCP connection to %s:%d succeeded (protocol-specific query is not supported for this game)",
		host, port), time.Since(start).Milliseconds())
}

// ---------------- Minecraft Java ----------------

// queryMinecraft 走 Server List Ping：handshake(next state=1) → status request → JSON。
func queryMinecraft(ctx context.Context, host string, port int) Result {
	start := time.Now()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	// handshake
	var hs bytes.Buffer
	writeVarInt(&hs, 0x00)
	writeVarInt(&hs, 764) // 1.20.4 的协议号，-1 也可以但要占 5 字节
	writeMCString(&hs, host)
	_ = binary.Write(&hs, binary.BigEndian, uint16(port))
	writeVarInt(&hs, 1) // next state = status

	var pkt bytes.Buffer
	writeVarInt(&pkt, int32(hs.Len()))
	pkt.Write(hs.Bytes())
	if _, err := conn.Write(pkt.Bytes()); err != nil {
		return Down("%s", err.Error())
	}

	// status request
	if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
		return Down("%s", err.Error())
	}

	reader := bufio.NewReader(conn)
	length, err := readVarInt(reader)
	if err != nil {
		return Down("Failed to read Minecraft status response: %v", err)
	}
	if length <= 0 || length > 1<<21 {
		return Down("Invalid Minecraft status response length: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return Down("Failed to read Minecraft status response: %v", err)
	}

	// body = packetID(varint) + string(varint len + json)
	r := bytes.NewReader(body)
	if _, err := readVarInt(r); err != nil {
		return Down("Invalid Minecraft status response: %v", err)
	}
	jsonStr, err := readMCString(r)
	if err != nil {
		return Down("Invalid Minecraft status response: %v", err)
	}

	var status struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Players struct {
			Online int `json:"online"`
			Max    int `json:"max"`
		} `json:"players"`
		Description json.RawMessage `json:"description"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &status); err != nil {
		return Down("Invalid Minecraft status JSON: %v", err)
	}

	motd := mcDescription(status.Description)
	if motd == "" {
		motd = fmt.Sprintf("Minecraft %s", status.Version.Name)
	}
	msg := fmt.Sprintf("%s (%s, %d/%d players)", motd, status.Version.Name, status.Players.Online, status.Players.Max)
	return Up(msg, time.Since(start).Milliseconds())
}

// mcDescription 把 MOTD（可能是字符串、也可能是有 extra 的聊天组件）拍平成纯文本。
func mcDescription(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Text  string            `json:"text"`
		Extra []json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(obj.Text)
	for _, e := range obj.Extra {
		b.WriteString(mcDescription(e))
	}
	return strings.TrimSpace(b.String())
}

// ---------------- Minecraft Bedrock ----------------

// queryMinecraftBedrock 发 RakNet Unconnected Ping，读回 Unconnected Pong 里的 MOTD。
func queryMinecraftBedrock(ctx context.Context, host string, port int) Result {
	start := time.Now()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	// 0x01 = Unconnected Ping, 时间戳 8 字节, 固定 magic, client GUID 8 字节
	pkt := []byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0}
	pkt = append(pkt, []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe, 0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}...)
	pkt = append(pkt, make([]byte, 8)...)

	if _, err := conn.Write(pkt); err != nil {
		return Down("%s", err.Error())
	}

	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	if n < 35 || buf[0] != 0x1c {
		return Down("Invalid Bedrock pong response")
	}

	// 跳过 0x1c + 8 字节时间戳 + 8 字节 serverID + 16 字节 magic
	payload := string(buf[35:n])
	parts := strings.Split(payload, ";")
	if len(parts) < 2 {
		return Down("Invalid Bedrock pong payload")
	}
	msg := fmt.Sprintf("%s (%s, %s/%s players)", parts[1], parts[0], parts[4], parts[5])
	if len(parts) < 6 {
		msg = fmt.Sprintf("%s (%s)", parts[1], parts[0])
	}
	return Up(msg, time.Since(start).Milliseconds())
}

// ---------------- Source (A2S_INFO) ----------------

// querySource 发 A2S_INFO；服务器要求挑战时用返回的 challenge 重发一次。
func querySource(ctx context.Context, host string, port int) Result {
	start := time.Now()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req := append([]byte{0xff, 0xff, 0xff, 0xff, 0x54}, []byte("Source Engine Query\x00")...)
	if _, err := conn.Write(req); err != nil {
		return Down("%s", err.Error())
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	if n < 6 || buf[0] != 0xff || buf[4] != 0x49 {
		if n >= 5 && buf[4] == 0x41 {
			// 0x41 = challenge，带上 4 字节 challenge 再问一次
			challenge := buf[5:9]
			req2 := append([]byte{0xff, 0xff, 0xff, 0xff, 0x54}, []byte("Source Engine Query\x00")...)
			req2 = append(req2, challenge...)
			if _, err := conn.Write(req2); err != nil {
				return Down("%s", err.Error())
			}
			n, err = conn.Read(buf)
			if err != nil {
				return Down("%s", cleanDialError(err))
			}
		}
		if n < 6 || buf[4] != 0x49 {
			return Down("Unexpected A2S_INFO response")
		}
	}

	name := readCString(buf[6:])
	return Up(name, time.Since(start).Milliseconds())
}

// readCString 读以 0 结尾的字符串。
func readCString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// ---------------- Minecraft varint 工具 ----------------

func writeVarInt(w io.ByteWriter, v int32) {
	u := uint32(v)
	for {
		b := byte(u & 0x7f)
		u >>= 7
		if u != 0 {
			b |= 0x80
		}
		_ = w.WriteByte(b)
		if u == 0 {
			return
		}
	}
}

func writeMCString(w *bytes.Buffer, s string) {
	writeVarInt(w, int32(len(s)))
	w.WriteString(s)
}

func readVarInt(r io.ByteReader) (int32, error) {
	var (
		value uint32
		shift uint
	)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 35 {
			return 0, fmt.Errorf("varint too long")
		}
	}
	return int32(value), nil
}

func readMCString(r *bytes.Reader) (string, error) {
	n, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if n < 0 || int(n) > r.Len() {
		return "", fmt.Errorf("invalid string length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
