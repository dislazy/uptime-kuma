package checkers

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/conditions"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// DNSChecker 解析域名并按记录内容判定。
type DNSChecker struct{}

// Type 实现 Checker。
func (DNSChecker) Type() string { return "dns" }

func init() { Register(DNSChecker{}) }

// dnsLookupResult 是一次解析的结构化结果。
type dnsLookupResult struct {
	// values 是"记录值"列表，供条件表达式逐条匹配（对应官方 some(record => ...)）
	values []string
	// message 是写进 dns_last_result 与心跳的展示文案，格式与官方一致
	message string
}

// Check 按配置的记录类型做一次解析。
func (DNSChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := strings.TrimSpace(m.Hostname())
	if host == "" {
		// 兼容把域名填在 url 里的旧配置
		host = strings.TrimSpace(m.URL())
	}
	if host == "" {
		return Down("Domain is empty")
	}

	recordType := strings.ToUpper(strings.TrimSpace(m.Get("dns_resolve_type")))
	if recordType == "" {
		recordType = "A"
	}

	resolver := buildResolver(m.Get("dns_resolve_server"))
	start := time.Now()

	res, err := performLookup(ctx, resolver, recordType, host)
	ping := time.Since(start).Milliseconds()
	if err != nil {
		return Result{Status: model.StatusDown, Msg: err.Error(), Ping: &ping}
	}

	// 条件优先：官方一旦配了 conditions 就不再单独校验 expected_value
	if rawCond := m.Get("conditions"); conditions.Has(rawCond) {
		ok, err := matchAnyRecord(rawCond, res.values)
		if err != nil {
			return Result{Status: model.StatusDown, Msg: err.Error(), Ping: &ping, DNSResult: res.message}
		}
		if !ok {
			// 官方失败时直接把记录文案当作错误信息抛出
			return Result{Status: model.StatusDown, Msg: res.message, Ping: &ping, DNSResult: res.message}
		}
		return Result{Status: model.StatusUp, Msg: res.message, Ping: &ping, DNSResult: res.message}
	}

	// 未配条件时保留 expected_value 的包含匹配，兼容早期配置
	if expected := strings.TrimSpace(m.Get("expected_value")); expected != "" {
		if !strings.Contains(res.message, expected) {
			return Result{
				Status:    model.StatusDown,
				Msg:       fmt.Sprintf("%s (expected %s)", res.message, expected),
				Ping:      &ping,
				DNSResult: res.message,
			}
		}
	}

	return Result{Status: model.StatusUp, Msg: res.message, Ping: &ping, DNSResult: res.message}
}

// matchAnyRecord 对每条记录求一次条件，任意一条成立即通过。
func matchAnyRecord(rawCond string, values []string) (bool, error) {
	for _, v := range values {
		ok, err := conditions.Eval(rawCond, map[string]any{"record": v})
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// buildResolver 构造带自定义上游的解析器。
func buildResolver(server string) *net.Resolver {
	server = strings.TrimSpace(server)
	if server == "" {
		return net.DefaultResolver
	}
	// 允许逗号分隔多个上游，取第一个即可满足连通性判断
	if i := strings.Index(server, ","); i >= 0 {
		server = strings.TrimSpace(server[:i])
	}
	// 允许带端口 "8.8.8.8:53"，不带则补 53
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}

	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", server)
		},
	}
}

// performLookup 按记录类型解析，并产出官方格式的展示文案。
func performLookup(ctx context.Context, r *net.Resolver, recordType, host string) (dnsLookupResult, error) {
	switch recordType {
	case "A", "AAAA":
		addrs, err := r.LookupHost(ctx, host)
		if err != nil {
			return dnsLookupResult{}, err
		}

		wantV4 := recordType == "A"
		var out []string
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip == nil {
				continue
			}
			if (ip.To4() != nil) == wantV4 {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return dnsLookupResult{}, fmt.Errorf("no %s record found", recordType)
		}
		return dnsLookupResult{values: out, message: "Records: " + strings.Join(out, " | ")}, nil

	case "CNAME":
		cname, err := r.LookupCNAME(ctx, host)
		if err != nil {
			return dnsLookupResult{}, err
		}
		// 官方 CNAME 的消息就是记录值本身，没有前缀
		return dnsLookupResult{values: []string{cname}, message: cname}, nil

	case "MX":
		mxs, err := r.LookupMX(ctx, host)
		if err != nil {
			return dnsLookupResult{}, err
		}
		values := make([]string, 0, len(mxs))
		parts := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			values = append(values, mx.Host)
			parts = append(parts, fmt.Sprintf("Hostname: %s - Priority: %d", mx.Host, mx.Pref))
		}
		return dnsLookupResult{values: values, message: strings.Join(parts, " | ")}, nil

	case "TXT":
		txts, err := r.LookupTXT(ctx, host)
		if err != nil {
			return dnsLookupResult{}, err
		}
		return dnsLookupResult{values: txts, message: "Records: " + strings.Join(txts, " | ")}, nil

	case "NS":
		nss, err := r.LookupNS(ctx, host)
		if err != nil {
			return dnsLookupResult{}, err
		}
		values := make([]string, 0, len(nss))
		for _, ns := range nss {
			values = append(values, ns.Host)
		}
		return dnsLookupResult{values: values, message: "Servers: " + strings.Join(values, " | ")}, nil

	case "PTR":
		names, err := r.LookupAddr(ctx, host)
		if err != nil {
			return dnsLookupResult{}, err
		}
		return dnsLookupResult{values: names, message: "Records: " + strings.Join(names, " | ")}, nil

	case "SRV":
		_, srvs, err := r.LookupSRV(ctx, "", "", host)
		if err != nil {
			return dnsLookupResult{}, err
		}
		values := make([]string, 0, len(srvs))
		parts := make([]string, 0, len(srvs))
		for _, s := range srvs {
			values = append(values, s.Target)
			parts = append(parts, fmt.Sprintf("Name: %s | Port: %d | Priority: %d | Weight: %d",
				s.Target, s.Port, s.Priority, s.Weight))
		}
		return dnsLookupResult{values: values, message: strings.Join(parts, " | ")}, nil

	default:
		// SOA / CAA 需要完整 DNS 报文，等 P2 引入 miekg/dns 后再补
		return dnsLookupResult{}, fmt.Errorf("record type %s is not supported yet", recordType)
	}
}
