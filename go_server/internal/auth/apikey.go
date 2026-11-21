package auth

// API Key 生成与校验，对齐官方两条路径：
//
//  1. better-auth（@better-auth/api-key 1.7.12）：
//     - key = 64 位 [a-zA-Z] 随机串，默认无前缀；
//     - 存储列 key = SHA-256 → base64url（无填充），start = key 前 6 字符；
//     - 校验按哈希查行，检查 enabled 与 expiresAt；
//  2. legacy（2.x 的 api_key 表）：
//     - 完整 key 形如 "uk<id>_<clear>"，clear 用 bcrypt 存在 api_key.key。
//
// 官方 v3 库列名是 referenceId，早期 go_server 自建库是 userId，
// 读写都做了两列兼容（启动时 backfillAPIKeyReferenceID 会把旧值迁过去）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

const (
	apiKeyLength      = 64
	apiKeyStartLength = 6
	legacyKeyPrefix   = "uk"
)

// ErrAPIKeyNotFound 校验失败时返回，调用方不给用户区分“不存在/已禁用/过期”。
var ErrAPIKeyNotFound = errors.New("API key 不存在或已失效")

// HashAPIKey 与 better-auth 的 defaultKeyHasher 一致：
// SHA-256 → base64url，无填充。
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomAPIKey 生成 64 位 [a-zA-Z] 随机串，与官方 generateRandomString(length, "a-z", "A-Z") 一致。
func randomAPIKey() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, apiKeyLength)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见，退化为时间熵（与 GenID 同策略）
		return store.Now() + GenID()
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// CreateAPIKey 创建一把新 key，返回（id, 明文 key）。
// 明文只在创建时给前端展示一次，库里只存哈希。
func (s *Service) CreateAPIKey(ctx context.Context, userID, name string, expiresAt *time.Time) (string, string, error) {
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		expiresAt = nil
	}

	// 官方 createApiKey 前会先清掉已过期的 key
	if err := s.deleteExpiredAPIKeys(ctx); err != nil {
		return "", "", err
	}

	id := GenID()
	clear := randomAPIKey()

	values := map[string]any{
		"id":          id,
		"configId":    "default",
		"name":        name,
		"start":       clear[:apiKeyStartLength],
		"prefix":      nil,
		"key":         HashAPIKey(clear),
		"referenceId": userID,
		"enabled":     1,
		"expiresAt":   nil,
		"createdAt":   store.Now(),
		"updatedAt":   store.Now(),
	}
	if expiresAt != nil {
		values["expiresAt"] = store.FormatTime(*expiresAt)
	}

	if _, err := s.db.Insert(ctx, "better_auth_apikey", values); err != nil {
		return "", "", err
	}
	return id, clear, nil
}

// ListAPIKeys 返回全部 key 的展示字段（不含哈希），对齐官方 listApiKeys 的 Omit<ApiKey,"key">。
func (s *Service) ListAPIKeys(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, start, prefix, enabled, expiresAt, createdAt, updatedAt
		FROM better_auth_apikey ORDER BY createdAt DESC`)
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"id":        r.Str("id"),
			"name":      r.Str("name"),
			"start":     r.Str("start"),
			"prefix":    nullableStr(r, "prefix"),
			"enabled":   r.Int("enabled") != 0,
			"expiresAt": nullableStr(r, "expiresAt"),
			"createdAt": nullableStr(r, "createdAt"),
			"updatedAt": nullableStr(r, "updatedAt"),
		})
	}
	return out, nil
}

// DeleteAPIKey 删除一把 better-auth key。
func (s *Service) DeleteAPIKey(ctx context.Context, id string) error {
	return s.db.DeleteWhere(ctx, "better_auth_apikey", "id = ?", id)
}

// VerifyAPIKey 校验明文 key，成功返回所属用户 ID。
// better-auth 与 legacy 两条路径都试，语义对齐官方 verifyAPIKey。
func (s *Service) VerifyAPIKey(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", ErrAPIKeyNotFound
	}

	// 路径 1：better-auth（哈希查表）。
	// SELECT * ：不同来源的库列名有差异（官方 referenceId / 早期 go 库 userId），
	// 行级读取按实际列名取，避免列清单写死。
	row, err := s.db.QueryOne(ctx,
		"SELECT * FROM better_auth_apikey WHERE [key] = ?", HashAPIKey(key))
	if err == nil && row != nil {
		if row.Int("enabled") == 0 {
			return "", ErrAPIKeyNotFound
		}
		if expired(row.Str("expiresAt")) {
			// 官方验证时会顺手删掉过期 key
			_ = s.deleteExpiredAPIKeys(ctx)
			return "", ErrAPIKeyNotFound
		}
		if ref := row.Str("referenceId"); ref != "" {
			return ref, nil
		}
		// 早期 go_server 自建库列名是 userId
		if uid := row.Str("userId"); uid != "" {
			return uid, nil
		}
		return "", ErrAPIKeyNotFound
	}
	if err != nil {
		logx.Debugf("auth", "查询 better_auth_apikey 失败: %v", err)
	}

	// 路径 2：legacy "uk<id>_<clear>"
	if !strings.HasPrefix(key, legacyKeyPrefix) {
		return "", ErrAPIKeyNotFound
	}
	sep := strings.IndexByte(key, '_')
	if sep <= len(legacyKeyPrefix) || sep+1 >= len(key) {
		return "", ErrAPIKeyNotFound
	}
	idPart := key[len(legacyKeyPrefix):sep]
	clearPart := key[sep+1:]

	lrow, err := s.db.QueryOne(ctx, "SELECT [key], active, expires FROM api_key WHERE id = ?", idPart)
	if err != nil || lrow == nil {
		return "", ErrAPIKeyNotFound
	}
	if lrow.Int("active") == 0 || expired(lrow.Str("expires")) {
		return "", ErrAPIKeyNotFound
	}
	ok, err := VerifyPassword(clearPart, lrow.Str("key"))
	if err != nil || !ok {
		return "", ErrAPIKeyNotFound
	}
	// legacy key 不归属 better_auth_user，给调用方一个非空标识
	// （REST 层只关心“验证通过”，socket 层不走这条路径）。
	return "legacy:" + idPart, nil
}

// deleteExpiredAPIKeys 对齐官方 deleteAllExpiredApiKeys。
func (s *Service) deleteExpiredAPIKeys(ctx context.Context) error {
	_, err := s.db.Exec(ctx,
		"DELETE FROM better_auth_apikey WHERE expiresAt IS NOT NULL AND expiresAt != '' AND expiresAt < ?",
		store.Now())
	return err
}

// expired 判断库里的时间字符串是否已过期（空串视为永不过期）。
func expired(v string) bool {
	if v == "" {
		return false
	}
	t, err := store.ParseTime(v)
	return err == nil && t.Before(time.Now())
}

// nullableStr 把行里的可空列转成前端友好的 null（空串→nil）。
func nullableStr(r store.Row, col string) any {
	if v := r.Str(col); v != "" {
		return v
	}
	return nil
}

// ---------------- 修改密码（better-auth change-password） ----------------

// ErrPasswordTooShort better-auth 默认最短 8 位。
var ErrPasswordTooShort = errors.New("Password must be at least 8 characters")

// ChangePassword 对应 POST /api/auth/change-password：
// 校验当前密码 → 存 scrypt 新哈希（better-auth 默认格式，老格式会在这里被顺带升级）。
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	if err := s.CheckPassword(ctx, userID, currentPassword); err != nil {
		return ErrInvalidCredentials
	}
	if len(newPassword) < 8 {
		return ErrPasswordTooShort
	}

	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		"UPDATE better_auth_account SET password = ?, updatedAt = ? WHERE userId = ? AND providerId = 'credential'",
		hash, store.Now(), userID)
	return err
}
