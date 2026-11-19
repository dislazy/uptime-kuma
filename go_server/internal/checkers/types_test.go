package checkers

import (
	"sort"
	"testing"
)

// frontendMonitorTypes 是 src/pages/EditMonitor.vue 里「监控类型」下拉框的全量取值。
//
// 这份清单是护栏：官方每加一个类型，前端下拉就会多一项，而后端如果忘了注册，
// 用户选完保存后监控永远不会被调度（前端不报错，只是 status 一直是 pending）。
var frontendMonitorTypes = []string{
	"dns", "docker", "gamedig", "globalping", "group", "grpc-keyword", "http",
	"json-query", "kafka-producer", "keyword", "manual", "mongodb", "mqtt", "mysql",
	"ntp", "oracledb", "ping", "pm2", "port", "postgres", "push", "rabbitmq",
	"radius", "real-browser", "redis", "sftp", "sip-options", "smtp", "snmp",
	"sqlserver", "steam", "system-service", "tailscale-ping", "websocket-upgrade",
}

// passiveMonitorTypes 不走 checkers 注册表，由 monitor.skipCheck 直接跳过。
var passiveMonitorTypes = map[string]bool{
	"group":  true,
	"manual": true,
	"push":   true,
}

func TestFrontendTypesRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, typ := range Types() {
		registered[typ] = true
	}

	var missing []string
	for _, typ := range frontendMonitorTypes {
		if passiveMonitorTypes[typ] {
			continue
		}
		if !registered[typ] {
			missing = append(missing, typ)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("以下前端可选类型没有注册 checker：%v（已注册：%v）", missing, Types())
	}
}

// TestPassiveTypesNotRegistered 反向校验：被动类型不应该出现在注册表里，
// 否则 skipCheck 会把它们当真去探测。
func TestPassiveTypesNotRegistered(t *testing.T) {
	for _, typ := range Types() {
		if passiveMonitorTypes[typ] {
			t.Fatalf("%q 是被动类型，不应注册 checker", typ)
		}
	}
}
