// Package store 是数据访问层：数据库连接、schema 初始化、以及各表的读写。
//
// 支持两种存储后端，与官方 UPTIME_KUMA_DB_TYPE 对齐：
//   - sqlite  —— 内置，文件默认 <DATA_DIR>/kuma.db
//   - mariadb —— 外部 MySQL / MariaDB，走 go-sql-driver/mysql
//
// 两种后端共用同一份表结构与绝大部分 SQL（占位符都是 ?），只在以下几处方言
// 差异上分支：表清单/列信息查询、建表脚本、UPSERT 语法、CAST 类型。
//
// 关键设计：所有写入都通过 map[string]any 完成，并在写入前查询列信息
// 过滤掉表中不存在的列。这样带来两个好处：
//  1. 老库（缺字段）不会因为 INSERT 带上新列而整条失败；
//  2. 官方后续新增字段时，Go 侧无需改代码即可继续工作。
package store

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gomysql "github.com/go-sql-driver/mysql" // 纯 Go 驱动，免 CGO；同时用于注册自定义 TLS
	_ "modernc.org/sqlite"                   // 纯 Go 驱动，免 CGO

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
)

//go:embed schema.sql
var schemaFS embed.FS

//go:embed schema_mysql.sql
var schemaMySQLFS embed.FS

// dialect 标识底层数据库类型。
type dialect string

const (
	dialectSQLite  dialect = "sqlite"
	dialectMariaDB dialect = "mariadb"
)

// MariaDBConfig 是外部 MySQL/MariaDB 的连接参数，字段与官方 db-config.json 对应。
type MariaDBConfig struct {
	Hostname string
	Port     int
	DBName   string
	Username string
	Password string
	Socket   string // 非空时忽略 Hostname/Port，走 unix socket
	SSL      bool
	CA       string // PEM
	MaxConns int
}

// DB 包装 *sql.DB，并缓存各表列名。
type DB struct {
	db      *sql.DB
	path    string // sqlite 文件路径，或 mariadb 的 host/db 描述（仅用于日志）
	dialect dialect

	mu    sync.RWMutex
	cols  map[string]map[string]bool // table -> column set
	table map[string]bool
}

// TimeLayout 是与官方 redbean `isoDateTimeMillis` 一致的格式。
// 官方存的是 UTC 字符串，前端按原样解析。
const TimeLayout = "2006-01-02 15:04:05.000"

// Open 打开（不存在则创建）数据库文件。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	// WAL + busy_timeout 是低配机器上避免 "database is locked" 的关键。
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(0)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	return &DB{
		db:      sqlDB,
		path:    path,
		dialect: dialectSQLite,
		cols:    map[string]map[string]bool{},
		table:   map[string]bool{},
	}, nil
}

// OpenMariaDB 连接外部 MySQL / MariaDB。
//
// 与官方 mysql2 配置的关键一致性：
//   - ParseTime=false：驱动把 DATETIME 原样返回字符串，由 ParseTime 解析。
//     官方用 typeCast 把 DATETIME 转成原样字符串，语义相同；若让驱动做时区换算，
//     官方存的 UTC 字符串会被再偏移一次。
//   - session time_zone='+00:00'：让 CURRENT_TIMESTAMP / TIMESTAMP 列也按 UTC，
//     否则默认值会写成数据库服务器本地时间，被当作 UTC 读出来就偏了。
func OpenMariaDB(cfg MariaDBConfig) (*DB, error) {
	// 官方在建连接池前先 CREATE DATABASE IF NOT EXISTS，否则首次部署
	// 会因为 "Unknown database" 直接起不来。
	if err := ensureMariaDBDatabase(cfg); err != nil {
		return nil, err
	}

	dsn, err := mariadbDSN(cfg)
	if err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	maxConns := cfg.MaxConns
	if maxConns < 1 {
		maxConns = 10
	}
	idle := maxConns / 2
	if idle < 1 {
		idle = 1
	}
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(idle)
	// MySQL 默认 wait_timeout 8 小时会静默断开空闲连接，比它短即可；
	// 复用失效连接会报 "invalid connection"，定期换新更稳。
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	sqlDB.SetConnMaxIdleTime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接数据库失败 (%s): %w", mariadbLabel(cfg), err)
	}

	return &DB{
		db:      sqlDB,
		path:    mariadbLabel(cfg),
		dialect: dialectMariaDB,
		cols:    map[string]map[string]bool{},
		table:   map[string]bool{},
	}, nil
}

func mariadbLabel(cfg MariaDBConfig) string {
	addr := cfg.Socket
	if addr == "" {
		addr = net.JoinHostPort(cfg.Hostname, itoa(cfg.Port))
	}
	return fmt.Sprintf("mariadb:%s/%s", addr, cfg.DBName)
}

// ensureMariaDBDatabase 先连到服务器（不指定库）建库，与官方
// setup-database.js 里 `CREATE DATABASE IF NOT EXISTS ... CHARACTER SET utf8mb4` 等价。
//
// 用户没有 CREATE 权限但库已存在时，这里会报错并提示——官方那道
// connection.execute 同样会抛，不会静默降级。
func ensureMariaDBDatabase(cfg MariaDBConfig) error {
	if strings.TrimSpace(cfg.DBName) == "" {
		return fmt.Errorf("UPTIME_KUMA_DB_NAME 为空")
	}

	probe := cfg
	probe.DBName = ""
	dsn, err := mariadbDSN(probe)
	if err != nil {
		return err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("连接数据库服务器失败: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("连接数据库服务器失败 (%s): %w", mariadbLabel(cfg), err)
	}

	q := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s CHARACTER SET utf8mb4", quoteBacktick(cfg.DBName))
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("创建数据库 %s 失败（请确认账号有 CREATE 权限）: %w", cfg.DBName, err)
	}
	return nil
}

// quoteBacktick 用反引号包裹标识符，内部的 ` 按 MySQL 规则双写转义。
func quoteBacktick(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func itoa(n int) string {
	if n <= 0 {
		return "3306"
	}
	return fmt.Sprintf("%d", n)
}

// tlsRegistered 记录已注册的 TLS 配置名，避免同名重复注册报错。
var (
	tlsMu         sync.Mutex
	tlsRegistered = map[string]bool{}
)

// mariadbDSN 用驱动的 Config 结构拼 DSN，这样密码里的 @ : / ? 等字符会被正确转义。
func mariadbDSN(cfg MariaDBConfig) (string, error) {
	c := gomysql.NewConfig()
	c.User = cfg.Username
	c.Passwd = cfg.Password
	c.DBName = cfg.DBName
	c.ParseTime = false // 关键：DATETIME 原样返回字符串
	c.Loc = time.UTC
	c.Timeout = 10 * time.Second
	// 驱动没有独立的 Charset 字段，charset 只能走 Params。
	c.Params = map[string]string{
		"charset":   "utf8mb4",
		"time_zone": "'+00:00'",
	}

	if cfg.Socket != "" {
		c.Net = "unix"
		c.Addr = cfg.Socket
	} else {
		c.Net = "tcp"
		c.Addr = net.JoinHostPort(cfg.Hostname, itoa(cfg.Port))
	}

	if cfg.SSL {
		if cfg.CA == "" {
			c.TLSConfig = "true" // 用系统根证书并校验主机名
		} else {
			name, err := registerCA(cfg.CA, cfg.Hostname)
			if err != nil {
				return "", err
			}
			c.TLSConfig = name
		}
	}

	return c.FormatDSN(), nil
}

// registerCA 用自定义 CA 注册一个驱动级 TLS 配置，返回可在 DSN 里引用的名字。
func registerCA(pem, hostname string) (string, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return "", fmt.Errorf("UPTIME_KUMA_DB_CA 不是合法的 PEM 证书")
	}

	sum := sha1.Sum([]byte(pem))
	name := "kuma-" + hex.EncodeToString(sum[:8])

	tlsMu.Lock()
	defer tlsMu.Unlock()
	if tlsRegistered[name] {
		return name, nil
	}
	if err := gomysql.RegisterTLSConfig(name, &tls.Config{
		RootCAs:    pool,
		ServerName: hostname,
		MinVersion: tls.VersionTLS12,
	}); err != nil {
		return "", fmt.Errorf("注册 TLS 配置失败: %w", err)
	}
	tlsRegistered[name] = true
	return name, nil
}

// dialectOf 返回底层数据库类型。
func (d *DB) dialectOf() dialect { return d.dialect }

// isMySQL 是否外部 MySQL/MariaDB。
func (d *DB) isMySQL() bool { return d.dialect == dialectMariaDB }

// bracketIdent 匹配 SQL 里的 [ident] 写法（SQLite 的标识符引号）。
// 只匹配简单标识符，不会碰到字符串字面量——代码里没有含 [...] 的 SQL 字符串常量。
var bracketIdent = regexp.MustCompile(`\[([A-Za-z_][A-Za-z0-9_]*)\]`)

// rewriteSQL 把 SQLite 的 [ident] 改写成 MySQL 的反引号形式。
//
// 选择在执行入口统一改写、而不是把几百条 SQL 逐条改成反引号，原因有二：
//  1. Go 的原始字符串用反引号定界，SQL 里写反引号就必须改成双引号并转义，
//     改动量大且容易漏；
//  2. 表名里 `user` / `group` / `key` / `default` / `interval` 是 MySQL 保留字，
//     必须有引号，不能简单去掉方括号。
func (d *DB) rewriteSQL(q string) string {
	if !d.isMySQL() {
		return q
	}
	return bracketIdent.ReplaceAllString(q, "`$1`")
}

// exec / query 是带方言改写的底层执行入口，store 内部一律走它们。
func (d *DB) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, d.rewriteSQL(q), args...)
}

func (d *DB) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, d.rewriteSQL(q), args...)
}

// SQL 暴露底层连接，供少数需要事务/自定义 SQL 的地方使用。
func (d *DB) SQL() *sql.DB { return d.db }

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// Close 关闭连接。
func (d *DB) Close() error { return d.db.Close() }

// schemaScript 返回当前方言的建表脚本。
//
// 两份脚本必须保持"表名、列名完全一致"，只有类型写法不同——测试
// TestSchemaParity 会逐表逐列比对，漂移会直接失败。
func (d *DB) schemaScript() (string, error) {
	if d.isMySQL() {
		raw, err := schemaMySQLFS.ReadFile("schema_mysql.sql")
		if err != nil {
			return "", fmt.Errorf("读取 schema_mysql.sql 失败: %w", err)
		}
		return string(raw), nil
	}
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return "", fmt.Errorf("读取 schema.sql 失败: %w", err)
	}
	return string(raw), nil
}

// Migrate 初始化 schema。
//
// 若库中已存在官方建好的 monitor 表，则视为"接管既有库"，不再执行建表脚本，
// 只补建 go_server 自己的元数据表——避免和官方 schema 打架。
func (d *DB) Migrate(ctx context.Context) error {
	exists, err := d.tableExists(ctx, "monitor")
	if err != nil {
		return err
	}

	if exists {
		logx.Infof("store", "检测到既有数据库，接管模式（不重建表）: %s", d.path)
		if err := d.ensureMetaTable(ctx); err != nil {
			return err
		}
		// 老库可能缺后来新增的列，按 schema 补齐
		if err := d.ensureColumns(ctx); err != nil {
			logx.Warnf("store", "补列迁移未完成: %v", err)
		}
		// 老库也可能缺后来新增的整张表（如 better_auth_apikey），按 CREATE TABLE
		// IF NOT EXISTS 原样补建；索引只对新建的表执行，避免碰官方既有表的列差异。
		if err := d.ensureTables(ctx); err != nil {
			logx.Warnf("store", "补表迁移未完成: %v", err)
		}
		if err := d.backfillAPIKeyReferenceID(ctx); err != nil {
			logx.Warnf("store", "apikey referenceId 回填未完成: %v", err)
		}
		d.warmCache(ctx)
		d.reportMissing(ctx)
		return nil
	}

	script, err := d.schemaScript()
	if err != nil {
		return err
	}

	logx.Infof("store", "初始化数据库: %s（%s）", d.path, d.dialect)
	for _, stmt := range splitStatements(script) {
		if _, err := d.exec(ctx, stmt); err != nil {
			return fmt.Errorf("执行建表语句失败: %w\nSQL: %s", err, truncate(stmt, 200))
		}
	}

	d.warmCache(ctx)
	return nil
}

func (d *DB) ensureMetaTable(ctx context.Context) error {
	q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		%s   VARCHAR(100) PRIMARY KEY,
		value   TEXT
	)`, d.quoteIdent("_gokuma_meta"), d.quoteIdent("key"))
	_, err := d.exec(ctx, q)
	return err
}

func (d *DB) tableExists(ctx context.Context, name string) (bool, error) {
	var n int
	var err error
	if d.isMySQL() {
		err = d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables
			  WHERE table_schema = DATABASE() AND table_name = ?`, name).Scan(&n)
	} else {
		err = d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&n)
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// warmCache 预读表与列信息。
func (d *DB) warmCache(ctx context.Context) {
	var (
		rows *sql.Rows
		err  error
	)
	if d.isMySQL() {
		rows, err = d.db.QueryContext(ctx,
			`SELECT table_name FROM information_schema.tables
			  WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	}
	if err != nil {
		logx.Warnf("store", "读取表清单失败: %v", err)
		return
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			names = append(names, n)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range names {
		d.table[n] = true
		d.loadColumns(ctx, n)
	}
}

// loadColumns 读取某表列集合，调用方需持有写锁或保证串行。
func (d *DB) loadColumns(ctx context.Context, table string) map[string]bool {
	if c, ok := d.cols[table]; ok {
		return c
	}
	set := map[string]bool{}

	if d.isMySQL() {
		// MySQL 的 information_schema 用参数绑定即可，不需要拼 SQL
		rows, err := d.db.QueryContext(ctx,
			`SELECT column_name FROM information_schema.columns
			  WHERE table_schema = DATABASE() AND table_name = ?`, table)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err == nil {
					set[name] = true
				}
			}
		}
		d.cols[table] = set
		return set
	}

	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info([%s])`, sanitizeIdent(table)))
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var (
				cid     int
				name    string
				typ     string
				notnull int
				dflt    sql.NullString
				pk      int
			)
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil {
				set[name] = true
			}
		}
	}
	d.cols[table] = set
	return set
}

// Columns 返回表的列集合（带缓存）。
func (d *DB) Columns(ctx context.Context, table string) map[string]bool {
	d.mu.RLock()
	if c, ok := d.cols[table]; ok {
		d.mu.RUnlock()
		return c
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loadColumns(ctx, table)
}

// HasTable 判断表是否存在。
func (d *DB) HasTable(ctx context.Context, table string) bool {
	d.mu.RLock()
	if v, ok := d.table[table]; ok {
		d.mu.RUnlock()
		return v
	}
	d.mu.RUnlock()

	ok, err := d.tableExists(ctx, table)
	if err != nil {
		return false
	}
	d.mu.Lock()
	d.table[table] = ok
	d.mu.Unlock()
	return ok
}

// ensureTables 在接管既有库时补建 schema.sql 里新增、但库里还没有的表。
//
// 只执行 CREATE TABLE IF NOT EXISTS（幂等），索引语句仅针对本次新建的表执行，
// 不触碰官方既有表的索引定义，避免列差异导致的意外报错。
func (d *DB) ensureTables(ctx context.Context) error {
	script, err := d.schemaScript()
	if err != nil {
		return err
	}

	created := map[string]bool{}
	for _, stmt := range splitStatements(script) {
		s := strings.TrimSpace(stmt)
		table, ok := createTableName(s)
		if !ok {
			continue
		}
		exists, err := d.tableExists(ctx, table)
		if err != nil || exists {
			continue
		}
		if _, err := d.exec(ctx, stmt); err != nil {
			logx.Warnf("store", "补表 %s 失败: %v", table, err)
			continue
		}
		logx.Infof("store", "已补表 %s", table)
		created[table] = true
	}

	if len(created) > 0 {
		for _, stmt := range splitStatements(script) {
			s := strings.TrimSpace(stmt)
			table, ok := indexTableName(s)
			if !ok || !created[table] {
				continue
			}
			if _, err := d.db.ExecContext(ctx, stmt); err != nil && !isDuplicateIndexErr(err) {
				logx.Warnf("store", "补索引失败: %v\nSQL: %s", err, truncate(stmt, 120))
			}
		}
	}
	return nil
}

// isDuplicateIndexErr 判断"索引已存在"错误。
//
// MySQL 没有 CREATE INDEX IF NOT EXISTS，重复执行会报 1061（Duplicate key name）。
// 这一路径只在补建新表后执行，理论不会重复，但手工干预过的库可能遇到，忽略即可。
func isDuplicateIndexErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate key name")
}

// createTableName 从 "CREATE TABLE IF NOT EXISTS [x] (...)" / "`x`" 里取表名。
func createTableName(stmt string) (string, bool) {
	const prefix = "CREATE TABLE IF NOT EXISTS"
	if !strings.HasPrefix(stmt, prefix) {
		return "", false
	}
	return readIdent(strings.TrimSpace(strings.TrimPrefix(stmt, prefix)))
}

// readIdent 读取以 [x] 或 `x` 包裹的标识符。
func readIdent(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	var closer byte
	switch s[0] {
	case '[':
		closer = ']'
	case '`':
		closer = '`'
	default:
		return "", false
	}
	end := strings.IndexByte(s[1:], closer)
	if end < 0 {
		return "", false
	}
	return s[1 : 1+end], true
}

// indexTableName 从 "CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON [x] (...)" 里取表名。
func indexTableName(stmt string) (string, bool) {
	up := strings.ToUpper(stmt)
	if !strings.HasPrefix(up, "CREATE INDEX") && !strings.HasPrefix(up, "CREATE UNIQUE INDEX") {
		return "", false
	}
	i := strings.Index(up, " ON ")
	if i < 0 {
		return "", false
	}
	return readIdent(strings.TrimSpace(stmt[i+len(" ON "):]))
}

// backfillAPIKeyReferenceID 把早期 go_server 自建库里的 userId 列值
// 迁到官方列名 referenceId 上（2026-11 起对齐官方 2026-06-29 迁移）。
func (d *DB) backfillAPIKeyReferenceID(ctx context.Context) error {
	exists, err := d.tableExists(ctx, "better_auth_apikey")
	if err != nil || !exists {
		return nil
	}
	cols := d.Columns(ctx, "better_auth_apikey")
	// PRAGMA 返回的列名保持声明时的大小写，两种历史拼法都认
	if !(cols["referenceId"] || cols["referenceid"]) || !(cols["userId"] || cols["userid"]) {
		return nil
	}
	res, err := d.Exec(ctx,
		"UPDATE better_auth_apikey SET referenceId = userId "+
			"WHERE (referenceId IS NULL OR referenceId = '') AND userId IS NOT NULL AND userId != ''")
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			logx.Infof("store", "已回填 better_auth_apikey.referenceId（%d 行）", n)
		}
	}
	return err
}

// reportMissing 在接管官方库时，提示源码里用到但库里缺失的列，方便排查。
func (d *DB) reportMissing(ctx context.Context) {
	// 只检查最关键的几张表，避免启动噪声。
	for _, t := range []string{"monitor", "heartbeat"} {
		if !d.HasTable(ctx, t) {
			logx.Warnf("store", "数据库缺少表 %s，相关功能将不可用", t)
		}
	}
}

// splitStatements 按分号拆分 SQL 脚本，同时去掉 -- 注释。
func splitStatements(script string) []string {
	var (
		out   []string
		buf   strings.Builder
		lines = strings.Split(script, "\n")
	)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}

	for _, stmt := range strings.Split(buf.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sanitizeIdent 只允许字母数字下划线，防止 PRAGMA 拼接注入。
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		return path[:i]
	}
	return "."
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// FormatTime 把时间格式化为官方一致的 UTC 字符串。
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// Now 返回当前 UTC 时间的官方格式。
func Now() string { return FormatTime(time.Now()) }

// ParseTime 解析官方格式时间；兼容 RFC3339 与无毫秒的形式。
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{TimeLayout, "2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05.000Z"}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %q", s)
}

// filterColumns 过滤出表中真实存在的列，并保证顺序稳定。
// 列名会经过 resolveColumn 自适应（camelCase ↔ snake_case）。
func (d *DB) filterColumns(ctx context.Context, table string, m map[string]any) ([]string, []any) {
	cols := d.Columns(ctx, table)

	resolved := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		col := resolveColumn(cols, k)
		if col == "" {
			// 表里没有这一列，跳过而不是整条失败
			continue
		}
		resolved[col] = v
	}

	keys := make([]string, 0, len(resolved))
	for k := range resolved {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	vals := make([]any, len(keys))
	for i, k := range keys {
		vals[i] = normalizeValue(resolved[k])
	}
	return keys, vals
}

// normalizeValue 把 Go 值转成 SQLite 能存的形式。
func normalizeValue(v any) any {
	switch t := v.(type) {
	case bool:
		if t {
			return 1
		}
		return 0
	case time.Time:
		return FormatTime(t)
	case []string:
		return strings.Join(t, ",")
	default:
		return v
	}
}

// ---------------- 补列迁移 ----------------

// schemaColumn 是 schema.sql 里的一列声明。
type schemaColumn struct {
	name string
	def  string // 类型 + 约束，直接拼进 ALTER TABLE
}

// ensureColumns 把 schema.sql 里新增、但库里还没有的列补上。
//
// CREATE TABLE IF NOT EXISTS 对已存在的表是空操作，所以升级自身版本
// 或接管一个旧版官方库时，新增列不会自动出现。这里按声明补齐，
// 保证「新增一个字段就只需改 schema.sql」。
func (d *DB) ensureColumns(ctx context.Context) error {
	script, err := d.schemaScript()
	if err != nil {
		return err
	}

	for _, t := range parseSchemaColumns(script) {
		if ok, err := d.tableExists(ctx, t.table); err != nil || !ok {
			continue
		}

		d.mu.Lock()
		delete(d.cols, t.table) // 丢掉旧缓存，重新读一次
		d.mu.Unlock()
		have := d.Columns(ctx, t.table)

		for _, c := range t.columns {
			if have[c.name] {
				continue
			}
			// 自增主键列不能用 ADD COLUMN 补，且它不可能缺失
			if strings.Contains(strings.ToUpper(c.def), "AUTO_INCREMENT") {
				continue
			}
			q := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s",
				d.quoteIdent(t.table), d.quoteIdent(c.name), c.def)
			if _, err := d.exec(ctx, q); err != nil {
				logx.Warnf("store", "补列 %s.%s 失败: %v", t.table, c.name, err)
				continue
			}
			logx.Infof("store", "已补列 %s.%s %s", t.table, c.name, c.def)
		}

		d.mu.Lock()
		delete(d.cols, t.table)
		d.mu.Unlock()
	}
	return nil
}

// quoteIdent 按方言包裹标识符，并做白名单过滤防注入。
func (d *DB) quoteIdent(s string) string {
	s = sanitizeIdent(s)
	if d.isMySQL() {
		return "`" + s + "`"
	}
	return "[" + s + "]"
}

type schemaTable struct {
	table   string
	columns []schemaColumn
}

// parseSchemaColumns 从建表脚本里抽出「表 → 列声明」。
// 只认 schema.sql 的固定写法（每列一行、以逗号分隔），够用且不会误判。
func parseSchemaColumns(script string) []schemaTable {
	var (
		out []schemaTable
		cur *schemaTable
	)

	// 跳过这些不是列的行首关键字
	skip := map[string]bool{
		"PRIMARY": true, "FOREIGN": true, "UNIQUE": true, "CHECK": true,
		"CONSTRAINT": true, "CREATE": true, "KEY": true, "INDEX": true,
	}

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "CREATE TABLE") {
			if name, ok := createTableName(trimmed); ok {
				out = append(out, schemaTable{table: name})
				cur = &out[len(out)-1]
			}
			continue
		}

		if cur == nil {
			continue
		}

		if trimmed == ");" || strings.HasPrefix(trimmed, ");") {
			cur = nil
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		col := strings.Trim(fields[0], "[]`")
		if skip[strings.ToUpper(col)] {
			continue
		}

		def := strings.TrimSpace(strings.TrimSuffix(trimmed[len(fields[0]):], ","))
		if def == "" {
			continue
		}
		cur.columns = append(cur.columns, schemaColumn{name: col, def: def})
	}

	return out
}
