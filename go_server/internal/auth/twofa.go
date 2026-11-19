package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/store"
)

// 两步验证（better-auth twoFactor 插件兼容）。
//
// 库里的表是 better_auth_twofactor：
//
//	id / userId / secret(加密) / backupCodes(加密 JSON 数组) / verified
//	failedVerificationCount / lockedUntil
//
// secret 与 backupCodes 都是 better-auth 的对称加密格式（见 symmetric.go），
// 因此官方库里的数据可以直接读，不需要让用户重新绑定。
//
// 锁定规则对齐 better-auth 1.7：连续 5 次失败锁 5 分钟。

const (
	twoFAMaxFailures = 5
	twoFALockWindow  = 5 * time.Minute
	backupCodeCount  = 10
)

// TwoFAState 是读取出来的 2FA 记录。
type TwoFAState struct {
	ID          string
	UserID      string
	Secret      string // 已解密
	BackupCodes []string
	Verified    bool
	FailedCount int
	LockedUntil string
}

// TwoFAStatus 返回用户是否开启了两步验证。
func (s *Service) TwoFAStatus(ctx context.Context, userID string) (bool, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT twoFactorEnabled FROM better_auth_user WHERE id = ?", userID)
	if err != nil || row == nil {
		return false, err
	}
	return toBool(row.Str("twoFactorEnabled")), nil
}

// BeginTwoFA 生成新的 TOTP 密钥与备用码，写入（或覆盖）未验证的记录。
// 返回 otpauth URI 与明文备用码（只在此刻返回一次）。
func (s *Service) BeginTwoFA(ctx context.Context, userID, username, issuer string) (uri string, backupCodes []string, err error) {
	if issuer == "" {
		issuer = "Uptime Kuma"
	}

	secret, err := GenerateTOTPSecret()
	if err != nil {
		return "", nil, err
	}
	backupCodes = generateBackupCodes(backupCodeCount)

	encSecret, err := SymmetricEncrypt(string(s.secret), secret)
	if err != nil {
		return "", nil, err
	}
	rawCodes, _ := json.Marshal(backupCodes)
	encCodes, err := SymmetricEncrypt(string(s.secret), string(rawCodes))
	if err != nil {
		return "", nil, err
	}

	row, err := s.db.QueryOne(ctx,
		"SELECT id FROM better_auth_twofactor WHERE userId = ?", userID)
	if err != nil {
		return "", nil, err
	}

	if row == nil {
		_, err = s.db.Insert(ctx, "better_auth_twofactor", map[string]any{
			"id":                      GenID(),
			"userId":                  userID,
			"secret":                  encSecret,
			"backupCodes":             encCodes,
			"verified":                0,
			"failedVerificationCount": 0,
		})
	} else {
		_, err = s.db.Exec(ctx,
			`UPDATE better_auth_twofactor
			 SET secret = ?, backupCodes = ?, verified = 0, failedVerificationCount = 0, lockedUntil = NULL
			 WHERE userId = ?`, encSecret, encCodes, userID)
	}
	if err != nil {
		return "", nil, err
	}

	return TOTPURI(secret, username, issuer), backupCodes, nil
}

// ConfirmTwoFA 校验首次绑定时的验证码，成功后把记录标记为已验证并开启用户开关。
func (s *Service) ConfirmTwoFA(ctx context.Context, userID, code string) error {
	st, err := s.loadTwoFA(ctx, userID)
	if err != nil {
		return err
	}
	if st == nil {
		return errors.New("Two-factor authentication is not set up")
	}
	if st.LockedUntil != "" {
		if until, err := time.ParseInLocation("2006-01-02 15:04:05", st.LockedUntil, time.Local); err == nil && until.After(time.Now()) {
			return fmt.Errorf("Too many failed attempts. Try again after %s", until.Format("15:04:05"))
		}
	}

	if !VerifyTOTP(st.Secret, code) {
		s.recordTwoFAFailure(ctx, userID, st)
		return errors.New("Invalid two-factor code")
	}

	if _, err := s.db.Exec(ctx,
		`UPDATE better_auth_twofactor SET verified = 1, failedVerificationCount = 0, lockedUntil = NULL
		 WHERE userId = ?`, userID); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		"UPDATE better_auth_user SET twoFactorEnabled = 1 WHERE id = ?", userID)
	return err
}

// VerifyTwoFA 登录阶段的校验：TOTP 或备用码都可以。
func (s *Service) VerifyTwoFA(ctx context.Context, userID, code string) error {
	st, err := s.loadTwoFA(ctx, userID)
	if err != nil {
		return err
	}
	if st == nil || !st.Verified {
		return errors.New("Two-factor authentication is not enabled")
	}
	if st.LockedUntil != "" {
		if until, err := time.ParseInLocation("2006-01-02 15:04:05", st.LockedUntil, time.Local); err == nil && until.After(time.Now()) {
			return fmt.Errorf("Too many failed attempts. Try again after %s", until.Format("15:04:05"))
		}
	}

	if VerifyTOTP(st.Secret, code) {
		_, _ = s.db.Exec(ctx,
			"UPDATE better_auth_twofactor SET failedVerificationCount = 0, lockedUntil = NULL WHERE userId = ?", userID)
		return nil
	}

	// 备用码一次性使用：命中后从列表里删掉
	if used, remaining := consumeBackupCode(st.BackupCodes, code); used {
		s.saveBackupCodes(ctx, userID, remaining)
		_, _ = s.db.Exec(ctx,
			"UPDATE better_auth_twofactor SET failedVerificationCount = 0, lockedUntil = NULL WHERE userId = ?", userID)
		return nil
	}

	s.recordTwoFAFailure(ctx, userID, st)
	return errors.New("Invalid two-factor code")
}

// RegenerateBackupCodes 重新生成备用码（要求已开启 2FA）。
func (s *Service) RegenerateBackupCodes(ctx context.Context, userID string) ([]string, error) {
	st, err := s.loadTwoFA(ctx, userID)
	if err != nil {
		return nil, err
	}
	if st == nil || !st.Verified {
		return nil, errors.New("Two-factor authentication is not enabled")
	}

	codes := generateBackupCodes(backupCodeCount)
	s.saveBackupCodes(ctx, userID, codes)
	return codes, nil
}

// DisableTwoFA 关闭两步验证并删除记录。
func (s *Service) DisableTwoFA(ctx context.Context, userID string) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM better_auth_twofactor WHERE userId = ?", userID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, "UPDATE better_auth_user SET twoFactorEnabled = 0 WHERE id = ?", userID)
	return err
}

// CheckPassword 校验某个用户的密码（关闭 2FA 前需要）。
func (s *Service) CheckPassword(ctx context.Context, userID, password string) error {
	hash, err := s.passwordHash(ctx, userID)
	if err != nil {
		return err
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	return nil
}

// ---------------- 内部 ----------------

func (s *Service) loadTwoFA(ctx context.Context, userID string) (*TwoFAState, error) {
	row, err := s.db.QueryOne(ctx,
		`SELECT id, userId, secret, backupCodes, verified, failedVerificationCount, lockedUntil
		 FROM better_auth_twofactor WHERE userId = ?`, userID)
	if err != nil || row == nil {
		return nil, err
	}

	st := &TwoFAState{
		ID:          row.Str("id"),
		UserID:      row.Str("userId"),
		Verified:    toBool(row.Str("verified")),
		FailedCount: int(row.Int("failedVerificationCount")),
		LockedUntil: row.Str("lockedUntil"),
	}

	if raw := row.Str("secret"); raw != "" {
		if plain, err := SymmetricDecrypt(string(s.secret), raw); err == nil {
			st.Secret = plain
		}
	}
	if raw := row.Str("backupCodes"); raw != "" {
		if plain, err := SymmetricDecrypt(string(s.secret), raw); err == nil {
			_ = json.Unmarshal([]byte(plain), &st.BackupCodes)
		}
	}
	return st, nil
}

func (s *Service) recordTwoFAFailure(ctx context.Context, userID string, st *TwoFAState) {
	count := st.FailedCount + 1
	if count >= twoFAMaxFailures {
		until := store.FormatTime(time.Now().Add(twoFALockWindow))
		_, _ = s.db.Exec(ctx,
			"UPDATE better_auth_twofactor SET failedVerificationCount = 0, lockedUntil = ? WHERE userId = ?",
			until, userID)
		return
	}
	_, _ = s.db.Exec(ctx,
		"UPDATE better_auth_twofactor SET failedVerificationCount = ? WHERE userId = ?", count, userID)
}

func (s *Service) saveBackupCodes(ctx context.Context, userID string, codes []string) {
	raw, _ := json.Marshal(codes)
	enc, err := SymmetricEncrypt(string(s.secret), string(raw))
	if err != nil {
		return
	}
	_, _ = s.db.Exec(ctx, "UPDATE better_auth_twofactor SET backupCodes = ? WHERE userId = ?", enc, userID)
}

func consumeBackupCode(codes []string, input string) (bool, []string) {
	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return false, codes
	}
	for i, c := range codes {
		if strings.EqualFold(strings.TrimSpace(c), input) {
			out := make([]string, 0, len(codes)-1)
			out = append(out, codes[:i]...)
			out = append(out, codes[i+1:]...)
			return true, out
		}
	}
	return false, codes
}

func generateBackupCodes(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			continue
		}
		code := hex.EncodeToString(b) // 10 位十六进制
		out = append(out, code)
	}
	return out
}

func toBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true"
}

// ---------------- 二次验证的临时凭证 ----------------

// TwoFAPendingTTL 是"密码已过、还差验证码"状态的存活时间。
const TwoFAPendingTTL = 5 * time.Minute

// SignTwoFAPending 生成一个短期令牌，放在 HttpOnly Cookie 里，
// 表示"这个用户已经过了密码这一步"。
//
// better-auth 内部用的是它自己的签名 cookie；这里用 HMAC-SHA256 自签，
// 语义相同且不依赖会话表（用户此时还没有会话）。
func (s *Service) SignTwoFAPending(userID string) string {
	exp := time.Now().Add(TwoFAPendingTTL).Unix()
	payload := fmt.Sprintf("%s|%d", userID, exp)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("2fa:" + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// ParseTwoFAPending 校验并解出 userID。
func (s *Service) ParseTwoFAPending(token string) (string, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}

	payload := string(raw)
	userID, expStr, ok := strings.Cut(payload, "|")
	if !ok {
		return "", false
	}

	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("2fa:" + payload))
	expect := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expect), []byte(parts[1])) {
		return "", false
	}

	var exp int64
	if _, err := fmt.Sscan(expStr, &exp); err != nil {
		return "", false
	}
	if time.Now().Unix() > exp {
		return "", false
	}
	return userID, true
}

// IssueSession 是给 2FA 成功后补发会话用的公开入口。
func (s *Service) IssueSession(ctx context.Context, user *User, ip, ua string, remember bool) (*User, string, error) {
	return s.issueSession(ctx, user, ip, ua, remember)
}

// 保证 base32 被引用（GenerateTOTPSecret 在 totp.go 里用它）
var _ = base32.StdEncoding
