package checkers

import (
	"strings"
	"testing"
)

// TestMySQLDSNTranslatesSSL 覆盖 mysql2 连接串里的 ssl 参数翻译。
//
// 2026-11-21 线上：TiDB Cloud 的连接串是
// `...?ssl={"rejectUnauthorized":true}`，我们把 ssl 原样透传，驱动不认，
// 于是明文连库，被服务端以 "insecure transport prohibited" 拒绝。
func TestMySQLDSNTranslatesSSL(t *testing.T) {
	cases := []struct {
		name     string
		conn     string
		wantTLS  string // 期望出现在 DSN 里的 tls 值，空表示不应有 tls
		noSSLKey bool
	}{
		{
			// mysql2 的 verifyIdentity 默认关闭：rejectUnauthorized=true 只验证书链，
			// 不比对主机名。这里必须落到"链校验"profile，不能用 tls=true，
			// 否则自定义域名（TiDB 的 tidb.songbo.fun）会因为证书 CN 不匹配被拒。
			name:     "rejectUnauthorized=true 走链校验（不比对主机名）",
			conn:     `mysql://u:p@host:4000/db?ssl={"rejectUnauthorized":true}`,
			wantTLS:  mysqlTLSChainOnly,
			noSSLKey: true,
		},
		{
			name:     "rejectUnauthorized=false 跳过校验",
			conn:     `mysql://u:p@host:4000/db?ssl={"rejectUnauthorized":false}`,
			wantTLS:  "skip-verify",
			noSSLKey: true,
		},
		{
			name:     "ssl=true 等价于链校验",
			conn:     "mysql://u:p@host:3306/db?ssl=true",
			wantTLS:  mysqlTLSChainOnly,
			noSSLKey: true,
		},
		{
			name:     "ssl=false 不启用 TLS",
			conn:     "mysql://u:p@host:3306/db?ssl=false",
			wantTLS:  "",
			noSSLKey: true,
		},
		{
			name:     "没有 ssl 参数时保持原样",
			conn:     "mysql://u:p@host:3306/db",
			wantTLS:  "",
			noSSLKey: true,
		},
		{
			name:     "其他参数继续透传",
			conn:     `mysql://u:p@host:3306/db?charset=utf8mb4&ssl={"rejectUnauthorized":true}`,
			wantTLS:  mysqlTLSChainOnly,
			noSSLKey: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dsn, err := mysqlDSN(c.conn, "")
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if c.noSSLKey && strings.Contains(dsn, "ssl=") {
				t.Fatalf("DSN 里不应残留 ssl 参数（驱动不认识）: %s", dsn)
			}
			if c.wantTLS == "" {
				if strings.Contains(dsn, "tls=") {
					t.Fatalf("不应带 tls: %s", dsn)
				}
				return
			}
			if !strings.Contains(dsn, "tls="+c.wantTLS) {
				t.Fatalf("期望 tls=%s，实际: %s", c.wantTLS, dsn)
			}
		})
	}
}

// TestMySQLDSNSSLInvalid 非法 ssl 值要报错，而不是静默退化成明文。
func TestMySQLDSNSSLInvalid(t *testing.T) {
	if _, err := mysqlDSN(`mysql://u:p@host:3306/db?ssl={oops}`, ""); err == nil {
		t.Fatal("非法 ssl 应报错")
	}
}

// TestMySQLTLSMode 直接覆盖翻译函数的分支。
func TestMySQLTLSMode(t *testing.T) {
	cases := map[string]string{
		`{"rejectUnauthorized":true}`:           mysqlTLSChainOnly,
		`{"rejectUnauthorized":false}`:          "skip-verify",
		`{"rejectUnauthorized":false,"ca":"x"}`: "skip-verify",
		`{}`:                                    mysqlTLSChainOnly,
		`true`:                                  mysqlTLSChainOnly,
		`false`:                                 "",
	}
	for in, want := range cases {
		got, err := mysqlTLSMode([]string{in})
		if err != nil {
			t.Fatalf("%s 解析失败: %v", in, err)
		}
		if got != want {
			t.Errorf("%s → %q，期望 %q", in, got, want)
		}
	}
}
