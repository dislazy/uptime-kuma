package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// 会话 Cookie 名。better-auth 默认值，前端与 socket 握手都依赖它。
const (
	CookieName         = "better-auth.session_token"
	SecureCookieName   = "__Secure-better-auth.session_token"
	defaultSessionTTL  = 7 * 24 * time.Hour
	rememberSessionTTL = 30 * 24 * time.Hour
)

// ErrInvalidCredentials 登录失败。
var ErrInvalidCredentials = errors.New("用户名或密码不正确")

// ErrTwoFactorRequired 需要二次验证。
var ErrTwoFactorRequired = errors.New("需要两步验证")

// User 是登录后的用户视图。
type User struct {
	ID               string
	Username         string
	Name             string
	TwoFactorEnabled bool
}

// Service 提供账号与会话能力。
type Service struct {
	db     *store.DB
	secret []byte
}

// NewService 构造。secret 用于签名 Cookie，必须持久化（存在设置表里）。
func NewService(db *store.DB, secret []byte) *Service {
	return &Service{db: db, secret: secret}
}

// HasUser 判断系统是否已初始化。
// 官方逻辑：better_auth_user 有记录，或旧 user 表有记录，都算已初始化。
func (s *Service) HasUser(ctx context.Context) (bool, error) {
	n, err := s.db.Count(ctx, "better_auth_user", "")
	if err != nil {
		// 表可能不存在（非官方库），退化为只看旧表
		logx.Debugf("auth", "查询 better_auth_user 失败: %v", err)
	} else if n > 0 {
		return true, nil
	}

	m, err := s.db.Count(ctx, "user", "")
	if err != nil {
		return false, nil
	}
	return m > 0, nil
}

// CreateUser 创建管理员账号（首次初始化用）。
func (s *Service) CreateUser(ctx context.Context, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("用户名不能为空")
	}
	if len(password) < 6 {
		return nil, errors.New("密码至少 6 位")
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}

	id := GenID()
	now := store.FormatTime(time.Now())

	_, err = s.db.Insert(ctx, "better_auth_user", map[string]any{
		"id":               id,
		"name":             username,
		"email":            username + "@" + id[:8] + ".local",
		"emailVerified":    1,
		"createdAt":        now,
		"updatedAt":        now,
		"username":         username,
		"displayUsername":  username,
		"role":             "admin",
		"twoFactorEnabled": 0,
	})
	if err != nil {
		return nil, err
	}

	_, err = s.db.Insert(ctx, "better_auth_account", map[string]any{
		"id":         GenID(),
		"userId":     id,
		"accountId":  id,
		"providerId": "credential",
		"password":   hash,
		"createdAt":  now,
		"updatedAt":  now,
	})
	if err != nil {
		return nil, err
	}

	return &User{ID: id, Username: username, Name: username}, nil
}

// Login 校验用户名密码，成功则创建会话并返回签名后的 Cookie 值。
func (s *Service) Login(ctx context.Context, username, password, ip, ua string, remember bool) (*User, string, error) {
	username = strings.TrimSpace(username)

	row, err := s.db.QueryOne(ctx,
		"SELECT id, username, name, twoFactorEnabled FROM better_auth_user WHERE username = ?", username)
	if err != nil {
		return nil, "", err
	}

	// 官方库可能只有旧 user 表，首次登录时迁移
	if row == nil {
		user, err := s.migrateLegacyUser(ctx, username, password)
		if err != nil {
			return nil, "", err
		}
		return s.issueSession(ctx, user, ip, ua, remember)
	}

	user := &User{
		ID:               row.Str("id"),
		Username:         row.Str("username"),
		Name:             row.Str("name"),
		TwoFactorEnabled: row.Bool("twoFactorEnabled"),
	}

	hash, err := s.passwordHash(ctx, user.ID)
	if err != nil {
		return nil, "", err
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return nil, "", ErrInvalidCredentials
	}

	// 老格式登录成功后顺手升级哈希
	if NeedsRehash(hash) {
		if newHash, err := HashPassword(password); err == nil {
			_, _ = s.db.Exec(ctx,
				"UPDATE better_auth_account SET password = ?, updatedAt = ? WHERE userId = ? AND providerId = 'credential'",
				newHash, store.FormatTime(time.Now()), user.ID)
		}
	}

	if user.TwoFactorEnabled {
		return user, "", ErrTwoFactorRequired
	}

	return s.issueSession(ctx, user, ip, ua, remember)
}

// issueSession 创建会话记录并返回签名的 cookie 值。
func (s *Service) issueSession(ctx context.Context, user *User, ip, ua string, remember bool) (*User, string, error) {
	ttl := defaultSessionTTL
	if remember {
		ttl = rememberSessionTTL
	}

	token := GenID() + GenID()
	now := time.Now()

	_, err := s.db.Insert(ctx, "better_auth_session", map[string]any{
		"id":        GenID(),
		"userId":    user.ID,
		"token":     token,
		"expiresAt": store.FormatTime(now.Add(ttl)),
		"ipAddress": ip,
		"userAgent": truncate(ua, 255),
		"createdAt": store.FormatTime(now),
		"updatedAt": store.FormatTime(now),
	})
	if err != nil {
		return nil, "", err
	}

	return user, s.SignCookie(token), nil
}

// SignCookie 生成 "token.签名" 形式的 Cookie 值。
func (s *Service) SignCookie(token string) string {
	return token + "." + s.sign(token)
}

// ParseCookieValue 校验并取出原始 token。
func (s *Service) ParseCookieValue(value string) (string, bool) {
	idx := strings.LastIndexByte(value, '.')
	if idx <= 0 {
		return "", false
	}
	token, sig := value[:idx], value[idx+1:]

	want := s.sign(token)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", false
	}
	return token, true
}

func (s *Service) sign(token string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// UserByToken 用原始 token 查会话。
func (s *Service) UserByToken(ctx context.Context, token string) (*User, error) {
	row, err := s.db.QueryOne(ctx, `
		SELECT s.expiresAt AS expiresAt, u.id AS id, u.username AS username, u.name AS name,
		       u.twoFactorEnabled AS twoFactorEnabled
		FROM better_auth_session s JOIN better_auth_user u ON u.id = s.userId
		WHERE s.token = ?`, token)
	if err != nil || row == nil {
		return nil, err
	}

	exp, err := store.ParseTime(row.Str("expiresAt"))
	if err == nil && time.Now().After(exp) {
		_, _ = s.db.Exec(ctx, "DELETE FROM better_auth_session WHERE token = ?", token)
		return nil, nil
	}

	return &User{
		ID:               row.Str("id"),
		Username:         row.Str("username"),
		Name:             row.Str("name"),
		TwoFactorEnabled: row.Bool("twoFactorEnabled"),
	}, nil
}

// UserFromRequest 从 Cookie 头解析当前用户。
func (s *Service) UserFromRequest(ctx context.Context, r *http.Request) (*User, error) {
	if r == nil {
		return nil, nil
	}
	return s.UserFromHeader(ctx, r.Header.Get("Cookie"))
}

// UserFromHeader 用原始 Cookie 头解析当前用户（socket 握手复用）。
func (s *Service) UserFromHeader(ctx context.Context, cookieHeader string) (*User, error) {
	raw := cookieFromHeader(cookieHeader)
	if raw == "" {
		return nil, nil
	}
	token, ok := s.ParseCookieValue(raw)
	if !ok {
		return nil, nil
	}
	return s.UserByToken(ctx, token)
}

// Logout 删除会话。
func (s *Service) Logout(ctx context.Context, rawCookie string) error {
	token, ok := s.ParseCookieValue(rawCookie)
	if !ok {
		return nil
	}
	_, err := s.db.Exec(ctx, "DELETE FROM better_auth_session WHERE token = ?", token)
	return err
}

// SetCookie 下发会话 Cookie。
func (s *Service) SetCookie(w http.ResponseWriter, value string, remember bool, secure bool) {
	c := &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
	if remember {
		c.MaxAge = int(rememberSessionTTL.Seconds())
	}
	if secure {
		c.Name = SecureCookieName
	}
	http.SetCookie(w, c)
}

// ClearCookie 清除会话 Cookie。
func (s *Service) ClearCookie(w http.ResponseWriter, secure bool) {
	c := &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Secure:   secure,
	}
	if secure {
		c.Name = SecureCookieName
	}
	http.SetCookie(w, c)
}

// passwordHash 取账号的密码哈希。
func (s *Service) passwordHash(ctx context.Context, userID string) (string, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT password FROM better_auth_account WHERE userId = ? AND providerId = 'credential'", userID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrInvalidHash
	}
	return row.Str("password"), nil
}

// CheckUsernamePassword 对齐官方 better-auth.ts checkPassword(username, password)：
// 按用户名查 better_auth_user → credential 哈希校验。
// 供 /metrics 等处 Basic Auth 使用（官方 userAuthorizer 的底层）。
func (s *Service) CheckUsernamePassword(ctx context.Context, username, password string) bool {
	row, err := s.db.QueryOne(ctx,
		"SELECT id FROM better_auth_user WHERE username = ?", username)
	if err != nil || row == nil {
		return false
	}
	hash, err := s.passwordHash(ctx, row.Str("id"))
	if err != nil || hash == "" {
		return false
	}
	ok, err := VerifyPassword(password, hash)
	return err == nil && ok
}

// migrateLegacyUser 把旧 user 表里的账号迁移到 better-auth 表。
func (s *Service) migrateLegacyUser(ctx context.Context, username, password string) (*User, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT id, username, password, twofa_status FROM [user] WHERE username = ?", username)
	if err != nil || row == nil {
		return nil, ErrInvalidCredentials
	}

	ok, verifyErr := VerifyPassword(password, row.Str("password"))
	if verifyErr != nil || !ok {
		return nil, ErrInvalidCredentials
	}

	user, err := s.CreateUser(ctx, username, password)
	if err != nil {
		return nil, err
	}

	logx.Infof("auth", "已将旧版账号 %s 迁移到 better-auth 表", username)
	return user, nil
}

// cookieFromHeader 从 Cookie 头里取出会话 Cookie 的原始值。
func cookieFromHeader(header string) string {
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if name == CookieName || name == SecureCookieName {
			return value
		}
	}
	return ""
}

// CookieFromRequest 供 socket 握手复用。
func CookieFromRequest(r *http.Request) string {
	return cookieFromHeader(r.Header.Get("Cookie"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// UserByID 按 id 读取用户视图（2FA 登录成功后补发会话时用）。
func (s *Service) UserByID(ctx context.Context, id string) (*User, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT id, username, name, twoFactorEnabled FROM better_auth_user WHERE id = ?", id)
	if err != nil || row == nil {
		return nil, err
	}
	return &User{
		ID:               row.Str("id"),
		Username:         row.Str("username"),
		Name:             row.Str("name"),
		TwoFactorEnabled: row.Bool("twoFactorEnabled"),
	}, nil
}
