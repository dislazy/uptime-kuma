package checkers

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// PingChecker 通过系统 ping 命令探测 ICMP 可达性。
//
// 之所以不直接发原始 ICMP：
//   - 原始套接字需要 root 或 CAP_NET_RAW，容器里经常没有；
//   - 自己实现 ICMP 还要处理 checksum、序列号、IPv6 差异，收益不高。
//
// 代价是镜像里必须有 ping 命令。官方镜像同样依赖系统 ping，
// 所以 Dockerfile 里会显式安装 iputils。
type PingChecker struct{}

var pingTimeRe = regexp.MustCompile(`time[=<]\s*([\d.]+)\s*ms`)

// Type 实现 Checker。
func (PingChecker) Type() string { return "ping" }

func init() { Register(PingChecker{}) }

// Check 执行一次 ping。
func (PingChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		// 部分配置把目标写在 url 字段里
		host = m.URL()
	}
	if host == "" {
		return Down("Hostname is empty")
	}

	if _, err := exec.LookPath("ping"); err != nil {
		return Down("ping command not found in the container image")
	}

	packetSize := int(m.GetInt("packet_size"))
	if packetSize <= 0 {
		packetSize = 56
	}

	args := []string{"-c", "1", "-W", "5", "-s", strconv.Itoa(packetSize), host}

	out, err := exec.CommandContext(ctx, "ping", args...).CombinedOutput()
	output := string(out)

	if err != nil {
		// ping 返回非 0 也要看输出：超时/不可达的情况消息更有用
		if msg := firstMeaningfulLine(output); msg != "" {
			return Down("%s", msg)
		}
		return Down("%s", err.Error())
	}

	if mm := pingTimeRe.FindStringSubmatch(output); len(mm) == 2 {
		if f, err := strconv.ParseFloat(mm[1], 64); err == nil {
			return Up(fmt.Sprintf("%s ms", mm[1]), int64(f))
		}
	}

	return Up("OK", 0)
}

// firstMeaningfulLine 从 ping 输出里挑一行最能说明问题的文本。
func firstMeaningfulLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "packet loss") || strings.Contains(lower, "transmitted") {
			continue
		}
		return line
	}
	return ""
}
