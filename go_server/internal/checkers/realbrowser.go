//go:build realbrowser

package checkers

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// RealBrowserChecker 用真实 Chromium 打开页面。
//
// 构建方式：go build -tags realbrowser ./cmd/go_server
//
// 为什么不用 chromedp：它需要 CDP 长连接 + 一整套依赖，而这里只需要
// "页面能不能打开"。直接调 Chromium 的 headless --dump-dom 等价且零依赖：
// 进程正常退出 = 页面渲染完成。
//
// 浏览器路径优先级：设置项 chromeExecutable → 环境变量 CHROME_PATH →
// PATH 里的 google-chrome / chromium / chromium-browser。
type RealBrowserChecker struct{}

// Type 实现 Checker。
func (RealBrowserChecker) Type() string { return "real-browser" }

func init() { Register(RealBrowserChecker{}) }

// Check 实现 Checker。
func (RealBrowserChecker) Check(ctx context.Context, m *model.Monitor) Result {
	target := m.URL()
	if target == "" {
		return Down("URL is required")
	}
	u, err := url.Parse(target)
	if err != nil {
		return Down("Invalid URL: %v", err)
	}
	// 与官方一致：只允许 http/https，防止 file:// 读到容器内文件
	if u.Scheme != "http" && u.Scheme != "https" {
		return Down("Invalid url protocol, only http and https are allowed.")
	}

	binary, err := findChrome(ctx)
	if err != nil {
		return Down("%s", err.Error())
	}

	timeout := time.Duration(float64(m.Interval)*0.8) * time.Second
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 30 * time.Second
	}

	navCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(navCtx, binary,
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--virtual-time-budget=5000",
		"--dump-dom",
		target,
	)
	out, err := cmd.Output()
	ping := time.Since(start).Milliseconds()

	if err != nil {
		if navCtx.Err() == context.DeadlineExceeded {
			return Down("Timeout while loading %s (%.0fs)", target, timeout.Seconds())
		}
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg == "" {
				msg = strings.TrimSpace(string(out))
			}
			return Down("%s", truncateOutput(msg, 300))
		}
		return Down("%s", err.Error())
	}
	if len(out) == 0 {
		return Down("Empty page content returned by the browser")
	}

	return Up("OK", ping)
}

// findChrome 依次尝试设置项、环境变量与 PATH。
func findChrome(ctx context.Context) (string, error) {
	if p := strings.TrimSpace(Setting(ctx, "chromeExecutable", "")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", chromeNotFoundError{path: p}
	}
	if p := strings.TrimSpace(os.Getenv("CHROME_PATH")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", chromeNotFoundError{}
}

type chromeNotFoundError struct{ path string }

func (e chromeNotFoundError) Error() string {
	if e.path != "" {
		return "Chromium executable not found at " + e.path +
			". Set 'Chrome Executable' in Settings or install chromium."
	}
	return "Chromium executable not found. Set 'Chrome Executable' in Settings, or install chromium / google-chrome."
}
