package store

import "strings"

// 列名自适应。
//
// 背景：官方 v3 用 redbean 的 freeze 模式，bean 属性名即列名，所以库里
// 同时存在 `upsideDown`（JS 代码写入）和 `upside_down`（老迁移建表）这类
// 双份列；不同来源的库（官方 3.x、官方 2.x、go_server 自建）列名风格并不统一。
//
// 与其维护一张巨大的映射表，不如在运行时按"目标库实际有哪些列"来解析：
// 写入 camelCase 而库里只有 snake_case 时自动改名，反之亦然。
// 这样同一个二进制可以读取三种库，不需要用户做任何迁移。

// resolveColumn 把逻辑字段名解析成表中真实存在的列名；找不到返回 ""。
func resolveColumn(columnSet map[string]bool, key string) string {
	if len(columnSet) == 0 {
		// 拿不到列信息时不改名，直接按原样写入（让数据库报错更早暴露问题）
		return key
	}
	if columnSet[key] {
		return key
	}

	// 保留列名本身就是 snake_case 的情况（如 basic_auth_user）
	if snake := toSnake(key); snake != key && columnSet[snake] {
		return snake
	}
	if camel := toCamel(key); camel != key && columnSet[camel] {
		return camel
	}

	// 特殊别名：语义相同但拼写不同的列
	for _, alt := range columnAliases[key] {
		if columnSet[alt] {
			return alt
		}
	}
	return ""
}

// columnAliases 补充无法通过大小写推导出来的别名。
var columnAliases = map[string][]string{
	"downCount":                 {"down_count"},
	"accepted_statuscodes_json": {"acceptedStatuscodesJson"},
	"http_body_encoding":        {"httpBodyEncoding"},
	"ignore_tls":                {"ignoreTls"},
	"invert_keyword":            {"invertKeyword"},
	"push_token":                {"pushToken"},
	"retry_interval":            {"retryInterval"},
	"resend_interval":           {"resendInterval"},
	"upside_down":               {"upsideDown"},
	"packet_size":               {"packetSize"},
	"save_response":             {"saveResponse"},
	"save_error_response":       {"saveErrorResponse"},
	"response_max_length":       {"responseMaxLength"},
	"dns_resolve_type":          {"dnsResolveType"},
	"dns_resolve_server":        {"dnsResolveServer"},
	"dns_last_result":           {"dnsLastResult"},
	"docker_container":          {"dockerContainer"},
	"docker_host":               {"dockerHost"},
	"tls_ca":                    {"tlsCa"},
	"tls_cert":                  {"tlsCert"},
	"tls_key":                   {"tlsKey"},
	"json_path":                 {"jsonPath"},
	"json_path_operator":        {"jsonPathOperator"},
	"expected_value":            {"expectedValue"},
	"cache_bust":                {"cacheBust"},
	"screenshot_delay":          {"screenshotDelay"},
	"created_date":              {"createdDate"},
	"user_id":                   {"userId"},
	"monitor_id":                {"monitorId"},
	"status_page_id":            {"statusPageId"},
	"maintenance_id":            {"maintenanceId"},
	"notification_id":           {"notificationId"},
	"tag_id":                    {"tagId"},
	"group_id":                  {"groupId"},
	"is_default":                {"isDefault"},
	"expiry_notification":       {"expiryNotification"},
}

// toSnake 把 camelCase 转成 snake_case。
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// toCamel 把 snake_case 转成 camelCase。
func toCamel(s string) string {
	var b strings.Builder
	upperNext := false
	for _, r := range s {
		if r == '_' {
			upperNext = true
			continue
		}
		if upperNext && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 'a' + 'A')
			upperNext = false
			continue
		}
		upperNext = false
		b.WriteRune(r)
	}
	return b.String()
}
