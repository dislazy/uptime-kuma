package checkers

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// SystemServiceChecker 检查 systemd 服务（或 Windows 服务）是否在运行。
//
// 官方用 systemctl is-active，这里保持一致：exit code 非 0 时的 stdout/stderr
// 就是服务状态（inactive / failed / unknown），比笼统的 "connection failed" 有用。
type SystemServiceChecker struct{}

// Type 实现 Checker。
func (SystemServiceChecker) Type() string { return "system-service" }

func init() { Register(SystemServiceChecker{}) }

// 只允许字母数字和 . _ - @，避免参数注入（systemctl 会把 -- 开头的当选项）
var serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9._\-@]+$`)

// Check 实现 Checker。
func (SystemServiceChecker) Check(ctx context.Context, m *model.Monitor) Result {
	name := strings.TrimSpace(m.ConfigValue("systemServiceName"))
	if name == "" {
		return Down("Service Name is required.")
	}

	switch runtime.GOOS {
	case "linux":
		return checkSystemdService(ctx, name)
	case "windows":
		return checkWindowsService(ctx, name)
	default:
		return Down("System Service monitoring is not supported on %s", runtime.GOOS)
	}
}

func checkSystemdService(ctx context.Context, name string) Result {
	if !serviceNameRe.MatchString(name) {
		return Down("Invalid service name. Please use the internal Service Name (no spaces).")
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, "systemctl", "is-active", name).CombinedOutput()
	output := truncateOutput(strings.TrimSpace(string(out)), 200)

	if err != nil {
		if output == "" {
			output = "Service '" + name + "' is not running."
		}
		return Down("%s", output)
	}
	return Up("Service '"+name+"' is running.", 0)
}

func checkWindowsService(ctx context.Context, name string) Result {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(name) {
		return Down("Invalid service name. Only alphanumeric characters and '.', '_', '-' are allowed.")
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-Service -Name '"+strings.ReplaceAll(name, "'", "''")+"').Status").CombinedOutput()
	output := truncateOutput(strings.TrimSpace(string(out)), 200)

	if err != nil {
		return Down("Service '%s' is not running/found.", name)
	}
	if output == "Running" {
		return Up("Service '"+name+"' is running.", 0)
	}
	return Down("Service '%s' is %s.", name, output)
}

// truncateOutput 防止把整段 systemctl 输出写进 heartbeat 表。
func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ---------- tailscale ----------

// TailscalePingChecker 用 `tailscale ping --c 1` 判断对端是否可达。
//
// 这是唯一可行的做法：tailscale 的连通性不走标准 ICMP，必须经它的
// 控制面/数据面（DERP 或直连），也只能由 tailscale 客户端自己判断。
type TailscalePingChecker struct{}

// Type 实现 Checker。
func (TailscalePingChecker) Type() string { return "tailscale-ping" }

func init() { Register(TailscalePingChecker{}) }

// Check 实现 Checker。
func (TailscalePingChecker) Check(ctx context.Context, m *model.Monitor) Result {
	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}

	// 官方用 interval * 0.8 作为上限，避免和下一轮探测重叠
	timeout := time.Duration(float64(m.Interval)*0.8) * time.Second
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 10 * time.Second
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, "tailscale", "ping", "--c", "1", host).Output()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			return Down("Error checking Tailscale ping: tailscale command not found in PATH")
		}
		return Down("Error checking Tailscale ping: %v", err)
	}
	if len(out) == 0 {
		return Down("Error checking Tailscale ping: No output from Tailscale ping")
	}

	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.Contains(line, "pong from"):
			// 形如 "pong from host (100.x) in 12ms"
			ping := int64(0)
			if _, after, ok := strings.Cut(line, " in "); ok {
				fields := strings.Fields(after)
				if len(fields) > 0 {
					ms := strings.TrimSuffix(fields[0], "ms")
					if n, err := strconv.ParseInt(strings.TrimSpace(ms), 10, 64); err == nil {
						ping = n
					}
				}
			}
			return Up("OK", ping)
		case strings.Contains(line, "timed out"):
			return Down("Ping timed out: %q", line)
		case strings.Contains(line, "no matching peer"):
			return Down("Nonexistant or inaccessible due to ACLs: %q", line)
		case strings.Contains(line, "is local Tailscale IP"):
			return Down("Tailscale only works if used on other machines: %q", line)
		case strings.TrimSpace(line) != "":
			return Down("Unexpected output: %q", line)
		}
	}
	return Down("Error checking Tailscale ping: No pong received")
}

// ---------- pm2 ----------

// PM2Checker 明确返回"不支持"。
//
// pm2 是 Node 专属的进程管理器，Go 后端既不启动 node，也没有它的进程表，
// 硬做一个假的会误导用户。前端入口保留，错误提示指向替代方案。
type PM2Checker struct{}

// Type 实现 Checker。
func (PM2Checker) Type() string { return "pm2" }

func init() { Register(PM2Checker{}) }

// Check 实现 Checker。
func (PM2Checker) Check(ctx context.Context, m *model.Monitor) Result {
	return Down("PM2 is not available in the Go server. Please use a systemd service and the 'System Service' monitor type instead.")
}
