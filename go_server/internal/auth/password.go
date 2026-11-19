package auth

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/text/unicode/norm"
)

// 密码哈希的三种历史格式，必须全部能验证：
//
//	1. better-auth 默认：scrypt，存为 "<saltHex>:<keyHex>"
//	2. Uptime Kuma 2.x ：bcrypt，以 "$2a$" / "$2b$" / "$2y$" 开头
//	3. Uptime Kuma 1.x ：password-hash 库，以 "sha1$" 开头
//
// 老用户用老格式登录成功后，调用方应升级成新格式。

// scrypt 参数必须与 better-auth 保持一致，否则官方库里的用户无法登录。
const (
	scryptN      = 16384
	scryptR      = 16
	scryptP      = 1
	scryptKeyLen = 64
)

// ErrInvalidHash 表示哈希格式无法识别。
var ErrInvalidHash = errors.New("无法识别的密码哈希格式")

// HashPassword 生成 better-auth 兼容的 scrypt 哈希。
func HashPassword(password string) (string, error) {
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(saltBytes)

	key, err := scrypt.Key([]byte(normalizePassword(password)), []byte(salt), scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return salt + ":" + hex.EncodeToString(key), nil
}

// VerifyPassword 校验明文密码是否匹配存储的哈希。
func VerifyPassword(password, stored string) (bool, error) {
	switch {
	case stored == "":
		return false, ErrInvalidHash

	case strings.Contains(stored, ":") && !strings.HasPrefix(stored, "sha1$"):
		return verifyScrypt(password, stored)

	case strings.HasPrefix(stored, "$2"):
		return verifyBcrypt(password, stored)

	case strings.HasPrefix(stored, "sha1$"):
		return verifyLegacySHA1(password, stored)
	}
	return false, ErrInvalidHash
}

// NeedsRehash 判断是否需要把老格式升级成 scrypt。
func NeedsRehash(stored string) bool {
	return strings.HasPrefix(stored, "$2") || strings.HasPrefix(stored, "sha1$")
}

func verifyScrypt(password, stored string) (bool, error) {
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		return false, ErrInvalidHash
	}
	salt, wantHex := parts[0], parts[1]

	want, err := hex.DecodeString(wantHex)
	if err != nil {
		return false, ErrInvalidHash
	}

	got, err := scrypt.Key([]byte(normalizePassword(password)), []byte(salt), scryptN, scryptR, scryptP, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func verifyBcrypt(password, stored string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return false, err
}

// verifyLegacySHA1 兼容 password-hash 库的格式：
//
//	sha1$<iterations>$<salt>$<hash>
func verifyLegacySHA1(password, stored string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 {
		return false, ErrInvalidHash
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false, ErrInvalidHash
	}
	salt, want := parts[2], parts[3]

	var sum string
	for i := 0; i <= iterations; i++ {
		var h = sha1.New()
		if i == 0 {
			h.Write([]byte(salt + password))
		} else {
			h.Write([]byte(sum + salt + password))
		}
		sum = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}

	return subtle.ConstantTimeCompare([]byte(sum), []byte(want)) == 1, nil
}

// normalizePassword 对齐 better-auth 的 NFKC 归一化。
// 非 ASCII 密码（例如中文、全角字符）必须归一化，否则与官方算出的哈希不一致。
func normalizePassword(p string) string {
	return norm.NFKC.String(p)
}

// GenID 生成 better-auth 风格的字符串主键（32 位十六进制）。
func GenID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
