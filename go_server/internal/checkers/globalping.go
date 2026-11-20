package checkers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// GlobalpingChecker 调 globalping.io 的公共探测网络做 ping / http / dns。
//
// 与官方一致：先在 /v1/measurements 创建测量，再轮询同一个资源直到 finished。
// 有 API Token 时带上 Bearer，能拿到更高的配额。
type GlobalpingChecker struct{}

// Type 实现 Checker。
func (GlobalpingChecker) Type() string { return "globalping" }

func init() { Register(GlobalpingChecker{}) }

const globalpingAPI = "https://api.globalping.io/v1"

// Check 实现 Checker。
func (GlobalpingChecker) Check(ctx context.Context, m *model.Monitor) Result {
	subtype := strings.ToLower(strings.TrimSpace(m.Get("subtype")))
	if subtype == "" {
		subtype = "ping"
	}

	opts := map[string]any{
		"inProgressUpdates": false,
		"limit":             1,
		"locations":         []map[string]any{{"magic": m.Get("location")}},
	}

	switch subtype {
	case "ping":
		return globalpingPing(ctx, m, opts)
	case "http":
		return globalpingHTTP(ctx, m, opts)
	case "dns":
		return globalpingDNS(ctx, m, opts)
	default:
		return Down("Unknown Globalping subtype: %s", subtype)
	}
}

func globalpingPing(ctx context.Context, m *model.Monitor, opts map[string]any) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}

	mo := map[string]any{
		"packets":  m.GetInt("ping_count"),
		"protocol": defaultStr(m.Get("protocol"), "ICMP"),
	}
	if mo["packets"] == int64(0) {
		mo["packets"] = 3
	}
	if mo["protocol"] == "TCP" && m.Port() > 0 {
		mo["port"] = m.Port()
	}
	applyIPFamily(mo, m.Get("ip_family"))

	opts["type"] = "ping"
	opts["target"] = host
	opts["measurementOptions"] = mo

	res, err := globalpingMeasure(ctx, opts)
	if err != nil {
		return Down("%s", err.Error())
	}

	if res.Result.Status == "failed" {
		return Down("%s", globalpingFormat(res.Probe, "Failed: "+res.Result.RawOutput))
	}
	if res.Result.Timings.Total == 0 && res.Result.Stats.Avg == 0 {
		return Down("%s", globalpingFormat(res.Probe, "Failed: "+res.Result.RawOutput))
	}

	return Up(globalpingFormat(res.Probe, "OK"), int64(res.Result.Stats.Avg))
}

func globalpingHTTP(ctx context.Context, m *model.Monitor, opts map[string]any) Result {
	target := m.URL()
	if target == "" {
		return Down("URL is required")
	}
	u, err := url.Parse(target)
	if err != nil {
		return Down("Invalid URL: %v", err)
	}

	protocol := strings.ToUpper(strings.TrimSuffix(u.Scheme, ":"))
	if m.Get("protocol") == "HTTP2" {
		protocol = "HTTP2"
	}

	hdrs := map[string]string{}
	if raw := m.Get("headers"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &hdrs)
	}
	if user := m.Get("basic_auth_user"); user != "" || m.Get("basic_auth_pass") != "" {
		hdrs["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+m.Get("basic_auth_pass")))
	} else if token := m.Get("bearer_token"); token != "" {
		hdrs["Authorization"] = "Bearer " + token
	}

	req := map[string]any{
		"host":    u.Hostname(),
		"path":    u.Path,
		"method":  defaultStr(m.Get("method"), "GET"),
		"headers": hdrs,
	}
	if u.RawQuery != "" {
		req["query"] = u.RawQuery
	}

	mo := map[string]any{"request": req, "protocol": protocol}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			mo["port"] = n
		}
	}
	applyIPFamily(mo, m.Get("ip_family"))
	if r := m.Get("dns_resolve_server"); r != "" {
		mo["resolver"] = r
	}

	opts["type"] = "http"
	opts["target"] = u.Hostname()
	opts["measurementOptions"] = mo

	res, err := globalpingMeasure(ctx, opts)
	if err != nil {
		return Down("%s", err.Error())
	}
	if res.Result.Status == "failed" {
		return Down("%s", globalpingFormat(res.Probe, "Failed: "+res.Result.RawOutput))
	}

	ping := int64(res.Result.Timings.Total)
	if !statusAccepted(res.Result.StatusCode, m.Get("accepted_statuscodes_json")) {
		return Down("%s", globalpingFormat(res.Probe,
			fmt.Sprintf("Status code %d not accepted. Output: %s", res.Result.StatusCode, res.Result.RawOutput)))
	}

	base := globalpingFormat(res.Probe, fmt.Sprintf("%d - %s", res.Result.StatusCode, res.Result.StatusCodeName))

	if kw := m.Get("keyword"); kw != "" {
		found := strings.Contains(res.Result.RawOutput, kw)
		if found == m.GetBool("invert_keyword") {
			shown := gpStripTags(res.Result.RawOutput)
			if len(shown) > 50 {
				shown = shown[:47] + "..."
			}
			return Down("%s, but keyword is %s in [%s]", base, presentWord(found), shown)
		}
		return Up(base+", keyword "+presentWord(found)+" found", ping)
	}

	if path := m.Get("json_path"); path != "" {
		actual, err := evalJSONPath([]byte(res.Result.RawOutput), path)
		if err != nil {
			return Down("%s", base+", "+err.Error())
		}
		expected := m.Get("expected_value")
		op := defaultStr(m.Get("json_path_operator"), "==")
		if !compareJSON(actual, expected, op) {
			return Down("%s", globalpingFormat(res.Probe,
				fmt.Sprintf("JSON query does not pass (comparing %v %s %s)", actual, op, expected)))
		}
		return Up(globalpingFormat(res.Probe,
			fmt.Sprintf("JSON query passes (comparing %v %s %s)", actual, op, expected)), ping)
	}

	return Up(globalpingFormat(res.Probe, "OK"), ping)
}

func globalpingDNS(ctx context.Context, m *model.Monitor, opts map[string]any) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}

	mo := map[string]any{
		"query":    map[string]any{"type": defaultStr(m.Get("dns_resolve_type"), "A")},
		"protocol": defaultStr(m.Get("protocol"), "UDP"),
	}
	if m.Port() > 0 {
		mo["port"] = m.Port()
	}
	applyIPFamily(mo, m.Get("ip_family"))
	if r := m.Get("dns_resolve_server"); r != "" {
		mo["resolver"] = r
	}

	opts["type"] = "dns"
	opts["target"] = host
	opts["measurementOptions"] = mo

	res, err := globalpingMeasure(ctx, opts)
	if err != nil {
		return Down("%s", err.Error())
	}
	if res.Result.Status == "failed" {
		return Down("%s", globalpingFormat(res.Probe, "Failed: "+res.Result.RawOutput))
	}

	values := make([]string, 0, len(res.Result.Answers))
	for _, a := range res.Result.Answers {
		values = append(values, a.Value)
	}
	msg := strings.Join(values, " | ")
	ping := int64(res.Result.Timings.Total)

	if msg == "" {
		msg = "No records found. " + res.Result.StatusCodeName
	}

	// dns_last_result 由上层写回，这里通过 DNSResult 字段带出去
	out := Result{DNSResult: msg}

	if kw := m.Get("keyword"); kw != "" {
		re, err := regexp.Compile("(?i)" + kw)
		if err != nil {
			return Down("Invalid keyword regex: %v", err)
		}
		matched := false
		for _, v := range values {
			if re.MatchString(v) {
				matched = true
				break
			}
		}
		if !matched {
			return Down("%s", globalpingFormat(res.Probe, "No record matched. "+msg))
		}
	}

	out.Status = model.StatusUp
	out.Msg = globalpingFormat(res.Probe, msg)
	out.Ping = &ping
	return out
}

// ---------------- API 调用 ----------------

type globalpingProbe struct {
	City      string   `json:"city"`
	State     string   `json:"state"`
	Country   string   `json:"country"`
	Continent string   `json:"continent"`
	Network   string   `json:"network"`
	ASN       int      `json:"asn"`
	Tags      []string `json:"tags"`
}

type globalpingResult struct {
	Probe  globalpingProbe `json:"probe"`
	Result struct {
		Status         string `json:"status"`
		RawOutput      string `json:"rawOutput"`
		StatusCode     int    `json:"statusCode"`
		StatusCodeName string `json:"statusCodeName"`
		Stats          struct {
			Avg float64 `json:"avg"`
		} `json:"stats"`
		Timings struct {
			Total float64 `json:"total"`
		} `json:"timings"`
		Answers []struct {
			Value string `json:"value"`
		} `json:"answers"`
	} `json:"result"`
}

// globalpingMeasure 创建测量并轮询到结束。
func globalpingMeasure(ctx context.Context, opts map[string]any) (*globalpingResult, error) {
	apiKey := Setting(ctx, "globalpingApiToken", "")
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}

	code, body, err := httpPostJSON(ctx, globalpingAPI+"/measurements", opts, headers)
	if err != nil {
		return nil, err
	}
	// 官方在 500 时会重试一次（探测节点偶发故障）
	if code == 500 {
		code, body, err = httpPostJSON(ctx, globalpingAPI+"/measurements", opts, headers)
		if err != nil {
			return nil, err
		}
	}
	if code == 429 {
		if apiKey == "" {
			return nil, fmt.Errorf("Failed to create measurement: rate limit exceeded. Without an API token the limit is lower, please configure a Globalping API token in Settings")
		}
		return nil, fmt.Errorf("Failed to create measurement: rate limit exceeded, please try again later")
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("Failed to create measurement: %s", globalpingAPIError(body, code))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
		return nil, fmt.Errorf("Failed to create measurement: invalid response")
	}

	// 轮询直到 finished；ctx 超时由调用方的探测超时兜底
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("Failed to fetch measurement (%s): timeout", created.ID)
		case <-time.After(300 * time.Millisecond):
		}

		code, body, err := httpGET(ctx, globalpingAPI+"/measurements/"+created.ID, headers)
		if err != nil {
			return nil, err
		}
		if code == 404 {
			continue // 还在排队
		}
		if code < 200 || code >= 300 {
			return nil, fmt.Errorf("Failed to fetch measurement (%s): %s", created.ID, globalpingAPIError(body, code))
		}

		var full struct {
			Status  string             `json:"status"`
			Results []globalpingResult `json:"results"`
			Error   json.RawMessage    `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &full); err != nil {
			return nil, fmt.Errorf("Failed to fetch measurement (%s): invalid response", created.ID)
		}
		if full.Status == "finished" {
			if len(full.Results) == 0 {
				return nil, fmt.Errorf("Failed to fetch measurement (%s): no results", created.ID)
			}
			return &full.Results[0], nil
		}
	}
}

// globalpingFormat 生成 "城市, 国家, 洲, 网络 (ASxxxx) : 文本"。
func globalpingFormat(p globalpingProbe, text string) string {
	loc := p.City
	if p.State != "" {
		loc += " (" + p.State + ")"
	}
	loc += fmt.Sprintf(", %s, %s, %s (AS%d)", p.Country, p.Continent, p.Network, p.ASN)

	for _, t := range p.Tags {
		if t == "" {
			continue
		}
		if last := t[len(t)-1]; last >= '0' && last <= '9' {
			loc += ", (" + t + ")"
			break
		}
	}
	return loc + " : " + text
}

// globalpingAPIError 从 {"error":{"message":"..."}} 里取错误文案。
func globalpingAPIError(body string, code int) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	if s := strings.TrimSpace(body); s != "" {
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		return s
	}
	return fmt.Sprintf("HTTP %d", code)
}

func applyIPFamily(mo map[string]any, family string) {
	switch family {
	case "ipv4":
		mo["ipVersion"] = 4
	case "ipv6":
		mo["ipVersion"] = 6
	}
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// gpStripTags 去掉 HTML 标签与多余空白，用于把 keyword 未命中的响应体压成一行展示。
func gpStripTags(s string) string {
	s = regexp.MustCompile(`<[^>]*>?`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`[\n\r]+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
