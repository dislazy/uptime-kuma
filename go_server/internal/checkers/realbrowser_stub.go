//go:build !realbrowser

package checkers

import (
	"context"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// RealBrowserChecker 在默认构建里不可用。
//
// 真实浏览器探测需要把 Chromium 打进镜像（约 +300MB），对低配服务器不友好，
// 所以做成可选构建：`go build -tags realbrowser`。
// 默认构建保留类型与前端入口，但明确告知需要重新构建，而不是假装能跑。
type RealBrowserChecker struct{}

// Type 实现 Checker。
func (RealBrowserChecker) Type() string { return "real-browser" }

func init() { Register(RealBrowserChecker{}) }

// Check 实现 Checker。
func (RealBrowserChecker) Check(ctx context.Context, m *model.Monitor) Result {
	return Down("real-browser monitor is not compiled in this build. Rebuild with '-tags realbrowser' and provide a Chromium executable (see README).")
}
