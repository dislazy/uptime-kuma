package checkers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// RabbitMQChecker 走 Management API 的健康检查端点判断集群是否有告警。
//
// 注意这不是 AMQP 协议探测：官方用的是 HTTP 的 /api/health/checks/alarms/，
// 它同时反映"节点在不在"和"集群有没有 alarms"，比单纯建 AMQP 连接更有意义。
type RabbitMQChecker struct{}

// Type 实现 Checker。
func (RabbitMQChecker) Type() string { return "rabbitmq" }

func init() { Register(RabbitMQChecker{}) }

// Check 依次尝试配置的每个节点，任意一个健康即视为 UP。
func (RabbitMQChecker) Check(ctx context.Context, m *model.Monitor) Result {
	nodes, err := parseRabbitNodes(m.Get("rabbitmq_nodes"))
	if err != nil {
		return Down("%s", err.Error())
	}
	if len(nodes) == 0 {
		return Down("No RabbitMQ nodes configured")
	}

	username := m.Get("rabbitmq_username")
	password := m.Get("rabbitmq_password")

	client := &http.Client{Timeout: 15 * time.Second}

	var (
		errors   []string
		lastErr  error
		fastFail = false
	)
	_ = fastFail

	for i, node := range nodes {
		start := time.Now()
		reason, err := rabbitNodeHealth(ctx, client, node, username, password)
		ping := time.Since(start).Milliseconds()

		if err == nil {
			if len(nodes) == 1 {
				return Up("Node is reachable and there are no alerts in the cluster", ping)
			}
			return Up(fmt.Sprintf("One of the %d nodes is reachable and there are no alerts in the cluster", len(nodes)), ping)
		}

		lastErr = err
		errors = append(errors, fmt.Sprintf("Node %d: %s", i+1, err.Error()))
		_ = reason
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("all nodes unreachable")
	}
	return Down("All %d nodes failed because %s", len(errors), strings.Join(errors, "; "))
}

// rabbitNodeHealth 请求单个节点的健康检查端点。
// 返回 (告警原因, 错误)：200 视为健康，503 视为有告警。
func rabbitNodeHealth(ctx context.Context, client *http.Client, base, username, password string) (string, error) {
	// 不带结尾斜杠时，URL 拼接会把 base 的路径部分丢掉，这里补上
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"api/health/checks/alarms/", nil)
	if err != nil {
		return "", fmt.Errorf("invalid node URL %q: %w", base, err)
	}
	req.Header.Set("Accept", "application/json")
	// 官方对空账号也会带 Authorization 头，这里保持一致
	req.SetBasicAuth(username, password)

	resp, err := client.Do(req)
	if err != nil {
		return "", simplifyNetErr(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		return "", nil
	case http.StatusServiceUnavailable:
		// 503 的 body 里 reason 字段说明哪个 alarm 触发
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(body, &payload); err == nil && payload.Reason != "" {
			return payload.Reason, fmt.Errorf("%s", payload.Reason)
		}
		return "", fmt.Errorf("%s", strings.TrimSpace(string(body)))
	default:
		if resp.StatusCode == http.StatusUnauthorized {
			return "", fmt.Errorf("401 - Unauthorized (check RabbitMQ username and password)")
		}
		return "", fmt.Errorf("%d - %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
}

// parseRabbitNodes 解析节点列表。库里存的是 JSON 数组文本。
func parseRabbitNodes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}

	var nodes []string
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return nil, fmt.Errorf("Invalid RabbitMQ Nodes")
	}

	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		// 用户可能只填 host:15672，补上 scheme
		if !strings.Contains(n, "://") {
			n = "http://" + n
		}
		if _, err := url.Parse(n); err != nil {
			return nil, fmt.Errorf("Invalid RabbitMQ node %q", n)
		}
		out = append(out, n)
	}
	return out, nil
}

// simplifyNetErr 把底层网络错误压成一句人话。
func simplifyNetErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("Connection refused")
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("Host not found")
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return fmt.Errorf("Request timed out")
	default:
		return err
	}
}
