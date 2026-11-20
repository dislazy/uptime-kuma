// Command go_server 是 Uptime Kuma 的 Go 后端实现入口。
//
// 启动顺序刻意与官方保持一致，方便对照排查：
// 数据库 → 设置 → 鉴权 → 调度器 → socket 服务 → HTTP 服务。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/auth"
	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/config"
	"github.com/dislazy/uptime-kuma/go_server/internal/handlers"
	"github.com/dislazy/uptime-kuma/go_server/internal/httpapi"
	"github.com/dislazy/uptime-kuma/go_server/internal/hub"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/maintenance"
	"github.com/dislazy/uptime-kuma/go_server/internal/monitor"
	"github.com/dislazy/uptime-kuma/go_server/internal/notify"
	"github.com/dislazy/uptime-kuma/go_server/internal/socketio"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
	"github.com/dislazy/uptime-kuma/go_server/internal/version"
	"github.com/dislazy/uptime-kuma/go_server/internal/web"
)

func main() {
	cfg := config.Load()

	if config.PrintVersion {
		fmt.Println(version.String())
		return
	}

	// 容器 HEALTHCHECK 走这个分支：只做一次本地探活，不启动服务
	if config.HealthcheckMode {
		os.Exit(runHealthcheck(cfg))
	}

	logx.SetDebug(cfg.Debug)
	logx.Infof("main", "启动 %s", version.String())

	if err := cfg.EnsureDirs(); err != nil {
		logx.Errorf("main", "%v", err)
		os.Exit(1)
	}

	if err := run(cfg); err != nil {
		logx.Errorf("main", "启动失败: %v", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---------- 数据库 ----------
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}

	settings := store.NewSettings(db)
	if err := settings.SeedDefaults(ctx, serverTimezone()); err != nil {
		logx.Warnf("main", "写入默认设置失败: %v", err)
	}

	// Steam / Globalping / real-browser 这些监控类型要读全局设置，
	// 把读取器注入到 checkers，避免 checkers 反向依赖 store。
	checkers.SetSettings(settings)
	notify.SetSettings(settings)

	// Web Push 需要一对长期有效的 VAPID 密钥，首次启动时生成并落库
	if _, err := notify.EnsureVAPIDKeys(ctx, settings); err != nil {
		logx.Warnf("main", "生成 VAPID 密钥失败: %v", err)
	}

	// ---------- 鉴权 ----------
	secret, err := authSecret(ctx, settings)
	if err != nil {
		return err
	}
	authSvc := auth.NewService(db, secret)

	// ---------- 领域服务 ----------
	monitorStore := store.NewMonitorStore(db)
	beatStore := store.NewHeartbeatStore(db)
	statsStore := store.NewStatsStore(db)
	notifStore := store.NewNotificationStore(db)
	tagStore := store.NewTagStore(db)
	maintStore := store.NewMaintenanceStore(db)
	statusPageStore := store.NewStatusPageStore(db)

	beatStore.Start(ctx)
	defer beatStore.Stop()

	h := hub.New()
	engine := notify.NewEngine(notifStore, settings)

	mgr := monitor.NewManager(db, monitorStore, beatStore, statsStore, h, engine, cfg.MaxConcurrentChecks)

	// 维护窗口：状态由运行时算，监控调度前会先问一遍
	serverTZ := settings.GetString(ctx, "serverTimezone", serverTimezone())
	maintMgr := maintenance.New(maintStore, h, serverTZ)
	mgr.SetMaintenance(maintMgr.IsUnderMaintenance)

	// ---------- socket 服务 ----------
	sockSrv := socketio.NewServer()
	if os.Getenv("UPTIME_KUMA_WS_ORIGIN_CHECK") == "bypass" {
		sockSrv.CheckOrigin = socketio.AllowAllOrigins
	}

	deps := &handlers.Deps{
		Cfg: cfg, DB: db, Mon: monitorStore, Beat: beatStore, Stat: statsStore,
		Notif: notifStore, Set: settings, Tag: tagStore,
		Maint: maintStore, StatusPage: statusPageStore,
		Auth: authSvc, Hub: h, Mgr: mgr, MaintMgr: maintMgr, Eng: engine,
		StartedAt: time.Now(),
	}
	handlers.Register(sockSrv, deps)

	// ---------- HTTP 路由 ----------
	api := &httpapi.API{
		Cfg: cfg, DB: db, Mon: monitorStore, Beat: beatStore, Stat: statsStore,
		Set: settings, Auth: authSvc, Hub: h, Mgr: mgr,
		StatusPage: statusPageStore, Maint: maintStore, MaintMgr: maintMgr,
	}

	mux := http.NewServeMux()
	httpapi.Mount(mux, api)
	mux.Handle("/socket.io/", sockSrv)
	mux.Handle("/", web.New(cfg.WebDir))

	// ---------- 启动监控与维护窗口 ----------
	if err := maintMgr.Start(ctx); err != nil {
		return fmt.Errorf("启动维护窗口失败: %w", err)
	}
	defer maintMgr.Stop()

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("启动调度器失败: %w", err)
	}
	defer mgr.Stop()
	mgr.StartRollup(ctx, cfg.KeepHeartbeatDays)

	// ---------- HTTP 服务 ----------
	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           withRecovery(withAccessLog(mux, cfg)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设 WriteTimeout：socket.io 的长轮询会占用连接
	}

	errCh := make(chan error, 1)
	go func() {
		logx.Infof("main", "监听 %s（TLS=%v）", cfg.Addr(), cfg.IsTLS())
		if cfg.IsTLS() {
			errCh <- srv.ListenAndServeTLS(cfg.SSLCert, cfg.SSLKey)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	// ---------- 优雅退出 ----------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case sig := <-quit:
		logx.Infof("main", "收到信号 %s，开始退出", sig)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	mgr.Stop()
	cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logx.Warnf("main", "优雅退出失败: %v", err)
	}

	logx.Infof("main", "已退出")
	return nil
}

// serverTimezone 推断服务器时区，顺序与官方 getTimezone() 一致：
// 环境变量 TZ → 系统 /etc/localtime 指向 → 退化为 UTC。
//
// 注意不能用 time.Now().Zone()：它返回的是 "CST" 这种缩写，
// 不是 IANA 名字，time.LoadLocation 解析不了。
func serverTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
		logx.Warnf("main", "TZ=%q 不是有效的时区名，继续探测", tz)
	}

	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			name := link[i+len("zoneinfo/"):]
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}

	if name := time.Now().Location().String(); name != "" && name != "Local" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	return "UTC"
}

// authSecret 读取或生成会话签名密钥，持久化到设置表。
func authSecret(ctx context.Context, s *store.Settings) ([]byte, error) {
	if env := os.Getenv("UPTIME_KUMA_AUTH_SECRET"); env != "" {
		return []byte(env), nil
	}

	if raw, ok, err := s.Raw(ctx, "authSecret"); err == nil && ok && len(raw) >= 32 {
		// 存的是 JSON 字符串，去掉引号
		trimmed := raw
		if len(trimmed) > 1 && trimmed[0] == '"' {
			if unq, err := strconv.Unquote(trimmed); err == nil {
				trimmed = unq
			}
		}
		return []byte(trimmed), nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(buf)

	if err := s.Set(ctx, "authSecret", secret, "internal"); err != nil {
		return nil, err
	}
	logx.Infof("main", "已生成新的会话签名密钥")
	return []byte(secret), nil
}

// runHealthcheck 供容器 HEALTHCHECK 使用。
func runHealthcheck(cfg *config.Config) int {
	host := "127.0.0.1"
	if cfg.Host != "" && cfg.Host != "0.0.0.0" && cfg.Host != "::" {
		host = cfg.Host
	}

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://%s/api/health", net.JoinHostPort(host, strconv.Itoa(cfg.Port))))
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: status %d\n", res.StatusCode)
		return 1
	}
	return 0
}

// withRecovery 捕获 handler panic，避免单个请求打挂进程。
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logx.Errorf("http", "panic at %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withAccessLog 记录慢请求，正常请求只在 debug 级别输出。
func withAccessLog(next http.Handler, cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		elapsed := time.Since(start)

		// 静态资源与长轮询的日志噪声太大，过滤掉
		if elapsed > 2*time.Second {
			logx.Warnf("http", "慢请求 %s %s 耗时 %v", r.Method, r.URL.Path, elapsed)
			return
		}
		if cfg.Debug {
			logx.Debugf("http", "%s %s %v", r.Method, r.URL.Path, elapsed)
		}
	})
}
