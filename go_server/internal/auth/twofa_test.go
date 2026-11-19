package auth

import (
	"strings"
	"testing"
	"time"
)

func TestSymmetricRoundTrip(t *testing.T) {
	const secret = "test-secret"
	plain := "JBSWY3DPEHPK3PXP"

	enc, err := SymmetricEncrypt(secret, plain)
	if err != nil {
		t.Fatal(err)
	}
	// better-auth 存的是纯 hex；新版本带 $ba$1$ 前缀，两种都要能解
	for _, payload := range []string{enc, "$ba$1$" + enc} {
		got, err := SymmetricDecrypt(secret, payload)
		if err != nil {
			t.Fatalf("decrypt %q: %v", payload, err)
		}
		if got != plain {
			t.Fatalf("want %q, got %q", plain, got)
		}
	}

	if _, err := SymmetricDecrypt("wrong-secret", enc); err == nil {
		t.Fatal("expected error with wrong secret")
	}
}

func TestTOTPVerifyWindow(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix() / totpPeriod
	code := totpCode(secret, now)
	if !VerifyTOTP(secret, code) {
		t.Fatal("current code should verify")
	}
	if VerifyTOTP(secret, totpCode(secret, now+10)) {
		t.Fatal("far-future code must not verify")
	}
	if VerifyTOTP(secret, "000000") && code != "000000" {
		t.Fatal("random code must not verify")
	}
}

func TestTwoFAPendingToken(t *testing.T) {
	s := &Service{secret: []byte("k")}
	tok := s.SignTwoFAPending("user-1")

	uid, ok := s.ParseTwoFAPending(tok)
	if !ok || uid != "user-1" {
		t.Fatalf("want user-1, got %q ok=%v", uid, ok)
	}

	// 改一个字符就应该失效
	tampered := tok[:len(tok)-1] + "x"
	if _, ok := s.ParseTwoFAPending(tampered); ok {
		t.Fatal("tampered token must not parse")
	}
	if _, ok := (&Service{secret: []byte("other")}).ParseTwoFAPending(tok); ok {
		t.Fatal("token signed with another secret must not parse")
	}
}

func TestConsumeBackupCode(t *testing.T) {
	codes := []string{"aaa", "bbb", "ccc"}
	used, rest := consumeBackupCode(codes, "BBB")
	if !used || len(rest) != 2 || strings.Contains(strings.Join(rest, ","), "bbb") {
		t.Fatalf("unexpected result: used=%v rest=%v", used, rest)
	}
	if used, _ := consumeBackupCode(codes, "zzz"); used {
		t.Fatal("unknown code must not match")
	}
}
