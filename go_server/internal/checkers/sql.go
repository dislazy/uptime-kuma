package checkers

import (
	"context"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// 本文件是四个 SQL 类监控。它们的探测流程完全一致（连库 → 执行一条语句 → 数行数），
// 差别只在连接串格式，所以共用 runSQLProbe，各自只负责把连接串翻译成驱动认的形态。

// ---------------- MySQL / MariaDB ----------------

// MySQLChecker 连库执行查询，成功时报告返回行数。
type MySQLChecker struct{}

// Type 实现 Checker。
func (MySQLChecker) Type() string { return "mysql" }

func init() { Register(MySQLChecker{}) }

// Check 实现 Checker。
func (MySQLChecker) Check(ctx context.Context, m *model.Monitor) Result {
	// 官方沿用 radius_password 这一列存 MySQL 密码（历史命名），这里保持一致
	dsn, err := mysqlDSN(m.Get("database_connection_string"), m.Get("radius_password"))
	if err != nil {
		return Down("%s", err.Error())
	}

	return runSQLConditional(ctx, "mysql", dsn, defaultQuery(m, "SELECT 1"), m.Get("conditions"))
}

// ---------------- PostgreSQL ----------------

// PostgresChecker 连库执行查询。
type PostgresChecker struct{}

// Type 实现 Checker。
func (PostgresChecker) Type() string { return "postgres" }

func init() { Register(PostgresChecker{}) }

// Check 实现 Checker。
func (PostgresChecker) Check(ctx context.Context, m *model.Monitor) Result {
	dsn := m.Get("database_connection_string")
	if dsn == "" {
		return Down("Connection string is empty")
	}

	start := time.Now()
	_, err := runSQLProbe(ctx, "pgx", dsn, defaultQuery(m, "SELECT 1"))
	ping := time.Since(start).Milliseconds()

	if err != nil {
		// 官方 postgres 直接透出驱动原始错误，不做包装
		return Down("%s", err.Error())
	}

	// 官方此处 heartbeat.msg 留空，前端不显示消息，保持一致
	return Up("", ping)
}

// ---------------- Microsoft SQL Server ----------------

// SQLServerChecker 连库执行查询。
//
// 注意注册名是 sqlserver 而不是 mssql：前端 <option value="sqlserver">，
// 官方内部 name 也是 sqlserver，用 mssql 会匹配不到。
type SQLServerChecker struct{}

// Type 实现 Checker。
func (SQLServerChecker) Type() string { return "sqlserver" }

func init() {
	c := SQLServerChecker{}
	Register(c)
	// 兼容用户手改库或旧数据里写成 mssql 的情况
	Register(aliasChecker{base: c, name: "mssql"})
}

// Check 实现 Checker。
func (SQLServerChecker) Check(ctx context.Context, m *model.Monitor) Result {
	dsn, err := sqlserverDSN(m.Get("database_connection_string"))
	if err != nil {
		return Down("%s", err.Error())
	}

	return runSQLConditional(ctx, "sqlserver", dsn, defaultQuery(m, "SELECT 1"), m.Get("conditions"))
}

// ---------------- Oracle ----------------

// OracleChecker 连库执行查询。
type OracleChecker struct{}

// Type 实现 Checker。
func (OracleChecker) Type() string { return "oracledb" }

func init() { Register(OracleChecker{}) }

// Check 实现 Checker。
func (OracleChecker) Check(ctx context.Context, m *model.Monitor) Result {
	// Oracle 的账号密码在独立输入框里，落到 basic_auth_user / basic_auth_pass 两列
	dsn, err := oracleDSN(
		m.Get("database_connection_string"),
		m.Get("basic_auth_user"),
		m.Get("basic_auth_pass"),
	)
	if err != nil {
		return Down("%s", err.Error())
	}

	return runSQLConditional(ctx, "oracle", dsn, defaultQuery(m, "SELECT 1 FROM DUAL"), m.Get("conditions"))
}
