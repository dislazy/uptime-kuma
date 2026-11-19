package checkers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	// SQL 驱动在此集中注册，新增一个数据库监控只需在这里加一行 import
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "github.com/sijms/go-ora/v2"

	"github.com/songbo/uptime-kuma/go_server/internal/conditions"
	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// runSQLProbe 连库并执行一条语句，返回结果行数。
//
// 刻意不做单独的 Ping：Query 本身会隐式建连，省一次往返，
// 对低配机器上的短间隔监控有实际意义。
func runSQLProbe(ctx context.Context, driver, dsn, query string) (int64, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var n int64
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	return n, nil
}

// defaultQuery 取监控里配置的查询，没填就用默认的连通性语句。
func defaultQuery(m *model.Monitor, fallback string) string {
	if q := strings.TrimSpace(m.Get("database_query")); q != "" {
		return q
	}
	return fallback
}

// ---------------- 连接串格式转换 ----------------

// mysqlDSN 把 mysql:// 形式的 URI 转成 go-sql-driver 认的 DSN。
//
// go-sql-driver 只接受 user:pass@tcp(host:port)/db 这种写法，
// 但前端连接串模板给的是 URI，必须转一道。
func mysqlDSN(connStr, passwordOverride string) (string, error) {
	connStr = strings.TrimSpace(connStr)
	if connStr == "" {
		return "", fmt.Errorf("Connection string is empty")
	}

	// 已经是原生 DSN（不含 ://）就原样用
	if !strings.Contains(connStr, "://") {
		return applyMySQLPasswordOverride(connStr, passwordOverride), nil
	}

	u, err := url.Parse(connStr)
	if err != nil {
		return "", fmt.Errorf("invalid connection string: %w", err)
	}
	if strings.ToLower(u.Scheme) != "mysql" {
		return "", fmt.Errorf("unsupported mysql scheme %q", u.Scheme)
	}

	var b strings.Builder
	if u.User != nil {
		b.WriteString(u.User.Username())
		pass := passwordOverride
		if pass == "" {
			pass, _ = u.User.Password()
		}
		if pass != "" {
			b.WriteString(":")
			b.WriteString(pass)
		}
		b.WriteString("@")
	} else if passwordOverride != "" {
		return "", fmt.Errorf("connection string has no username to pair with the password")
	}

	b.WriteString("tcp(")
	b.WriteString(ensurePort(u.Host, "3306"))
	b.WriteString(")")
	b.WriteString(u.Path)

	// 查询参数原样透传（tls / charset / parseTime 等由用户自己控制）
	q := u.Query()
	if len(q) > 0 {
		b.WriteString("?")
		b.WriteString(q.Encode())
	}

	return b.String(), nil
}

// applyMySQLPasswordOverride 在原生 DSN 上替换密码。
func applyMySQLPasswordOverride(dsn, passwordOverride string) string {
	if passwordOverride == "" {
		return dsn
	}
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	cred := dsn[:at]
	rest := dsn[at:]

	colon := strings.Index(cred, ":")
	if colon < 0 {
		return cred + ":" + passwordOverride + rest
	}
	return cred[:colon] + ":" + passwordOverride + rest
}

// sqlserverDSN 把 ADO 连接串转成 go-mssqldb 的 URL 形式。
//
// 前端模板给的是 ADO 串（Server=..;Database=..;User Id=..），
// 而 go-mssqldb 只认 sqlserver:// URL，两者必须转换。
func sqlserverDSN(connStr string) (string, error) {
	connStr = strings.TrimSpace(connStr)
	if connStr == "" {
		return "", fmt.Errorf("Connection string is empty")
	}

	if strings.HasPrefix(strings.ToLower(connStr), "sqlserver://") {
		return connStr, nil
	}

	kv := parseADO(connStr)
	if len(kv) == 0 {
		return "", fmt.Errorf("cannot parse connection string, expected sqlserver:// URL or ADO format")
	}

	server := kv["server"]
	if server == "" {
		server = kv["data source"]
	}
	if server == "" {
		return "", fmt.Errorf("Server is required in the connection string")
	}

	host, port := splitServerPort(server, "1433")

	user := firstNonEmpty(kv["user id"], kv["uid"], kv["user"])
	pass := firstNonEmpty(kv["password"], kv["pwd"])

	u := url.URL{Scheme: "sqlserver", Host: net.JoinHostPort(host, port)}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}

	q := url.Values{}
	if db := firstNonEmpty(kv["database"], kv["initial catalog"]); db != "" {
		q.Set("database", db)
	}
	if enc := kv["encrypt"]; enc != "" {
		q.Set("encrypt", normalizeMSSQLBool(enc))
	}
	if tsc := kv["trustservercertificate"]; tsc != "" {
		q.Set("trustservercertificate", normalizeMSSQLBool(tsc))
	}
	if to := kv["connection timeout"]; to != "" {
		q.Set("dial timeout", to+"s")
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}

// oracleDSN 拼出 go-ora 需要的 URL。
//
// 前端模板是 localhost:1521/FREEPDB1 这种裸地址（用户名密码在独立输入框里），
// go-ora 要的是 oracle://user:pass@host:port/service。
func oracleDSN(connStr, user, pass string) (string, error) {
	connStr = strings.TrimSpace(connStr)
	if connStr == "" {
		return "", fmt.Errorf("Connection string is empty")
	}

	if strings.HasPrefix(strings.ToLower(connStr), "oracle://") {
		u, err := url.Parse(connStr)
		if err != nil {
			return "", fmt.Errorf("invalid connection string: %w", err)
		}
		if u.User == nil && user != "" {
			u.User = url.UserPassword(user, pass)
		}
		return u.String(), nil
	}

	// 裸地址：host:port/service 或 host/service
	hostPart, service := connStr, ""
	if i := strings.Index(connStr, "/"); i >= 0 {
		hostPart = connStr[:i]
		service = connStr[i+1:]
	}

	host, port := splitServerPort(hostPart, "1521")

	u := url.URL{Scheme: "oracle"}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	u.Host = net.JoinHostPort(host, port)
	if service != "" {
		u.Path = "/" + service
	}

	return u.String(), nil
}

// parseADO 解析 key=value; 形式的连接串，键统一小写。
func parseADO(connStr string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(connStr, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:i]))
		val := strings.TrimSpace(part[i+1:])
		out[key] = val
	}
	return out
}

// splitServerPort 拆分 host,port / host\instance,port 形式，缺端口时用默认值。
func splitServerPort(server, defaultPort string) (string, string) {
	server = strings.TrimSpace(server)

	// 先剥掉实例名（host\SQLEXPRESS），go-mssqldb 用 instance 参数表达，
	// 这里先只取主机部分，避免把反斜杠塞进 host 导致解析失败
	if i := strings.Index(server, "\\"); i >= 0 {
		server = server[:i]
	}

	if i := strings.LastIndex(server, ","); i >= 0 {
		host := strings.TrimSpace(server[:i])
		port := strings.TrimSpace(server[i+1:])
		if host != "" && port != "" {
			return host, port
		}
	}

	return server, defaultPort
}

// normalizeMSSQLBool 把 ADO 的 Yes/No/True/False 转成 go-mssqldb 认的字面量。
func normalizeMSSQLBool(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1", "mandatory":
		return "true"
	case "no", "false", "0", "disable", "optional":
		return "false"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// runSQLValue 执行查询并取第一行第一列，供条件求值使用。
//
// 条件表达式只能针对单个值判定，所以这里要明确拒绝多行/多列，
// 否则用户会看到一个"随便取了某个值"的诡异结果。
func runSQLValue(ctx context.Context, driver, dsn, query string) (string, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if len(cols) > 1 {
		return "", fmt.Errorf("Multiple columns were found, expected only one value")
	}

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("Query returned no results")
	}

	var v any
	if err := rows.Scan(&v); err != nil {
		return "", err
	}

	if rows.Next() {
		return "", fmt.Errorf("Multiple values were found, expected only one value")
	}

	return cellString(v), nil
}

// runSQLConditional 是 mysql / sqlserver / oracledb 共用的探测流程。
//
// 配了条件就取单值并求值，没配就只数行数——这也是官方在引入条件系统后
// 保留的兼容路径。
func runSQLConditional(ctx context.Context, driver, dsn, query, rawCond string) Result {
	start := time.Now()

	if conditions.Has(rawCond) {
		val, err := runSQLValue(ctx, driver, dsn, query)
		ping := time.Since(start).Milliseconds()
		if err != nil {
			return Down("Database connection/query failed: %s", err.Error())
		}

		ok, err := conditions.Eval(rawCond, map[string]any{"result": val})
		if err != nil {
			return Down("%s", err.Error())
		}
		if !ok {
			return Down("Query result did not meet the specified conditions (%s)", val)
		}
		return Up("Query did meet specified conditions", ping)
	}

	n, err := runSQLProbe(ctx, driver, dsn, query)
	ping := time.Since(start).Milliseconds()
	if err != nil {
		return Down("Database connection/query failed: %s", err.Error())
	}
	return Up(fmt.Sprintf("Rows: %d", n), ping)
}

// cellString 把驱动返回的列值转成字符串。
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case string:
		return t
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", t)
	}
}
