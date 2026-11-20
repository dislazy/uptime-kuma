// Package store 是数据访问层：SQLite 连接、schema 初始化、以及各表的读写。
//
// 关键设计：所有写入都通过 map[string]any 完成，并在写入前用 PRAGMA table_info
// 过滤掉表中不存在的列。这样带来两个好处：
//  1. 老库（缺字段）不会因为 INSERT 带上新列而整条失败；
//  2. 官方后续新增字段时，Go 侧无需改代码即可继续工作。
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，免 CGO

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
)

//go:embed schema.sql
var schemaFS embed.FS

// DB 包装 *sql.DB，并缓存各表列名。
type DB struct {
	db   *sql.DB
	path string

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

	return &DB{db: sqlDB, path: path, cols: map[string]map[string]bool{}, table: map[string]bool{}}, nil
}

// SQL 暴露底层连接，供少数需要事务/自定义 SQL 的地方使用。
func (d *DB) SQL() *sql.DB { return d.db }

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// Close 关闭连接。
func (d *DB) Close() error { return d.db.Close() }

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
		// 老库可能缺后来新增的列，按 schema.sql 补齐
		if err := d.ensureColumns(ctx); err != nil {
			logx.Warnf("store", "补列迁移未完成: %v", err)
		}
		d.warmCache(ctx)
		d.reportMissing(ctx)
		return nil
	}

	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("读取 schema.sql 失败: %w", err)
	}

	logx.Infof("store", "初始化数据库: %s", d.path)
	for _, stmt := range splitStatements(string(raw)) {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行建表语句失败: %w\nSQL: %s", err, truncate(stmt, 200))
		}
	}

	d.warmCache(ctx)
	return nil
}

func (d *DB) ensureMetaTable(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS [_gokuma_meta] (
		[key]   VARCHAR(100) PRIMARY KEY,
		value   TEXT
	)`)
	return err
}

func (d *DB) tableExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// warmCache 预读表与列信息。
func (d *DB) warmCache(ctx context.Context) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
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
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("读取 schema.sql 失败: %w", err)
	}

	for _, t := range parseSchemaColumns(string(raw)) {
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
			q := fmt.Sprintf("ALTER TABLE [%s] ADD COLUMN [%s] %s",
				sanitizeIdent(t.table), sanitizeIdent(c.name), c.def)
			if _, err := d.db.ExecContext(ctx, q); err != nil {
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
		"CONSTRAINT": true, "CREATE": true, "KEY": true,
	}

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "CREATE TABLE") {
			name := ""
			if i := strings.Index(trimmed, "["); i >= 0 {
				if j := strings.Index(trimmed[i:], "]"); j > 0 {
					name = trimmed[i+1 : i+j]
				}
			}
			if name != "" {
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
		col := strings.Trim(fields[0], "[]")
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
