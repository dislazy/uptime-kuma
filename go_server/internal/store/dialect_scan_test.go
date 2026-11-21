package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestOpenMariaDBUnreachable 验证连不上数据库时给出的是明确错误，而不是 panic 或长时间挂起。
// 用 127.0.0.1:1（必然 ECONNREFUSED），不需要真实数据库。
func TestOpenMariaDBUnreachable(t *testing.T) {
	_, err := OpenMariaDB(MariaDBConfig{
		Hostname: "127.0.0.1",
		Port:     1,
		DBName:   "kuma",
		Username: "nobody",
		Password: "x",
		MaxConns: 1,
	})
	if err == nil {
		t.Fatal("连不上时应当返回错误")
	}
	if !strings.Contains(err.Error(), "连接数据库服务器失败") {
		t.Errorf("错误文案应明确指出连不上数据库服务器，got: %v", err)
	}
}

// dialectExemptFiles 是允许出现 SQLite 专属语法的文件：
// 它们的 SQLite 语句都关在 isMySQL() 分支里，是刻意的方言实现。
var dialectExemptFiles = map[string]bool{
	"db.go":    true, // PRAGMA table_info / sqlite_master，均在 sqlite 分支
	"stats.go": true, // ON CONFLICT / excluded. / MAX()，均在 sqlite 分支
}

var (
	sqlLikeRe   = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|CREATE TABLE|ALTER TABLE|PRAGMA|WITH)\b`)
	castIntRe   = regexp.MustCompile(`(?i)CAST\s*\([^)]*AS\s+INTEGER\s*\)`)
	sqliteWords = []string{
		"PRAGMA ",
		"sqlite_master",
		"ON CONFLICT",
		"excluded.",
		"AUTOINCREMENT",
		"INSERT OR REPLACE",
		"INSERT OR IGNORE",
		"DATETIME('now')",
		"last_insert_rowid",
		"julianday",
		"GROUP_CONCAT",
		"COLLATE NOCASE",
		"WITHOUT ROWID",
	}
	// 子查询必须带别名（MySQL 1248），SQLite 不要求。
	derivedTableRe = regexp.MustCompile(`(?is)FROM\s*\(\s*SELECT`)
	derivedAliasRe = regexp.MustCompile(`(?is)\)\s*(AS\s+[` + "`" + `\w]+|\w+)\s*(WHERE|ORDER|LIMIT|GROUP|$|\n)`)
)

// TestNoSQLiteOnlySQLInSources 静态扫描 internal/ 下的 Go 源码，
// 找出 SQL 字符串里的 SQLite 专属语法和 MySQL 严格模式会拒绝的写法。
//
// 目的是防止后续维护时在 handlers / store 其它文件里写出只在 SQLite 下能跑的 SQL——
// 那种错误在没有 MySQL 环境时不可见，上线才炸。
func TestNoSQLiteOnlySQLInSources(t *testing.T) {
	root := ".."
	fset := token.NewFileSet()
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if dialectExemptFiles[filepath.Base(path)] {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !sqlLikeRe.MatchString(s) {
				return true
			}
			scanned++

			pos := fset.Position(lit.Pos())
			for _, issue := range sqlDialectIssues(s) {
				t.Errorf("%s:%d %s", filepath.Base(path), pos.Line, issue)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if scanned < 50 {
		t.Fatalf("只扫到 %d 条 SQL，扫描逻辑可能失效", scanned)
	}
	t.Logf("已检查 %d 条 SQL 字面量", scanned)
}

// sqlDialectIssues 返回一条 SQL 里在 MySQL 下会出问题的地方。
func sqlDialectIssues(s string) []string {
	var issues []string
	for _, kw := range sqliteWords {
		if strings.Contains(s, kw) {
			issues = append(issues, "SQL 含 SQLite 专属写法 "+strconv.Quote(kw)+"（MySQL 不支持）")
		}
	}
	if castIntRe.MatchString(s) {
		issues = append(issues, "SQL 含 CAST(... AS INTEGER)，MySQL 需用 SIGNED")
	}
	if derivedTableRe.MatchString(s) && !derivedAliasRe.MatchString(s) {
		issues = append(issues, "派生表缺少别名（MySQL 报 1248 Every derived table must have its own alias）")
	}
	return issues
}

// TestSQLDialectCheckerSelfTest 保证上面的检查器不是空转：
// 对已知坏写法必须报错，对改好的写法必须放行。
func TestSQLDialectCheckerSelfTest(t *testing.T) {
	bad := []string{
		"SELECT * FROM x WHERE a = 1 ON CONFLICT(a) DO UPDATE SET b = excluded.b",
		"INSERT OR REPLACE INTO x (a) VALUES (?)",
		"SELECT CAST(timestamp / 60 AS INTEGER) FROM stat_minutely",
		"SELECT * FROM (SELECT * FROM heartbeat ORDER BY time DESC LIMIT ?) ORDER BY time ASC",
		"SELECT name FROM sqlite_master WHERE type='table'",
		"CREATE TABLE IF NOT EXISTS `x` (`id` INTEGER PRIMARY KEY AUTOINCREMENT)",
	}
	for _, s := range bad {
		if len(sqlDialectIssues(s)) == 0 {
			t.Errorf("检查器漏报了坏 SQL: %s", s)
		}
	}

	good := []string{
		"INSERT INTO stat_minutely (a, b) VALUES (?, ?) ON DUPLICATE KEY UPDATE b = VALUES(b)",
		"SELECT CAST(timestamp / 60 AS SIGNED) * 60 AS bucket FROM stat_minutely",
		"SELECT * FROM (SELECT * FROM heartbeat ORDER BY time DESC LIMIT ?) AS t ORDER BY time ASC",
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()",
	}
	for _, s := range good {
		if got := sqlDialectIssues(s); len(got) != 0 {
			t.Errorf("检查器误报了正常 SQL: %s → %v", s, got)
		}
	}
}
