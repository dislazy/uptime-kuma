// Package httpapi 实现除 socket 之外的所有 HTTP 接口。
package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/auth"
	"github.com/dislazy/uptime-kuma/go_server/internal/config"
	"github.com/dislazy/uptime-kuma/go_server/internal/hub"
	"github.com/dislazy/uptime-kuma/go_server/internal/maintenance"
	"github.com/dislazy/uptime-kuma/go_server/internal/monitor"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
	"github.com/dislazy/uptime-kuma/go_server/internal/version"
)

// API 聚合 HTTP 接口依赖。
type API struct {
	Cfg  *config.Config
	DB   *store.DB
	Mon  *store.MonitorStore
	Beat *store.HeartbeatStore
	Stat *store.StatsStore
	Set  *store.Settings
	Auth *auth.Service
	Hub  *hub.Hub
	Mgr  *monitor.Manager

	StatusPage *store.StatusPageStore
	Maint      *store.MaintenanceStore
	MaintMgr   *maintenance.Manager
}

// Mount 把全部路由挂到 mux 上。
func Mount(mux *http.ServeMux, api *API) {
	// better-auth 兼容层
	mux.HandleFunc("POST /api/auth/sign-in/username", api.handleSignIn)
	mux.HandleFunc("POST /api/auth/sign-up/email", api.handleSignUpDisabled)
	mux.HandleFunc("GET /api/auth/get-session", api.handleGetSession)
	mux.HandleFunc("POST /api/auth/sign-out", api.handleSignOut)
	mux.HandleFunc("POST /api/auth/change-password", api.handleChangePassword)
	mux.HandleFunc("GET /api/auth/ok", api.handleOK)

	// 两步验证（better-auth twoFactor 插件）
	mux.HandleFunc("POST /api/auth/two-factor/enable", api.handleTwoFAEnable)
	mux.HandleFunc("POST /api/auth/two-factor/verify-totp", api.handleTwoFAVerifyTOTP)
	mux.HandleFunc("POST /api/auth/two-factor/verify-backup-code", api.handleTwoFAVerifyBackupCode)
	mux.HandleFunc("POST /api/auth/two-factor/disable", api.handleTwoFADisable)
	mux.HandleFunc("POST /api/auth/two-factor/generate-backup-codes", api.handleTwoFAGenerateBackupCodes)
	mux.HandleFunc("POST /api/auth/generate-backup-codes", api.handleTwoFAGenerateBackupCodes)
	mux.HandleFunc("GET /api/auth/", api.handleAuthFallback)
	mux.HandleFunc("POST /api/auth/", api.handleAuthFallback)

	// 首次初始化
	mux.HandleFunc("POST /api/setup", api.handleSetup)

	// 入口页判断
	mux.HandleFunc("GET /api/entry-page", api.handleEntryPage)

	// 被动推送监控
	mux.HandleFunc("/api/push/{token}", api.handlePush)

	// 状态页 RSS（公开访问）
	mux.HandleFunc("GET /status/{slug}/rss", api.handleStatusPageRSS)

	// 状态页（公开访问，无需登录）
	//
	// 用一个前缀入口统一分发：heartbeat 在 slug 之前、incident-history 在之后，
	// 用 {slug} 通配注册会互相冲突（/api/status-page/heartbeat/incident-history 两边都匹配）。
	mux.HandleFunc("GET /api/status-page/", api.handleStatusPageRoute)

	// 徽章
	mux.HandleFunc("GET /api/badge/{id}/{metric}", api.handleBadge)
	mux.HandleFunc("GET /api/badge/{id}/{metric}/{duration}", api.handleBadge)

	// 健康检查与指标
	mux.HandleFunc("GET /api/health", api.handleHealth)
	mux.HandleFunc("GET /metrics", api.handleMetrics)

	// 上传文件（状态页图标等）
	mux.Handle("GET /upload/", http.StripPrefix("/upload/", http.FileServer(http.Dir(api.Cfg.UploadDir))))

	// 需要 API Key 的 REST 接口（/api/info、/api/monitors）
	MountAPIRoutes(mux, api)
}

// ---------------- 工具 ----------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "message": msg, "error": msg})
}

// clientIP 解析真实客户端地址（受 trustProxy 控制）。
func (a *API) clientIP(r *http.Request) string {
	if a.Cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}

// ---------------- 健康检查 ----------------

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	ok := a.DB.SQL().PingContext(r.Context()) == nil
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ok":       ok,
		"version":  version.Version,
		"uptime":   int64(time.Since(startedAt).Seconds()),
		"monitors": a.Mgr.ActiveCount(),
		"clients":  a.Hub.Count(),
	})
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// P2 会用 prometheus/client_golang 输出完整指标；这里先给出最小可用集合。
	var b strings.Builder
	b.WriteString("# HELP gokuma_monitors_active 启用的监控数量\n")
	b.WriteString("# TYPE gokuma_monitors_active gauge\n")
	b.WriteString("gokuma_monitors_active " + itoa(a.Mgr.ActiveCount()) + "\n")

	b.WriteString("# HELP gokuma_socket_clients 当前 socket 连接数\n")
	b.WriteString("# TYPE gokuma_socket_clients gauge\n")
	b.WriteString("gokuma_socket_clients " + itoa(a.Hub.Count()) + "\n")

	b.WriteString("# HELP gokuma_uptime_seconds 进程运行秒数\n")
	b.WriteString("# TYPE gokuma_uptime_seconds counter\n")
	b.WriteString("gokuma_uptime_seconds " + itoa64(int64(time.Since(startedAt).Seconds())) + "\n")

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

var startedAt = time.Now()

func itoa(n int) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------- 静态资源辅助 ----------------

// resolveWebFile 把 URL 路径映射到前端静态文件，找不到时回退 index.html（SPA）。
func resolveWebFile(webDir, urlPath string) (string, bool) {
	clean := path.Clean("/" + urlPath)
	file := filepath.Join(webDir, filepath.FromSlash(clean))

	// 防止 ../ 逃逸
	absWeb, err1 := filepath.Abs(webDir)
	absFile, err2 := filepath.Abs(file)
	if err1 != nil || err2 != nil || !strings.HasPrefix(absFile, absWeb) {
		return "", false
	}

	if st, err := os.Stat(absFile); err == nil && !st.IsDir() {
		return absFile, true
	}
	return "", false
}
