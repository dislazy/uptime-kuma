package checkers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gosnmp/gosnmp"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// SNMPChecker 用 GET 取一个 OID，再对返回值做 JSON 查询断言。
//
// 官方只支持单 OID（前端也只有一个输入框），v3 固定 noAuthNoPriv——
// 这是官方当前实现的限制，不是这里的简化。
type SNMPChecker struct{}

// Type 实现 Checker。
func (SNMPChecker) Type() string { return "snmp" }

func init() { Register(SNMPChecker{}) }

// Check 实现 Checker。
func (SNMPChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}

	oid := m.Get("snmp_oid")
	if oid == "" {
		oid = "1.3.6.1.2.1.1.3.0"
	}

	port := m.Port()
	if port <= 0 {
		port = 161
	}

	version := m.Get("snmp_version")
	if version == "" {
		version = "2c"
	}

	timeout := Timeout(m)
	retries := int(m.GetInt("maxretries"))

	params := &gosnmp.GoSNMP{
		Target:    host,
		Port:      uint16(port),
		Timeout:   timeout,
		Retries:   retries,
		Transport: "udp",
	}
	_ = ctx // 连接级超时已通过 Timeout 设置，gosnmp 不支持逐包 ctx

	switch version {
	case "1":
		params.Version = gosnmp.Version1
		params.Community = m.Get("radius_password")
	case "3":
		user := m.Get("snmp_v3_username")
		if user == "" {
			return Down("SNMPv3 username is required")
		}
		params.Version = gosnmp.Version3
		params.SecurityModel = gosnmp.UserSecurityModel
		params.MsgFlags = gosnmp.NoAuthNoPriv
		params.SecurityParameters = &gosnmp.UsmSecurityParameters{UserName: user}
	default:
		params.Version = gosnmp.Version2c
		params.Community = m.Get("radius_password")
	}

	if err := params.Connect(); err != nil {
		return Down("%s", snmpError(err))
	}
	defer func() { _ = params.Conn.Close() }()

	packet, err := params.Get([]string{oid})
	if err != nil {
		return Down("%s", snmpError(err))
	}
	if len(packet.Variables) == 0 {
		return Down("No varbinds returned from SNMP session (OID: %s)", oid)
	}

	vb := packet.Variables[0]
	switch vb.Type {
	case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView:
		return Down("The SNMP query returned that no instance exists for OID %s", oid)
	}

	value := snmpValue(vb.Value)

	// 与官方一致：返回值先过 JSON 查询，再做比较
	expected := m.Get("expected_value")
	op := m.Get("json_path_operator")
	if op == "" {
		op = "=="
	}

	actual := any(value)
	if path := m.Get("json_path"); path != "" {
		raw, err := json.Marshal(value)
		if err != nil {
			return Down("Failed to serialize SNMP value: %v", err)
		}
		got, err := evalJSONPath(raw, path)
		if err != nil {
			return Down("%s", err.Error())
		}
		actual = got
	}

	if expected != "" && !compareJSON(actual, expected, op) {
		return Down("JSON query does not pass (comparing %s %s %s)", stringify(actual), op, expected)
	}

	return Up(fmt.Sprintf("JSON query passes (comparing %s %s %s)", stringify(actual), op, expected), 0)
}

// snmpValue 把 gosnmp 的返回值转成 JSON 友好的类型。
func snmpValue(v any) any {
	switch t := v.(type) {
	case []byte:
		s := string(t)
		// OCTET STRING 里如果本身就是 JSON/数字，交给上层继续解析
		return strings.TrimRight(s, "\x00")
	case string, int, int64, uint, uint64, float64, bool:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// snmpError 去掉 gosnmp 包装后的冗余前缀。
func snmpError(err error) string {
	msg := err.Error()
	if msg == "" {
		return "SNMP request failed"
	}
	return msg
}
