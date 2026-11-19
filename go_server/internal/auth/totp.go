package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP 实现（RFC 6238），供两步验证使用。
//
// 说明：官方用 better-auth 的 twoFactor 插件，密钥在库里是加密存储的。
// 为了让官方库里的用户能直接登录，这里先按明文 Base32 处理；
// 读取官方加密密钥需要复刻 better-auth 的加密方案（P3 补齐）。

const totpPeriod = 30

// GenerateTOTPSecret 生成新的 Base32 密钥。
func GenerateTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// TOTPURI 生成 otpauth 链接，供前端渲染二维码。
func TOTPURI(secret, username, issuer string) string {
	if issuer == "" {
		issuer = "Uptime Kuma"
	}
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=%d",
		issuer, username, secret, issuer, totpPeriod)
}

// VerifyTOTP 校验 6 位验证码，允许前后各一个时间窗口的时钟漂移。
func VerifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}

	counter := time.Now().Unix() / totpPeriod
	for _, offset := range []int64{-1, 0, 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, counter+offset)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// totpCode 计算指定计数器的 6 位验证码。
func totpCode(secret string, counter int64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return ""
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	return fmt.Sprintf("%06d", value%1_000_000)
}
