package store

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// TestMariaDBIntegration 连一台真实的 MySQL/MariaDB 跑完整流程：首次建库、
// 表结构落地、读写保真、聚合 UPSERT、重启接管。
//
// 默认跳过——本仓库的默认测试不依赖外部服务。要跑就设环境变量：
//
//	KUMA_TEST_MARIADB_HOST=mysql.example.com \
//	KUMA_TEST_MARIADB_PORT=3306 \
//	KUMA_TEST_MARIADB_NAME=uptime_test \
//	KUMA_TEST_MARIADB_USER=uptime_test_user \
//	KUMA_TEST_MARIADB_PASS=*** \
//	go test ./internal/store/ -run TestMariaDBIntegration -v
//
// 这个测试会 DROP 掉目标库里所有表，务必指向一次性测试库。
func TestMariaDBIntegration(t *testing.T) {
	cfg := mariadbTestConfig(t)
	ctx := context.Background()

	// 冲掉上一轮的残留，保证"首次初始化"这条路径每次都真的走过
	warm, err := OpenMariaDB(cfg)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	dropAllTables(t, warm, ctx)
	if err := warm.Close(); err != nil {
		t.Fatalf("关闭连接失败: %v", err)
	}

	// ---------- 1. 首次初始化 ----------
	db, err := OpenMariaDB(cfg)
	if err != nil {
		t.Fatalf("重新连接失败: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("首次建表失败: %v", err)
	}

	t.Run("表结构", func(t *testing.T) { assertSchemaLanded(t, db, ctx) })
	t.Run("时间与字符集", func(t *testing.T) { assertRoundTrip(t, db, ctx) })
	t.Run("聚合UPSERT", func(t *testing.T) { assertStatsUpsert(t, db, ctx) })
	t.Run("事务批量写", func(t *testing.T) { assertTxBatch(t, db, ctx) })

	// ---------- 2. 重启接管 ----------
	// 表已存在，Migrate 应走"接管"分支：不重建、不报错、数据不丢
	if err := db.Close(); err != nil {
		t.Fatalf("关闭连接失败: %v", err)
	}

	again, err := OpenMariaDB(cfg)
	if err != nil {
		t.Fatalf("重连失败: %v", err)
	}
	defer again.Close()

	if err := again.Migrate(ctx); err != nil {
		t.Fatalf("接管既有库失败: %v", err)
	}

	s := NewSettings(again)
	if v := s.GetString(ctx, "integration_marker", ""); v != "留存" {
		t.Errorf("重启后数据丢失：integration_marker = %q", v)
	}

	row, err := again.QueryOne(ctx, "SELECT COUNT(*) AS n FROM heartbeat WHERE monitor_id = 1")
	if err != nil || row == nil {
		t.Fatalf("重启后读取心跳失败: %v", err)
	}
	beats := row.Int("n")
	if beats != 2 {
		t.Errorf("重启后心跳条数不对：want 2 got %d", beats)
	}
}

func mariadbTestConfig(t *testing.T) MariaDBConfig {
	t.Helper()
	host := os.Getenv("KUMA_TEST_MARIADB_HOST")
	if host == "" {
		t.Skip("未设置 KUMA_TEST_MARIADB_HOST，跳过真实 MySQL 集成测试")
	}
	port := 3306
	if v := os.Getenv("KUMA_TEST_MARIADB_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("KUMA_TEST_MARIADB_PORT 非法: %v", err)
		}
		port = p
	}
	name := os.Getenv("KUMA_TEST_MARIADB_NAME")
	if name == "" {
		t.Fatal("KUMA_TEST_MARIADB_NAME 为空")
	}
	return MariaDBConfig{
		Hostname: host,
		Port:     port,
		DBName:   name,
		Username: os.Getenv("KUMA_TEST_MARIADB_USER"),
		Password: os.Getenv("KUMA_TEST_MARIADB_PASS"),
		MaxConns: 5,
	}
}

// dropAllTables 清空当前库的所有表。用 SET FOREIGN_KEY_CHECKS=0 兜底，
// 免得以后加了外键就删不动。
func dropAllTables(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()

	rows, err := db.query(ctx, `SELECT table_name FROM information_schema.tables
	                             WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("列出表失败: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("读取表名失败: %v", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表名失败: %v", err)
	}

	if _, err := db.exec(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("关闭外键检查失败: %v", err)
	}
	for _, n := range names {
		if _, err := db.exec(ctx, "DROP TABLE IF EXISTS `"+n+"`"); err != nil {
			t.Fatalf("删除表 %s 失败: %v", n, err)
		}
	}
	if _, err := db.exec(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("恢复外键检查失败: %v", err)
	}
	t.Logf("已清空 %d 张表", len(names))
}

// assertSchemaLanded 用 information_schema 读回真实结构，与 schema_mysql.sql 比对。
// 这一步验证的是"DDL 在真库上真的能执行"，静态测试只能验证两份脚本一致。
func assertSchemaLanded(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()

	want := parseSchemaFile(t, "schema_mysql.sql")

	rows, err := db.query(ctx, `SELECT table_name, column_name FROM information_schema.columns
	                             WHERE table_schema = DATABASE()`)
	if err != nil {
		t.Fatalf("读取列信息失败: %v", err)
	}
	got := map[string]map[string]bool{}
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			rows.Close()
			t.Fatalf("扫描列失败: %v", err)
		}
		if got[tbl] == nil {
			got[tbl] = map[string]bool{}
		}
		got[tbl][strings.ToLower(col)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列失败: %v", err)
	}

	for name, wt := range want {
		actual, ok := got[name]
		if !ok {
			t.Errorf("库中缺少表 %s", name)
			continue
		}
		var missing []string
		for col := range wt.cols {
			if !actual[strings.ToLower(col)] {
				missing = append(missing, col)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("表 %s 缺列: %s", name, strings.Join(missing, ", "))
		}
	}

	// _gokuma_meta 是 Go 侧自建的表，不在 schema 文件里；其余多出来的表就是异常
	for name := range got {
		if name == "_gokuma_meta" {
			continue
		}
		if _, ok := want[name]; !ok {
			t.Errorf("库中出现了 schema 里没有的表: %s", name)
		}
	}
	t.Logf("schema 校验通过：%d 张表", len(want))

	// 索引：内联 KEY 是否真的建出来了
	idxRows, err := db.query(ctx, `SELECT table_name, index_name FROM information_schema.statistics
	                                WHERE table_schema = DATABASE()`)
	if err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	gotIdx := map[string]map[string]bool{}
	for idxRows.Next() {
		var tbl, idx string
		if err := idxRows.Scan(&tbl, &idx); err != nil {
			idxRows.Close()
			t.Fatalf("扫描索引失败: %v", err)
		}
		if gotIdx[tbl] == nil {
			gotIdx[tbl] = map[string]bool{}
		}
		gotIdx[tbl][strings.ToLower(idx)] = true
	}
	idxRows.Close()
	if err := idxRows.Err(); err != nil {
		t.Fatalf("遍历索引失败: %v", err)
	}

	for name, wt := range want {
		for idx := range wt.indexes {
			if gotIdx[name] == nil || !gotIdx[name][strings.ToLower(idx)] {
				t.Errorf("表 %s 缺索引 %s", name, idx)
			}
		}
	}
}

// assertRoundTrip 验证两件最容易在 MySQL 上出偏差的事：
//  1. 毫秒精度：DATETIME(3) 是否真的保住了 .700，不会被四舍五入成下一秒；
//  2. 会话时区：DSN 的 time_zone='+00:00' 是否生效，写入的 UTC 文本原样读回；
//  3. 字符集：utf8mb4 能存 emoji。
func assertRoundTrip(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()

	row, err := db.QueryOne(ctx, "SELECT @@session.time_zone AS tz")
	if err != nil || row == nil {
		t.Fatalf("查询会话时区失败: %v", err)
	}
	if tz := row.Str("tz"); tz != "+00:00" {
		t.Errorf("会话时区应为 +00:00（否则全体时间偏 8 小时），got %q", tz)
	}

	const stamp = "2026-11-21 10:00:00.700"
	ping := int64(42)
	hb := NewHeartbeatStore(db)
	if _, err := hb.Save(ctx, model.Beat{
		MonitorID: 1, Status: 1, Msg: "OK", Time: stamp, Ping: &ping, Duration: 12, Important: true,
	}); err != nil {
		t.Fatalf("写心跳失败: %v", err)
	}

	back, err := hb.Recent(ctx, 1, 1)
	if err != nil {
		t.Fatalf("读心跳失败: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("应读回 1 条心跳，got %d", len(back))
	}
	if back[0].Time != stamp {
		t.Errorf("毫秒精度丢失：写入 %q 读回 %q（DATETIME 无精度会四舍五入到秒）", stamp, back[0].Time)
	}
	if back[0].Ping == nil || *back[0].Ping != ping {
		t.Errorf("ping 读回不一致: %+v", back[0].Ping)
	}

	st := NewSettings(db)
	if err := st.Set(ctx, "integration_marker", "留存", "general"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if err := st.Set(ctx, "integration_emoji", "🇨🇳 中文✅", "general"); err != nil {
		t.Fatalf("写 emoji 设置失败: %v", err)
	}
	// 清掉进程内缓存，逼它回库读
	st = NewSettings(db)
	if v := st.GetString(ctx, "integration_emoji", ""); v != "🇨🇳 中文✅" {
		t.Errorf("utf8mb4 读回不一致: %q", v)
	}
}

// assertStatsUpsert 打两次同一分钟的心跳，验证 ON DUPLICATE KEY UPDATE 与
// GREATEST 的算法与 SQLite 的 ON CONFLICT 完全一致。
func assertStatsUpsert(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()

	st := NewStatsStore(db)
	const minute = int64(1763700000) // 固定值，避免跨分钟边界

	if err := st.UpsertMinutely(ctx, 1, minute, 100, 1); err != nil {
		t.Fatalf("第一次聚合失败: %v", err)
	}
	if err := st.UpsertMinutely(ctx, 1, minute, 200, 1); err != nil {
		t.Fatalf("第二次聚合失败: %v", err)
	}

	w, err := st.Query(ctx, "stat_minutely", 1, minute, minute)
	if err != nil {
		t.Fatalf("查询聚合失败: %v", err)
	}
	if w.Up != 2 || w.Down != 0 {
		t.Errorf("计数错误：up=%d down=%d（want 2/0）", w.Up, w.Down)
	}
	// 与 SQLite 同算法：(100*1 + 200) / (1+1) = 150
	if w.Ping != 150 {
		t.Errorf("加权 ping 错误：got %v want 150（GREATEST 写错会在这里露出来）", w.Ping)
	}

	// 小时滚动：走 CAST AS SIGNED + ON DUPLICATE KEY UPDATE 那条路径
	if err := st.RollupHourly(ctx, minute+60); err != nil {
		t.Fatalf("小时聚合失败: %v", err)
	}
	hw, err := st.Query(ctx, "stat_hourly", 1, 0, minute+3600)
	if err != nil {
		t.Fatalf("查询小时聚合失败: %v", err)
	}
	if hw.Up != 2 || hw.Count != 1 {
		t.Errorf("小时桶错误：up=%d count=%d（want 2/1）", hw.Up, hw.Count)
	}
}

// assertTxBatch 走一遍事务 + PreparedStatement 的方言改写路径。
func assertTxBatch(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()

	if err := db.Tx(ctx, func(tx *Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO heartbeat (monitor_id, status, msg, time, duration, important, down_count)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		if _, err := stmt.ExecContext(ctx, 1, 1, "OK", "2026-11-21 10:00:01.000", 8, 0, 0); err != nil {
			return err
		}
		// 事务内也能用 [ident] 写法（执行入口会改写成反引号）
		_, err = tx.ExecContext(ctx, "INSERT INTO `setting` (`key`, value, type) VALUES (?, ?, ?)", "tx_probe", `"1"`, "general")
		return err
	}); err != nil {
		t.Fatalf("事务写入失败: %v", err)
	}

	row, err := db.QueryOne(ctx, "SELECT COUNT(*) AS n FROM `setting` WHERE `key` = ?", "tx_probe")
	if err != nil || row == nil {
		t.Fatalf("事务结果读取失败: %v", err)
	}
	if row.Int("n") != 1 {
		t.Errorf("事务写入未生效")
	}
}
