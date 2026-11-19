package checkers

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// presentWord 返回官方一致的措辞：", keyword is found" / ", keyword is not found"。
func presentWord(found bool) string {
	if found {
		return "found"
	}
	return "not found"
}

// certPoolFromPEM 解析自定义 CA 证书。
func certPoolFromPEM(pem []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("tls_ca 中没有可用的证书")
	}
	return pool, nil
}

// asNetError 是 errors.As 的薄封装，避免 http.go 里再引 errors 包。
func asNetError(err error, target *net.Error) bool {
	return errors.As(err, target)
}

// compareJSON 按操作符比较取到的值与期望值。
func compareJSON(actual any, expected, op string) bool {
	if actual == nil {
		return false
	}

	switch strings.ToLower(op) {
	case "contains":
		return strings.Contains(stringify(actual), expected)
	case "startswith":
		return strings.HasPrefix(stringify(actual), expected)
	case "endswith":
		return strings.HasSuffix(stringify(actual), expected)
	}

	// 数值比较优先
	if af, aok := toFloat(actual); aok {
		if ef, err := strconv.ParseFloat(strings.TrimSpace(expected), 64); err == nil {
			switch op {
			case "==", "===":
				return af == ef
			case "!=", "!==":
				return af != ef
			case ">":
				return af > ef
			case ">=":
				return af >= ef
			case "<":
				return af < ef
			case "<=":
				return af <= ef
			}
		}
	}

	as := stringify(actual)
	switch op {
	case "==", "===":
		return as == expected
	case "!=", "!==":
		return as != expected
	case ">":
		return as > expected
	case ">=":
		return as >= expected
	case "<":
		return as < expected
	case "<=":
		return as <= expected
	}
	return false
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// 避免 1 输出成 1.000000
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// ---------------- 通用 HTTP 助手 ----------------

// probeHTTP 供不依赖监控配置的检查器使用（globalping、steam 之外的内置 API 调用）。
var probeHTTP = &http.Client{Timeout: 30 * time.Second}

// httpPostJSON 发 JSON，返回状态码与响应体。
func httpPostJSON(ctx context.Context, target string, payload any, headers map[string]string) (int, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	return httpSend(ctx, http.MethodPost, target, "application/json", raw, headers)
}

// httpGET 发 GET，返回状态码与响应体。
func httpGET(ctx context.Context, target string, headers map[string]string) (int, string, error) {
	return httpSend(ctx, http.MethodGet, target, "", nil, headers)
}

func httpSend(ctx context.Context, method, target, contentType string, body []byte, headers map[string]string) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return 0, "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Uptime-Kuma-Go")
	}
	res, err := probeHTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, string(raw), nil
}
