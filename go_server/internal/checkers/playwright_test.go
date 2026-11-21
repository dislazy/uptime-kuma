package checkers

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPlaywrightProtocol 需要一个真实的 Playwright 浏览器服务器端点：
//
//	PW_E2E_ENDPOINT=ws://127.0.0.1:9333/<guid> go test -run TestPlaywrightProtocol
//
// 端点可用官方 playwright 生成（与线上 browserless 同协议）：
//
//	const { chromium } = require("playwright-core");
//	chromium.launchServer({ executablePath: ".../Chrome", headless: true, port: 9333 })
//
// 未设置环境变量时跳过（CI 无浏览器环境）。
func TestPlaywrightProtocol(t *testing.T) {
	endpoint := os.Getenv("PW_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("set PW_E2E_ENDPOINT to run the live playwright protocol test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1) 握手 + 版本
	c, err := pwConnect(ctx, endpoint)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if c.Version() == "" {
		t.Fatal("未取得浏览器版本")
	}
	t.Logf("远程浏览器版本: %s", c.Version())

	// 2) testRemoteBrowser 语义
	if err := TestRemoteBrowser(ctx, endpoint); err != nil {
		t.Fatalf("TestRemoteBrowser 失败: %v", err)
	}
	c.Close()

	// 3) 完整 checkPage 流程（公网站点，保证 200）
	c2, err := pwConnect(ctx, endpoint)
	if err != nil {
		t.Fatalf("第二次握手失败: %v", err)
	}
	defer c2.Close()

	start := time.Now()
	status, finalURL, err := c2.checkPage(ctx, "https://example.com", 30*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("checkPage 失败: %v", err)
	}
	if status != 200 {
		t.Fatalf("期望 200, 得到 %d (url=%s)", status, finalURL)
	}
	if finalURL == "" {
		t.Log("注意: result 未带 response url（某些版本行为），状态码已验证")
	}
	t.Logf("goto https://example.com → %d, 耗时 %s, finalURL=%s", status, elapsed, finalURL)

	// 4) 404 页面：官方语义 200-399 之外判 DOWN，这里只验证状态码采集正确
	status4, _, err := c2.checkPage(ctx, "https://example.com/definitely-not-exists-404", 30*time.Second)
	if err != nil {
		t.Logf("404 探测返回错误（部分站点 404 无 Response 对象，视官方行为而定）: %v", err)
	} else if status4 == 200 {
		t.Fatal("404 路径不应返回 200")
	} else {
		t.Logf("404 路径状态码 = %d", status4)
	}
}
