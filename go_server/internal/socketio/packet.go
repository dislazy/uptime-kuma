// Package socketio 实现 Engine.IO v4 + Socket.IO v4 服务端协议。
//
// 为什么自己写而不直接引第三方库：
//  1. Uptime Kuma 前端只用到默认命名空间、JSON 事件、ack 回调三件事，
//     完整协议里一大半能力（二进制附件、多命名空间、房间广播）我们用不上；
//  2. 自己实现可以精确控制 ping/pong 与背压策略，符合"低配机器"的定位；
//  3. 少一个依赖，二进制更小、升级不受制于人。
//
// 协议要点（EIO=4）：
//
//	Engine 包类型：0 open 1 close 2 ping 3 pong 4 message 5 upgrade 6 noop
//	Socket.IO 包类型：0 CONNECT 1 DISCONNECT 2 EVENT 3 ACK 4 CONNECT_ERROR
//	事件帧：42["eventName",arg1,arg2]
//	带 ack id：421["eventName",...]  → 服务端回 431[...]
//	polling 多包用 0x1E (Record Separator) 分隔
package socketio

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Engine.IO 包类型
const (
	engineOpen    = '0'
	engineClose   = '1'
	enginePing    = '2'
	enginePong    = '3'
	engineMessage = '4'
	engineUpgrade = '5'
	engineNoop    = '6'
)

// Socket.IO 包类型
const (
	sioConnect      = '0'
	sioDisconnect   = '1'
	sioEvent        = '2'
	sioAck          = '3'
	sioConnectError = '4'
)

// recordSeparator 是 EIO4 polling 的包分隔符。
const recordSeparator = "\x1e"

// Packet 是解析后的 Socket.IO 包。
type Packet struct {
	Type   byte   // Socket.IO 类型
	Event  string // 仅 EVENT 有效
	Args   []json.RawMessage
	AckID  int64 // -1 表示客户端没带 ack 回调
	HasAck bool
	Raw    json.RawMessage // CONNECT 的负载等
}

var errMalformed = errors.New("socket.io 包格式错误")

// parseSocketPacket 解析 Socket.IO 层负载（不含最前面的 engine '4'）。
func parseSocketPacket(s string) (*Packet, error) {
	if len(s) == 0 {
		return nil, errMalformed
	}

	p := &Packet{Type: s[0], AckID: -1}
	rest := s[1:]

	// 二进制附件数：形如 "2-"，本项目不使用二进制帧，有就跳过
	for i := 0; i < len(rest); i++ {
		if rest[i] == '-' && i > 0 && isDigits(rest[:i]) {
			rest = rest[i+1:]
			break
		}
		if !isDigit(rest[i]) {
			break
		}
	}

	// 命名空间：形如 "/admin,"，默认命名空间会省略
	if strings.HasPrefix(rest, "/") {
		idx := strings.IndexByte(rest, ',')
		if idx < 0 {
			return nil, errMalformed
		}
		rest = rest[idx+1:]
	}

	// ack id
	i := 0
	for i < len(rest) && isDigit(rest[i]) {
		i++
	}
	if i > 0 {
		id, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil {
			return nil, errMalformed
		}
		p.AckID = id
		p.HasAck = true
		rest = rest[i:]
	}

	if rest == "" {
		return p, nil
	}

	raw := json.RawMessage(rest)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: 负载不是合法 JSON", errMalformed)
	}
	p.Raw = raw

	if p.Type == sioEvent || p.Type == sioAck {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("%w: 事件不是数组", errMalformed)
		}
		if len(arr) > 0 {
			_ = json.Unmarshal(arr[0], &p.Event)
			p.Args = arr[1:]
		}
	}
	return p, nil
}

// encodeConnect 生成 CONNECT 应答。
//
// 必须是 Engine.IO message(4) + Socket.IO connect(0)，即 "40{...}"。
// 少写前面那个 '4'，客户端会把它当成 Engine.IO 的 open 包重新握手，
// 而 payload 里只有 sid 没有 upgrades —— engine.io-client 的 _filterUpgrades
// 直接抛 TypeError 崩掉（实测 v6 崩在 socket.js:690）。
func encodeConnect(sid string) string {
	b, _ := json.Marshal(map[string]string{"sid": sid})
	return string(engineMessage) + string(sioConnect) + string(b)
}

// encodeOpen 生成 Engine.IO open 包。
//
// upgrades 必须按握手的传输方式区分：
//   - polling 握手 → ["websocket"]，告诉客户端还能升级
//   - 直接 websocket 握手 → []，此时已经到顶了
//
// 直接 websocket 时如果还给 ["websocket"]，engine.io-client 的 _filterUpgrades
// 会去读它的 length 之外的字段并抛 TypeError 直接崩掉（实测 v6 必崩）。
func encodeOpen(sid string, upgrades []string, pingIntervalMS, pingTimeoutMS, maxPayload int) string {
	if upgrades == nil {
		upgrades = []string{}
	}
	b, _ := json.Marshal(struct {
		SID          string   `json:"sid"`
		Upgrades     []string `json:"upgrades"`
		PingInterval int      `json:"pingInterval"`
		PingTimeout  int      `json:"pingTimeout"`
		MaxPayload   int      `json:"maxPayload"`
	}{sid, upgrades, pingIntervalMS, pingTimeoutMS, maxPayload})

	return string(engineOpen) + string(b)
}

// encodeEvent 生成事件包。
func encodeEvent(event string, args []any) (string, error) {
	arr := make([]any, 0, len(args)+1)
	arr = append(arr, event)
	arr = append(arr, args...)

	b, err := json.Marshal(arr)
	if err != nil {
		return "", err
	}
	return string(engineMessage) + string(sioEvent) + string(b), nil
}

// encodeAck 生成 ack 应答包。
func encodeAck(ackID int64, data any) (string, error) {
	arr := []any{data}
	b, err := json.Marshal(arr)
	if err != nil {
		return "", err
	}
	return string(engineMessage) + string(sioAck) + strconv.FormatInt(ackID, 10) + string(b), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}
