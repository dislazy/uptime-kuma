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
//
// 数据库（与官方 server/setup-database.js 同名同义）：
//
//	UPTIME_KUMA_DB_TYPE                sqlite（默认）| mariadb
//	UPTIME_KUMA_DB_HOSTNAME            主机名（用 socket 时忽略）
//	UPTIME_KUMA_DB_PORT                端口，默认 3306
//	UPTIME_KUMA_DB_NAME                库名，默认 kuma
//	UPTIME_KUMA_DB_USERNAME            用户名
//	UPTIME_KUMA_DB_PASSWORD            密码
//	UPTIME_KUMA_DB_SOCKET              unix socket 路径（设置后忽略 hostname/port）
//	UPTIME_KUMA_DB_SSL                 是否启用 TLS
//	UPTIME_KUMA_DB_CA                  自定义 CA（PEM）
//	UPTIME_KUMA_DB_POOL_MAX_CONNECTIONS 连接池上限，默认 10，范围 1–100
//
// 上述 *USERNAME / *PASSWORD / *SSL / *CA 四个支持 `_FILE` 后缀（docker secrets），
// 两者同时设置会报错，与官方 getEnvOrFile 一致。
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

	// 存储后端：sqlite（默认）或 mariadb（外部 MySQL/MariaDB）
	DBType     string
	DBHostname string
	DBPort     int
	DBName     string
	DBUsername string
	DBPassword string
	DBSocket   string
	DBSSL      bool
	DBCA       string
	DBMaxConns int
}

// 数据库类型取值。
const (
	DBTypeSQLite  = "sqlite"
	DBTypeMariaDB = "mariadb"
)

// IsMariaDB 是否使用外部 MySQL/MariaDB 作为存储。
func (c *Config) IsMariaDB() bool { return c.DBType == DBTypeMariaDB }

// Load 解析参数与环境变量。命令行优先于环境变量。
func Load() (*Config, error) {
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

	if err := c.loadDatabase(); err != nil {
		return nil, err
	}

	return c, nil
}

// loadDatabase 读取存储后端配置。
//
// 官方只接受 sqlite / mariadb 两种（embedded-mariadb 由官方镜像内置 mariadbd
// 提供，go_server 不实现）。这里额外接受 mysql 作为 mariadb 的别名——官方对
// `UPTIME_KUMA_DB_TYPE=mysql` 会抛 "Unknown Database type"，我们宽容一点，
// 不改变任何既有行为。
func (c *Config) loadDatabase() error {
	c.DBType = strings.ToLower(strings.TrimSpace(os.Getenv("UPTIME_KUMA_DB_TYPE")))
	switch c.DBType {
	case "", DBTypeSQLite:
		c.DBType = DBTypeSQLite
		c.DBPort = 3306
		c.DBName = "kuma"
		c.DBMaxConns = 10
		return nil
	case DBTypeMariaDB, "mysql":
		c.DBType = DBTypeMariaDB
	default:
		return fmt.Errorf("数据库类型不支持: %q（可选 sqlite / mariadb）", c.DBType)
	}

	var err error
	if c.DBHostname = strings.TrimSpace(os.Getenv("UPTIME_KUMA_DB_HOSTNAME")); c.DBHostname == "" {
		c.DBHostname = "127.0.0.1"
	}
	c.DBPort = firstPositive(envInt("UPTIME_KUMA_DB_PORT"), 3306)
	if c.DBName = strings.TrimSpace(os.Getenv("UPTIME_KUMA_DB_NAME")); c.DBName == "" {
		c.DBName = "kuma"
	}
	c.DBSocket = strings.TrimSpace(os.Getenv("UPTIME_KUMA_DB_SOCKET"))

	if c.DBUsername, err = envOrFile("UPTIME_KUMA_DB_USERNAME"); err != nil {
		return err
	}
	if c.DBPassword, err = envOrFile("UPTIME_KUMA_DB_PASSWORD"); err != nil {
		return err
	}
	c.DBUsername = strings.TrimSpace(c.DBUsername)
	if c.DBUsername == "" {
		return fmt.Errorf("UPTIME_KUMA_DB_USERNAME 未设置（mariadb 模式必填）")
	}

	sslVal, err := envOrFile("UPTIME_KUMA_DB_SSL")
	if err != nil {
		return err
	}
	c.DBSSL = parseBool(sslVal)

	if c.DBCA, err = envOrFile("UPTIME_KUMA_DB_CA"); err != nil {
		return err
	}
	c.DBCA = strings.TrimSpace(c.DBCA)

	// 官方语义：无效、<1、>100 都回落/截断，不报错
	c.DBMaxConns = envInt("UPTIME_KUMA_DB_POOL_MAX_CONNECTIONS")
	if c.DBMaxConns < 1 {
		c.DBMaxConns = 10
	}
	if c.DBMaxConns > 100 {
		c.DBMaxConns = 100
	}

	return nil
}

// envOrFile 读取环境变量，或 `<name>_FILE` 指向的文件内容（docker secrets）。
// 两者同时设置时报错，与官方 getEnvOrFile 一致。
func envOrFile(name string) (string, error) {
	direct := os.Getenv(name)
	file := os.Getenv(name + "_FILE")
	if direct != "" && file != "" {
		return "", fmt.Errorf("Both %s and %s_FILE are set. Please use only one.", name, name)
	}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("Failed to read %s_FILE at %s: %w", name, file, err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return direct, nil
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
	return parseBool(os.Getenv(key))
}

func parseBool(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	return s == "1" || s == "true" || s == "yes" || s == "on"
}
