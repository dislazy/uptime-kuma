package store

import (
	"os"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

// 这两个测试是 MySQL 支持的安全网：没有真库可连，就只能靠结构比对
// 和语法残留扫描来保证两份 schema 不会漂移、代码里不会漏掉方言差异。

type parsedTable struct {
	name    string
	cols    map[string]bool
	indexes map[string]string // 索引名 -> 列名（逗号分隔，保序）
}

// parseSchemaFile 解析建表脚本，同时支持 SQLite 的 [x] 与 MySQL 的 `x`。
func parseSchemaFile(t *testing.T, path string) map[string]*parsedTable {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}

	tables := map[string]*parsedTable{}
	var cur *parsedTable

	for _, line := range strings.Split(string(raw), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "--") {
			continue
		}

		// 表外：CREATE [UNIQUE] INDEX ... ON <table> (<cols>)
		if cur == nil {
			if strings.HasPrefix(s, "CREATE INDEX") || strings.HasPrefix(s, "CREATE UNIQUE INDEX") {
				name, tbl, cols, ok := parseCreateIndex(s)
				if !ok {
					t.Fatalf("%s: 无法解析索引语句: %s", path, s)
				}
				pt := tables[tbl]
				if pt == nil {
					t.Fatalf("%s: 索引 %s 指向不存在的表 %s", path, name, tbl)
				}
				pt.indexes[name] = cols
				continue
			}
			if strings.HasPrefix(s, "CREATE TABLE") {
				name, ok := createTableName(s)
				if !ok {
					t.Fatalf("%s: 无法解析建表语句: %s", path, s)
				}
				if _, dup := tables[name]; dup {
					t.Fatalf("%s: 表 %s 重复定义", path, name)
				}
				cur = &parsedTable{name: name, cols: map[string]bool{}, indexes: map[string]string{}}
				tables[name] = cur
			}
			continue
		}

		// 表内结束
		if strings.HasPrefix(s, ");") {
			cur = nil
			continue
		}

		// 表内索引（MySQL 内联写法）
		upper := strings.ToUpper(s)
		if strings.HasPrefix(upper, "KEY ") ||
			strings.HasPrefix(upper, "UNIQUE KEY ") ||
			strings.HasPrefix(upper, "PRIMARY KEY ") ||
			strings.HasPrefix(upper, "INDEX ") ||
			strings.HasPrefix(upper, "UNIQUE INDEX ") {
			name, cols, ok := parseInlineIndex(s)
			if !ok {
				t.Fatalf("%s.%s: 无法解析内联索引: %s", path, cur.name, s)
			}
			if name != "" {
				cur.indexes[name] = cols
			}
			continue
		}

		// 列定义
		fields := strings.Fields(s)
		if len(fields) < 2 {
			continue
		}
		col := strings.Trim(fields[0], "[]`")
		if col == "" || strings.HasPrefix(col, "-") {
			continue
		}
		switch strings.ToUpper(col) {
		case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT", "CREATE", "KEY", "INDEX":
			continue
		}
		cur.cols[col] = true
	}

	return tables
}

// parseCreateIndex 解析 "CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON [t] (cols)"。
func parseCreateIndex(stmt string) (name, table, cols string, ok bool) {
	up := strings.ToUpper(stmt)
	i := strings.Index(up, " ON ")
	if i < 0 {
		return "", "", "", false
	}
	head := stmt[:i]
	clause := up[:i]

	// 索引名 = head 里最后一个标识符
	head = strings.TrimSpace(head)
	if j := strings.Index(head, "EXISTS "); j >= 0 {
		head = strings.TrimSpace(head[j+len("EXISTS "):])
	} else {
		switch {
		case strings.HasPrefix(clause, "CREATE UNIQUE INDEX "):
			head = strings.TrimSpace(head[len("CREATE UNIQUE INDEX "):])
		case strings.HasPrefix(clause, "CREATE INDEX "):
			head = strings.TrimSpace(head[len("CREATE INDEX "):])
		}
	}
	name = strings.Trim(head, "[]`")

	rest := strings.TrimSpace(stmt[i+len(" ON "):])
	tbl, ok := readIdent(rest)
	if !ok {
		return "", "", "", false
	}
	table = tbl

	open := strings.Index(rest, "(")
	close := strings.LastIndex(rest, ")")
	if open < 0 || close <= open {
		return "", "", "", false
	}
	cols = normalizeCols(rest[open+1 : close])
	if name == "" || cols == "" {
		return "", "", "", false
	}
	return name, table, cols, true
}

// parseInlineIndex 解析 MySQL 的 "KEY name (cols)" / "UNIQUE KEY name (cols)"。
// 表级 "PRIMARY KEY (cols)" 返回空名字，由调用方忽略。
func parseInlineIndex(stmt string) (name, cols string, ok bool) {
	open := strings.Index(stmt, "(")
	close := strings.LastIndex(stmt, ")")
	if open < 0 || close <= open {
		return "", "", false
	}
	head := strings.TrimSpace(stmt[:open])
	up := strings.ToUpper(head)
	if strings.HasPrefix(up, "PRIMARY KEY") {
		return "", normalizeCols(stmt[open+1 : close]), true
	}

	for _, prefix := range []string{"UNIQUE KEY ", "UNIQUE INDEX ", "KEY ", "INDEX "} {
		if strings.HasPrefix(up, prefix) {
			name = strings.Trim(strings.TrimSpace(head[len(prefix):]), "[]`")
			return name, normalizeCols(stmt[open+1 : close]), true
		}
	}
	return "", "", false
}

// normalizeCols 把 "`a`, `b`" 规整成 "a,b"。
func normalizeCols(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if c := strings.Trim(strings.TrimSpace(p), "[]`"); c != "" {
			out = append(out, c)
		}
	}
	return strings.Join(out, ",")
}

// TestSchemaParity 保证 schema.sql 与 schema_mysql.sql 是同一份逻辑 schema。
//
// 只比表名、列名、索引名与索引列顺序，不比类型——类型本来就要按方言翻译。
// 任何一边新增表/列/索引而另一边忘了改，这里会直接失败。
func TestSchemaParity(t *testing.T) {
	sqlite := parseSchemaFile(t, "schema.sql")
	mysql := parseSchemaFile(t, "schema_mysql.sql")

	if len(sqlite) == 0 || len(mysql) == 0 {
		t.Fatalf("解析结果为空: sqlite=%d mysql=%d", len(sqlite), len(mysql))
	}

	// 表名集合
	for name := range sqlite {
		if mysql[name] == nil {
			t.Errorf("schema_mysql.sql 缺少表 %s", name)
		}
	}
	for name := range mysql {
		if sqlite[name] == nil {
			t.Errorf("schema.sql 缺少表 %s", name)
		}
	}

	// 列与索引
	for name, st := range sqlite {
		mt := mysql[name]
		if mt == nil {
			continue
		}
		for c := range st.cols {
			if !mt.cols[c] {
				t.Errorf("%s: schema_mysql.sql 缺少列 %s", name, c)
			}
		}
		for c := range mt.cols {
			if !st.cols[c] {
				t.Errorf("%s: schema.sql 缺少列 %s（MySQL 侧多出）", name, c)
			}
		}
		for in, cols := range st.indexes {
			mc, ok := mt.indexes[in]
			if !ok {
				t.Errorf("%s: schema_mysql.sql 缺少索引 %s(%s)", name, in, cols)
				continue
			}
			if mc != cols {
				t.Errorf("%s: 索引 %s 列不一致 sqlite=%s mysql=%s", name, in, cols, mc)
			}
		}
		for in, cols := range mt.indexes {
			if _, ok := st.indexes[in]; !ok {
				t.Errorf("%s: schema.sql 缺少索引 %s(%s)（MySQL 侧多出）", name, in, cols)
			}
		}
	}
}

// TestMySQLSchemaHasNoSQLiteSyntax 扫描 MySQL 建表脚本，禁止出现 SQLite 专属写法。
func TestMySQLSchemaHasNoSQLiteSyntax(t *testing.T) {
	raw, err := os.ReadFile("schema_mysql.sql")
	if err != nil {
		t.Fatal(err)
	}

	// 去掉注释行后再扫，避免说明文字里的示例触发误报
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "--") {
			continue
		}
		lines = append(lines, l)
	}
	body := strings.Join(lines, "\n")

	forbidden := []string{
		"AUTOINCREMENT",   // MySQL 用 AUTO_INCREMENT
		"DATETIME('now')", // MySQL 用 CURRENT_TIMESTAMP
		"PRAGMA",          // MySQL 没有 PRAGMA
		"sqlite_",         // MySQL 没有 sqlite_master
		"ON CONFLICT",     // MySQL 用 ON DUPLICATE KEY UPDATE
		"excluded.",       // MySQL 用 VALUES(col)
		"CREATE INDEX",    // MySQL 的索引内联进 CREATE TABLE
		"REAL ",           // MySQL 用 DOUBLE
	}
	for _, kw := range forbidden {
		if strings.Contains(body, kw) {
			t.Errorf("schema_mysql.sql 残留 SQLite 写法: %s", kw)
		}
	}

	// 方括号是 SQLite 的标识符引号，MySQL 不认。
	// 只匹配 [ident] 形式，字符串字面量里的 ["200-299"] 不算。
	if bracketIdent.MatchString(body) {
		t.Errorf("schema_mysql.sql 含方括号标识符，MySQL 不认")
	}

	// 反引号必须成对
	if n := strings.Count(body, "`"); n%2 != 0 {
		t.Errorf("schema_mysql.sql 反引号数量为奇数(%d)，存在未闭合标识符", n)
	}

	// TEXT 列不能做 PRIMARY KEY（MySQL 限制）
	for _, l := range lines {
		up := strings.ToUpper(strings.TrimSpace(l))
		if strings.Contains(up, "TEXT") && strings.Contains(up, "PRIMARY KEY") {
			t.Errorf("TEXT 列不能做主键: %s", strings.TrimSpace(l))
		}
	}
}

// TestMariaDBDSN 检查连接串拼装：特殊字符、socket、TLS 都要能正确往返。
func TestMariaDBDSN(t *testing.T) {
	base := MariaDBConfig{
		Hostname: "db.internal",
		Port:     3307,
		DBName:   "kuma",
		Username: "kuma",
		Password: "p@ss:w/rd?#&",
		MaxConns: 10,
	}

	dsn, err := mariadbDSN(base)
	if err != nil {
		t.Fatalf("拼装 DSN 失败: %v", err)
	}

	cfg, err := parseDSNForTest(dsn)
	if err != nil {
		t.Fatalf("DSN 无法被驱动解析: %v\nDSN: %s", err, dsn)
	}
	if cfg.Passwd != base.Password {
		t.Errorf("密码未正确转义: got %q want %q", cfg.Passwd, base.Password)
	}
	if cfg.Addr != "db.internal:3307" {
		t.Errorf("地址错误: %s", cfg.Addr)
	}
	if cfg.DBName != "kuma" {
		t.Errorf("库名错误: %s", cfg.DBName)
	}
	if cfg.ParseTime {
		t.Errorf("ParseTime 必须为 false，否则 DATETIME 会被驱动二次时区换算")
	}
	if got := cfg.Params["time_zone"]; got != "'+00:00'" {
		t.Errorf("time_zone 必须固定为 UTC，got %q", got)
	}
	// charset 是驱动的原生参数，ParseDSN 不会把它留在 Params 里，
	// 只能从 DSN 字符串上确认。
	if !strings.Contains(dsn, "charset=utf8mb4") {
		t.Errorf("DSN 缺少 charset=utf8mb4: %s", dsn)
	}

	// unix socket：忽略 host/port
	sock := base
	sock.Socket = "/run/mysqld/mysqld.sock"
	dsn, err = mariadbDSN(sock)
	if err != nil {
		t.Fatalf("socket 模式拼装失败: %v", err)
	}
	cfg, err = parseDSNForTest(dsn)
	if err != nil {
		t.Fatalf("socket DSN 解析失败: %v", err)
	}
	if cfg.Net != "unix" || cfg.Addr != sock.Socket {
		t.Errorf("socket 模式错误: net=%s addr=%s", cfg.Net, cfg.Addr)
	}

	// TLS 无 CA → tls=true
	tlsCfg := base
	tlsCfg.SSL = true
	dsn, err = mariadbDSN(tlsCfg)
	if err != nil {
		t.Fatalf("TLS 模式拼装失败: %v", err)
	}
	cfg, err = parseDSNForTest(dsn)
	if err != nil {
		t.Fatalf("TLS DSN 解析失败: %v", err)
	}
	if cfg.TLSConfig != "true" {
		t.Errorf("TLS 无 CA 时应为 true，got %q", cfg.TLSConfig)
	}
}

// TestRewriteSQL 检查 [ident] → `ident` 的改写，以及 sqlite 方言下不生效。
func TestRewriteSQL(t *testing.T) {
	my := &DB{dialect: dialectMariaDB}
	sq := &DB{dialect: dialectSQLite}

	in := "SELECT * FROM [user] WHERE [key] = ? AND name LIKE ?"
	want := "SELECT * FROM `user` WHERE `key` = ? AND name LIKE ?"
	if got := my.rewriteSQL(in); got != want {
		t.Errorf("mysql 改写错误:\n got %s\nwant %s", got, want)
	}
	if got := sq.rewriteSQL(in); got != in {
		t.Errorf("sqlite 不应改写，got %s", got)
	}

	// 不含方括号的 SQL 原样返回
	plain := "INSERT INTO heartbeat (monitor_id) VALUES (?)"
	if got := my.rewriteSQL(plain); got != plain {
		t.Errorf("无方括号时不应改动，got %s", got)
	}

	// 字符串字面量里的方括号不能被当成标识符
	lit := "SELECT '[\"200-299\"]' AS x FROM [monitor]"
	wantLit := "SELECT '[\"200-299\"]' AS x FROM `monitor`"
	if got := my.rewriteSQL(lit); got != wantLit {
		t.Errorf("字面量被误改:\n got %s\nwant %s", got, wantLit)
	}
}

// TestMySQLUpsertSQL 断言统计聚合的 UPSERT 语句按方言生成正确。
func TestMySQLUpsertSQL(t *testing.T) {
	// 这里只验证拼接结果的关键特征，不连库。
	my := &DB{dialect: dialectMariaDB}
	if !strings.Contains(upsertMinutelySQL(my), "ON DUPLICATE KEY UPDATE") {
		t.Error("mysql 的 UpsertMinutely 应使用 ON DUPLICATE KEY UPDATE")
	}
	if !strings.Contains(upsertMinutelySQL(my), "GREATEST(") {
		t.Error("mysql 的 UpsertMinutely 必须用 GREATEST（MAX 是聚合函数）")
	}
	if strings.Contains(upsertMinutelySQL(my), "excluded.") {
		t.Error("mysql 不应出现 excluded.")
	}

	sq := &DB{dialect: dialectSQLite}
	if !strings.Contains(upsertMinutelySQL(sq), "ON CONFLICT") {
		t.Error("sqlite 的 UpsertMinutely 应保持 ON CONFLICT")
	}
	if !strings.Contains(upsertMinutelySQL(sq), "MAX(") {
		t.Error("sqlite 的 UpsertMinutely 应使用 MAX")
	}
}

func parseDSNForTest(dsn string) (*gomysql.Config, error) {
	return gomysql.ParseDSN(dsn)
}
