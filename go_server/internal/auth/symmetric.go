package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// better-auth 的对称加密兼容层。
//
// 官方（packages/better-auth/src/crypto/index.ts）的做法：
//
//	key      = SHA-256(BETTER_AUTH_SECRET)         ← 注意 hash 的是字符串本身
//	cipher   = XChaCha20-Poly1305（24 字节 nonce 前置）
//	密文     = hex( nonce || ciphertext )
//	新版本会再包一层信封：$ba$<version>$<hex>
//
// 库里存的 2FA secret 与 backupCodes 都是这个格式。不解开它，
// 老用户把 kuma.db 拷过来就登不进去——这是"数据零迁移"承诺的一部分。

const envelopePrefix = "$ba$"

// SymmetricEncrypt 加密，用于写入与官方兼容的 2FA 数据。
func SymmetricEncrypt(secret, plain string) (string, error) {
	key := sha256.Sum256([]byte(secret))
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	out := aead.Seal(nonce, nonce, []byte(plain), nil)
	return hex.EncodeToString(out), nil
}

// SymmetricDecrypt 解密 better-auth 写入的数据（含新版本信封格式）。
func SymmetricDecrypt(secret, data string) (string, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return "", errors.New("empty ciphertext")
	}

	// 剥掉 $ba$<version>$ 信封
	if strings.HasPrefix(data, envelopePrefix) {
		rest := data[len(envelopePrefix):]
		i := strings.IndexByte(rest, '$')
		if i < 0 {
			return "", errors.New("malformed encryption envelope")
		}
		data = rest[i+1:]
	}

	raw, err := hex.DecodeString(data)
	if err != nil {
		return "", err
	}

	key := sha256.Sum256([]byte(secret))
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("ciphertext too short")
	}

	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
