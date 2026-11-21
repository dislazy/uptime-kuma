package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// newAuthService 建一个带完整 schema 的临时库 + 服务。
func newAuthService(t *testing.T) *Service {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "kuma.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("初始化 schema 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(db, []byte("test-secret"))
}

// TestHashAPIKeyFormat 验证哈希格式与 better-auth defaultKeyHasher 一致：
// SHA-256 → base64url（无填充）。
func TestHashAPIKeyFormat(t *testing.T) {
	key := "AbCdEfGh12345678"
	want := base64.RawURLEncoding.EncodeToString(sha256Sum(key))
	if got := HashAPIKey(key); got != want {
		t.Fatalf("哈希格式不一致:\nwant %s\ngot  %s", want, got)
	}
	if HashAPIKey("") == "" {
		t.Fatal("空 key 也应有哈希")
	}
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// TestAPIKeyLifecycle 覆盖创建 → 列表 → 校验 → 删除。
func TestAPIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	svc := newAuthService(t)

	if _, err := svc.CreateUser(ctx, "admin", "password123"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	user, _, err := svc.Login(ctx, "admin", "password123", "1.2.3.4", "test", false)
	if err != nil || user == nil {
		t.Fatalf("登录失败: %v", err)
	}

	// 带过期时间创建
	expires := time.Now().Add(24 * time.Hour)
	id, clear, err := svc.CreateAPIKey(ctx, user.ID, "ci-key", &expires)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if len(clear) != apiKeyLength {
		t.Fatalf("key 长度 = %d，应为 %d", len(clear), apiKeyLength)
	}

	// 明文能校验，且返回所属用户
	uid, err := svc.VerifyAPIKey(ctx, clear)
	if err != nil || uid != user.ID {
		t.Fatalf("校验失败: uid=%q err=%v", uid, err)
	}

	// 列表不含哈希，含展示字段
	list, err := svc.ListAPIKeys(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表失败: n=%d err=%v", len(list), err)
	}
	if list[0]["id"] != id || list[0]["name"] != "ci-key" {
		t.Fatalf("列表字段不对: %v", list[0])
	}
	if _, has := list[0]["key"]; has {
		t.Fatal("列表绝不能带哈希列")
	}

	// 错 key 不通过
	if _, err := svc.VerifyAPIKey(ctx, clear+"x"); err == nil {
		t.Fatal("篡改过的 key 不应通过")
	}

	// 删除后失效
	if err := svc.DeleteAPIKey(ctx, id); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := svc.VerifyAPIKey(ctx, clear); err == nil {
		t.Fatal("删除后的 key 不应通过")
	}
}

// TestAPIKeyExpiry 过期 key 校验失败。
func TestAPIKeyExpiry(t *testing.T) {
	ctx := context.Background()
	svc := newAuthService(t)

	if _, err := svc.CreateUser(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	user, _, _ := svc.Login(ctx, "admin", "password123", "ip", "ua", false)

	past := time.Now().Add(-time.Hour)
	if _, clear, err := svc.CreateAPIKey(ctx, user.ID, "expired", &past); err != nil {
		t.Fatal(err)
	} else {
		// 官方语义：已过期的 expires 按“永不过期”处理
		if uid, err := svc.VerifyAPIKey(ctx, clear); err != nil || uid != user.ID {
			t.Fatalf("已过去的时间应视为不过期: %v", err)
		}
	}

	// 直接写一条已过期的行，校验必须失败
	future := time.Now().Add(time.Hour)
	id, clear, err := svc.CreateAPIKey(ctx, user.ID, "will-expire", &future)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = svc.db.Exec(ctx,
		"UPDATE better_auth_apikey SET expiresAt = ? WHERE id = ?",
		store.FormatTime(time.Now().Add(-time.Minute)), id)
	if _, err := svc.VerifyAPIKey(ctx, clear); err == nil {
		t.Fatal("过期 key 不应通过")
	}
}

// TestLegacyAPIKey 覆盖 2.x 的 "uk<id>_<clear>" 路径。
func TestLegacyAPIKey(t *testing.T) {
	ctx := context.Background()
	svc := newAuthService(t)

	hash, err := bcrypt.GenerateFromPassword([]byte("legacyClear"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	// api_key.user_id 有外键，先造一行 user
	if _, err := svc.db.Exec(ctx,
		`INSERT INTO [user] (id, username, password) VALUES (1, 'legacy-user', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Insert(ctx, "api_key", map[string]any{
		"key":     string(hash),
		"name":    "legacy",
		"user_id": 1,
		"active":  1,
	}); err != nil {
		t.Fatal(err)
	}

	if uid, err := svc.VerifyAPIKey(ctx, "uk1_legacyClear"); err != nil || uid == "" {
		t.Fatalf("legacy key 应通过: uid=%q err=%v", uid, err)
	}
	if _, err := svc.VerifyAPIKey(ctx, "uk1_wrong"); err == nil {
		t.Fatal("错误 clear 不应通过")
	}

	// 禁用后不通过
	_, _ = svc.db.Exec(ctx, "UPDATE api_key SET active = 0")
	if _, err := svc.VerifyAPIKey(ctx, "uk1_legacyClear"); err == nil {
		t.Fatal("禁用的 key 不应通过")
	}
}

// TestChangePassword 覆盖改密的主路径与错误分支。
func TestChangePassword(t *testing.T) {
	ctx := context.Background()
	svc := newAuthService(t)

	if _, err := svc.CreateUser(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	user, _, _ := svc.Login(ctx, "admin", "password123", "ip", "ua", false)

	if err := svc.ChangePassword(ctx, user.ID, "wrongpass", "newpassword8"); err != ErrInvalidCredentials {
		t.Fatalf("错误旧密码应报 ErrInvalidCredentials，got %v", err)
	}
	if err := svc.ChangePassword(ctx, user.ID, "password123", "short"); err != ErrPasswordTooShort {
		t.Fatalf("过短新密码应报 ErrPasswordTooShort，got %v", err)
	}
	if err := svc.ChangePassword(ctx, user.ID, "password123", "newpassword8"); err != nil {
		t.Fatalf("改密失败: %v", err)
	}

	// 新密码能登录，旧密码不能
	if _, _, err := svc.Login(ctx, "admin", "newpassword8", "ip", "ua", false); err != nil {
		t.Fatalf("新密码登录失败: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "password123", "ip", "ua", false); err == nil {
		t.Fatal("旧密码不应再能登录")
	}
}

// TestCheckUsernamePassword 对齐官方 better-auth.ts checkPassword：
// 按用户名查库 + credential 哈希校验，用于 /metrics Basic Auth 分支。
func TestCheckUsernamePassword(t *testing.T) {
	ctx := context.Background()
	svc := newAuthService(t)

	if _, err := svc.CreateUser(ctx, "admin", "password123"); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	if !svc.CheckUsernamePassword(ctx, "admin", "password123") {
		t.Fatal("正确用户名密码应通过")
	}
	if svc.CheckUsernamePassword(ctx, "admin", "wrong") {
		t.Fatal("错误密码应拒绝")
	}
	if svc.CheckUsernamePassword(ctx, "nobody", "password123") {
		t.Fatal("不存在的用户应拒绝")
	}
	if svc.CheckUsernamePassword(ctx, " admin ", "password123") {
		t.Fatal("官方 checkPassword 不做 trim，带空格的用户名应按原样匹配失败")
	}
}
