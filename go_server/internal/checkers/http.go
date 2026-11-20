package checkers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// HTTPChecker 处理 http / keyword / json-query 三种类型，
// 它们的差异只在于额外的断言条件。
type HTTPChecker struct{}

// Type 实现 Checker。
func (HTTPChecker) Type() string { return "http" }

// 一次探测最多读取的响应体大小，避免被超大页面拖垮内存。
const maxBodyRead = 1 << 20 // 1 MiB

func init() {
	c := HTTPChecker{}
	Register(c)
	Register(aliasChecker{base: c, name: "keyword"})
	Register(aliasChecker{base: c, name: "json-query"})
}

// aliasChecker 让同一份实现注册到多个类型名下。
type aliasChecker struct {
	base Checker
	name string
}

func (a aliasChecker) Type() string { return a.name }
func (a aliasChecker) Check(ctx context.Context, m *model.Monitor) Result {
	return a.base.Check(ctx, m)
}

// Check 执行一次 HTTP 探测。
func (HTTPChecker) Check(ctx context.Context, m *model.Monitor) Result {
	target := strings.TrimSpace(m.URL())
	if target == "" {
		return Down("URL is empty")
	}

	client, err := buildClient(m)
	if err != nil {
		return Down("Invalid TLS config: %s", err)
	}

	method := strings.ToUpper(strings.TrimSpace(m.Get("method")))
	if method == "" {
		method = "GET"
	}

	// cache_bust：每次带随机参数绕过 CDN 缓存
	if m.GetBool("cache_bust") {
		target = withCacheBust(target)
	}

	var bodyReader io.Reader
	if body := m.Get("body"); body != "" && method != "GET" && method != "HEAD" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return Down("Invalid URL: %s", err)
	}

	applyHeaders(req, m)
	applyAuth(req, m)

	start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return Down("%s", cleanHTTPError(err))
	}
	defer res.Body.Close()

	// 响应体按需读取：只有需要做 keyword / json 断言时才读
	needBody := m.Type == "keyword" || m.Type == "json-query" ||
		m.Get("keyword") != "" || m.Get("json_path") != ""

	var body []byte
	if needBody {
		body, _ = io.ReadAll(io.LimitReader(res.Body, maxBodyRead))
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	}

	ping := time.Since(start).Milliseconds()
	baseMsg := fmt.Sprintf("%d - %s", res.StatusCode, http.StatusText(res.StatusCode))

	// 1) 状态码
	if !statusAccepted(res.StatusCode, m.Get("accepted_statuscodes_json")) {
		return Result{Status: model.StatusDown, Msg: baseMsg, Ping: &ping}
	}

	// 2) keyword 断言
	if keyword := m.Get("keyword"); keyword != "" {
		contains := strings.Contains(string(body), keyword)
		matched := contains
		if m.GetBool("invert_keyword") {
			matched = !contains
		}
		if !matched {
			return Result{
				Status: model.StatusDown,
				Msg:    baseMsg + ", keyword is " + presentWord(contains),
				Ping:   &ping,
			}
		}
		baseMsg += ", keyword is " + presentWord(contains)
	}

	// 3) JSON 断言
	if path := m.Get("json_path"); path != "" {
		actual, err := evalJSONPath(body, path)
		if err != nil {
			return Result{Status: model.StatusDown, Msg: baseMsg + ", " + err.Error(), Ping: &ping}
		}
		expected := m.Get("expected_value")
		op := m.Get("json_path_operator")
		if op == "" {
			op = "=="
		}
		if !compareJSON(actual, expected, op) {
			return Result{
				Status: model.StatusDown,
				Msg:    fmt.Sprintf("%s, JSON query failed (comparing %v %s %s)", baseMsg, actual, op, expected),
				Ping:   &ping,
			}
		}
		baseMsg += fmt.Sprintf(", JSON query passes (comparing %v %s %s)", actual, op, expected)
	}

	return Result{Status: model.StatusUp, Msg: baseMsg, Ping: &ping}
}

func buildClient(m *model.Monitor) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: m.GetBool("ignore_tls"), //nolint:gosec // 用户显式选择忽略证书
	}

	// 自定义 CA / 客户端证书
	if ca := m.Get("tls_ca"); ca != "" {
		pool, err := certPoolFromPEM([]byte(ca))
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}
	if cert := m.Get("tls_cert"); cert != "" {
		key := m.Get("tls_key")
		pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			return nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	maxRedirects := int(m.GetInt("maxredirects"))
	if m.Get("maxredirects") == "" {
		maxRedirects = 10
	}

	return &http.Client{
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 0,
			MaxIdleConns:          32,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if maxRedirects == 0 {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}, nil
}

func applyHeaders(req *http.Request, m *model.Monitor) {
	raw := m.Get("headers")
	if raw == "" {
		return
	}

	var hdrs map[string]string
	if err := json.Unmarshal([]byte(raw), &hdrs); err != nil {
		return
	}
	for k, v := range hdrs {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}

	// 没显式设置 Content-Type 时，按 http_body_encoding 推断
	if req.Header.Get("Content-Type") == "" {
		switch strings.ToLower(m.Get("http_body_encoding")) {
		case "json":
			req.Header.Set("Content-Type", "application/json")
		case "xml", "form":
			req.Header.Set("Content-Type", "text/xml")
		}
	}
}

func applyAuth(req *http.Request, m *model.Monitor) {
	user := m.Get("basic_auth_user")
	pass := m.Get("basic_auth_pass")
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
}

// statusAccepted 判断状态码是否落在可接受范围内。
// 官方格式：["200-299"] 或 ["200","301"]。
func statusAccepted(code int, spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = `["200-299"]`
	}

	var list []string
	if err := json.Unmarshal([]byte(spec), &list); err != nil {
		return code >= 200 && code < 300
	}

	for _, item := range list {
		item = strings.TrimSpace(item)
		if i := strings.IndexByte(item, '-'); i > 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(item[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(item[i+1:]))
			if err1 == nil && err2 == nil && code >= lo && code <= hi {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(item); err == nil && n == code {
			return true
		}
	}
	return false
}

func withCacheBust(target string) string {
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + "_=" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// cleanHTTPError 把 Go 的冗长错误压成一句人话。
func cleanHTTPError(err error) string {
	msg := err.Error()

	var netErr net.Error
	if ok := asNetError(err, &netErr); ok && netErr.Timeout() {
		return "Timeout"
	}
	if strings.Contains(msg, "no such host") {
		return "ENOTFOUND"
	}
	if strings.Contains(msg, "connection refused") {
		return "ECONNREFUSED"
	}
	if strings.Contains(msg, "certificate") {
		return "TLS error: " + msg
	}
	return msg
}
