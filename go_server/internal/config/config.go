// Package config 负责解析命令行参数与环境变量。
//
// 命名与官方 Node 版本保持一致，这样 docker-compose.yml / .env 可以原样复用：
//
//	UPTIME_KUMA_PORT / PORT            监听端口，默认 3001
//	UPTIME_KUMA_HOST / HOST            监听地址，默认空（双栈）
//	DATA_DIR                           数据目录，默认 ./data
//	UPTIME_KUMA_SSL_KEY / SSL_KEY      TLS 私钥路径
//	UPTIME_KUMA_SSL_CERT / SSL_CERT    TLS 证书路径
//	UPTIME_KUMA_SSL_KEY_PASSPHRASE     私钥口令
//	UPTIME_KUMA_TRUST_PROXY            信任反向代理提供的 X-Forwarded-For
//	UPTIME_KUMA_DISABLE_AUTH           关闭鉴权（单机自用场景）
//	UPTIME_KUMA_DEBUG                  打开 debug 日志
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 是进程级配置，启动后只读。
type Config struct {
	Host      string
	Port      int
	DataDir   string
	DBPath    string
	UploadDir string
	WebDir    string

	SSLKey        string
	SSLCert       string
	SSLPassphrase string

	TrustProxy  bool
	DisableAuth bool
	Debug       bool
	DemoMode    bool

	// 监控调度
	MaxConcurrentChecks int

	// 数据保留
	KeepHeartbeatDays int
}

// Load 解析参数与环境变量。命令行优先于环境变量。
func Load() *Config {
	c := &Config{}

	var (
		host       = flag.String("host", "", "监听地址")
		port       = flag.Int("port", 0, "监听端口")
		dataDir    = flag.String("data-dir", "", "数据目录")
		webDir     = flag.String("web-dir", "", "前端静态资源目录")
		sslKey     = flag.String("ssl-key", "", "TLS 私钥路径")
		sslCert    = flag.String("ssl-cert", "", "TLS 证书路径")
		sslPass    = flag.String("ssl-key-passphrase", "", "TLS 私钥口令")
		trustProxy = flag.Bool("trust-proxy", false, "信任反向代理头")
		debug      = flag.Bool("debug", false, "debug 日志")
		demo       = flag.Bool("demo", false, "演示模式")
		maxChecks  = flag.Int("max-concurrent-checks", 0, "并发检查数上限")
		keepDays   = flag.Int("keep-heartbeat-days", 0, "心跳保留天数")
		health     = flag.Bool("healthcheck", false, "以健康检查模式运行（供 HEALTHCHECK 使用）")
		version    = flag.Bool("version", false, "打印版本号后退出")
	)
	flag.Parse()

	// 供 main 判断
	HealthcheckMode = *health
	PrintVersion = *version

	c.Host = firstNonEmpty(*host, os.Getenv("UPTIME_KUMA_HOST"), os.Getenv("HOST"))
	c.Port = firstPositive(*port, envInt("UPTIME_KUMA_PORT"), envInt("PORT"), 3001)

	c.DataDir = firstNonEmpty(*dataDir, os.Getenv("DATA_DIR"), "./data")
	abs, err := filepath.Abs(c.DataDir)
	if err == nil {
		c.DataDir = abs
	}
	c.DBPath = filepath.Join(c.DataDir, "kuma.db")
	c.UploadDir = filepath.Join(c.DataDir, "upload")

	c.WebDir = firstNonEmpty(*webDir, os.Getenv("UPTIME_KUMA_WEB_DIR"), "./web/dist")

	c.SSLKey = firstNonEmpty(*sslKey, os.Getenv("UPTIME_KUMA_SSL_KEY"), os.Getenv("SSL_KEY"))
	c.SSLCert = firstNonEmpty(*sslCert, os.Getenv("UPTIME_KUMA_SSL_CERT"), os.Getenv("SSL_CERT"))
	c.SSLPassphrase = firstNonEmpty(*sslPass, os.Getenv("UPTIME_KUMA_SSL_KEY_PASSPHRASE"), os.Getenv("SSL_KEY_PASSPHRASE"))

	c.TrustProxy = *trustProxy || envBool("UPTIME_KUMA_TRUST_PROXY")
	c.DisableAuth = envBool("UPTIME_KUMA_DISABLE_AUTH")
	c.Debug = *debug || envBool("UPTIME_KUMA_DEBUG")
	c.DemoMode = *demo || envBool("UPTIME_KUMA_DEMO")

	c.MaxConcurrentChecks = firstPositive(*maxChecks, envInt("UPTIME_KUMA_MAX_CONCURRENT_CHECKS"), 20)
	c.KeepHeartbeatDays = firstPositive(*keepDays, envInt("UPTIME_KUMA_KEEP_HEARTBEAT_DAYS"), 365)

	return c
}

// HealthcheckMode / PrintVersion 是 -healthcheck / -version 的一次性开关。
var (
	HealthcheckMode bool
	PrintVersion    bool
)

// IsTLS 是否启用 HTTPS。
func (c *Config) IsTLS() bool {
	return c.SSLKey != "" && c.SSLCert != ""
}

// Addr 返回 net.Listen 使用的地址。
func (c *Config) Addr() string {
	if c.Port <= 0 {
		return fmt.Sprintf(":%d", 3001)
	}
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// BaseURL 用于生成自引用链接。
func (c *Config) BaseURL() string {
	scheme := "http"
	if c.IsTLS() {
		return fmt.Sprintf("https://%s", c.publicHost())
	}
	return fmt.Sprintf("%s://%s", scheme, c.publicHost())
}

func (c *Config) publicHost() string {
	h := c.Host
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "localhost"
	}
	return fmt.Sprintf("%s:%d", h, c.Port)
}

// EnsureDirs 创建数据目录。
func (c *Config) EnsureDirs() error {
	for _, dir := range []string{c.DataDir, c.UploadDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func envInt(key string) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return 0
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
