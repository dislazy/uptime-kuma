package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestAPIKeyMigrationFromOldGoDB 模拟早期 go_server 自建库：
// better_auth_apikey 用旧列名 userId 且已有一行数据，跑一遍接管迁移后：
//   - referenceId 列被补上（ensureColumns）
//   - 旧值回填到 referenceId（backfillAPIKeyReferenceID）
//   - 明细读取按新列名工作
func TestAPIKeyMigrationFromOldGoDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kuma.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// 模拟旧库：删掉新表，按旧定义重建并塞一行
	if _, err := db.Exec(ctx, "DROP TABLE better_auth_apikey"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		CREATE TABLE [better_auth_apikey] (
			id           TEXT PRIMARY KEY,
			name         TEXT,
			start        TEXT,
			prefix       TEXT,
			[key]        TEXT NOT NULL,
			userId       TEXT NOT NULL,
			enabled      INTEGER DEFAULT 1,
			expiresAt    TIMESTAMP,
			createdAt    TIMESTAMP NOT NULL,
			updatedAt    TIMESTAMP NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO better_auth_apikey (id, name, [key], userId, enabled, createdAt, updatedAt)
		VALUES ('k1', 'old-key', 'hash-of-old', 'user-9', 1, '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`); err != nil {
		t.Fatal(err)
	}
	// userId 必须是真实存在的 better_auth_user，否则回填会被外键拦下（真实老库亦如此）
	if _, err := db.Exec(ctx, `
		INSERT INTO better_auth_user (id, name, email, username, emailVerified, createdAt, updatedAt)
		VALUES ('user-9', 'admin', 'u@x', 'admin', 1, '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`); err != nil {
		t.Fatal(err)
	}

	// 重新打开（模拟老库被新版本接管：monitor 已存在 → 走接管分支）
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.Migrate(ctx); err != nil {
		t.Fatalf("接管迁移失败: %v", err)
	}

	row, err := db2.QueryOne(ctx, "SELECT * FROM better_auth_apikey WHERE id = 'k1'")
	if err != nil || row == nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := row.Str("referenceId"); got != "user-9" {
		t.Fatalf("referenceId 回填失败: got %q, want user-9", got)
	}
}

// TestEnsureTablesCreatesMissingTable 验证接管模式下缺的整表会被补建。
func TestEnsureTablesCreatesMissingTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kuma.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "DROP TABLE better_auth_apikey"); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}

	// 补表 + 补索引都在
	if !db.HasTable(ctx, "better_auth_apikey") {
		t.Fatal("better_auth_apikey 应被补建")
	}
	var idxName string
	err = db.SQL().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='better_auth_apikey_key'").Scan(&idxName)
	if err != nil || idxName == "" {
		t.Fatalf("索引未补建: %v", err)
	}
}
