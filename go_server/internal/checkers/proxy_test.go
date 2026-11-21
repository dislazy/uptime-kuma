package checkers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// TestProxySetGetRoundtrip 校验 Raw 注入与读出对称。
func TestProxySetGetRoundtrip(t *testing.T) {
	m := &model.Monitor{Raw: map[string]any{}}

	if _, ok := ProxyFrom(m); ok {
		t.Fatal("未注入时应返回 ok=false")
	}

	SetProxy(m, ProxyInfo{
		Protocol: "socks5h", Host: "10.0.0.1", Port: 1080,
		Auth: true, Username: "u", Password: "p", Active: true,
	})

	info, ok := ProxyFrom(m)
	if !ok {
		t.Fatal("注入后应能读出")
	}
	if info.Protocol != "socks5h" || info.Host != "10.0.0.1" || info.Port != 1080 {
		t.Fatalf("读出内容不对: %+v", info)
	}
	if !info.Auth || info.Username != "u" || !info.Active {
		t.Fatalf("认证字段不对: %+v", info)
	}

	ClearProxy(m)
	if _, ok := ProxyFrom(m); ok {
		t.Fatal("ClearProxy 后应读不到")
	}
}

// TestApplyProxyTransport 校验各协议落到 Transport 的方式。
func TestApplyProxyTransport(t *testing.T) {
	cases := []struct {
		name     string
		info     ProxyInfo
		wantURL  string // http/https：期望的代理 URL
		wantDial bool   // socks 家族：期望自定义 DialContext
		wantErr  bool
	}{
		{
			name:    "http 代理带认证",
			info:    ProxyInfo{Protocol: "http", Host: "10.0.0.2", Port: 3128, Auth: true, Username: "u", Password: "p"},
			wantURL: "http://u:p@10.0.0.2:3128",
		},
		{
			name:    "https 代理",
			info:    ProxyInfo{Protocol: "https", Host: "proxy.example.com", Port: 8443},
			wantURL: "https://proxy.example.com:8443",
		},
		{
			name:     "socks5 本地解析",
			info:     ProxyInfo{Protocol: "socks5", Host: "10.0.0.3", Port: 1080},
			wantDial: true,
		},
		{
			name:     "socks5h 远程解析",
			info:     ProxyInfo{Protocol: "socks5h", Host: "10.0.0.3", Port: 1080},
			wantDial: true,
		},
		{
			name:     "socks 默认按 v5",
			info:     ProxyInfo{Protocol: "socks", Host: "10.0.0.3", Port: 1080},
			wantDial: true,
		},
		{
			name:     "socks4",
			info:     ProxyInfo{Protocol: "socks4", Host: "10.0.0.4", Port: 1080},
			wantDial: true,
		},
		{
			name:    "非法协议",
			info:    ProxyInfo{Protocol: "socks6", Host: "x", Port: 1},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &http.Transport{}
			err := ApplyProxy(tr, tc.info)

			if tc.wantErr {
				if err == nil {
					t.Fatal("应报错")
				}
				var pe *ProxyConfigError
				if !errors.As(err, &pe) {
					t.Fatalf("应返回 ProxyConfigError: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应用失败: %v", err)
			}

			if tc.wantURL != "" {
				req, _ := http.NewRequest("GET", "http://target.example/", nil)
				got, err := tr.Proxy(req)
				if err != nil {
					t.Fatalf("ProxyFunc 报错: %v", err)
				}
				if got.String() != tc.wantURL {
					t.Fatalf("代理 URL = %q, 期望 %q", got.String(), tc.wantURL)
				}
			}
			if tc.wantDial {
				if tr.Proxy != nil {
					t.Fatal("socks 家族不应走 Proxy 路径")
				}
				if tr.DialContext == nil {
					t.Fatal("socks 家族应设置 DialContext")
				}
			}
		})
	}
}

// TestProxyThroughHTTPChecker 校验 http checker 能读到注入的代理。
func TestProxyThroughHTTPChecker(t *testing.T) {
	m := &model.Monitor{Raw: map[string]any{}}
	m.Raw["url"] = "https://example.com"
	SetProxy(m, ProxyInfo{Protocol: "grpc", Host: "10.0.0.1", Port: 1, Active: true})

	c := HTTPChecker{}
	res := c.Check(context.Background(), m)
	if res.Status != 0 {
		t.Fatalf("非法代理协议应判定 DOWN: %+v", res)
	}
	if !strings.Contains(res.Msg, "Unsupported proxy protocol") {
		t.Fatalf("错误文案应透出官方 createAgents 语义: %q", res.Msg)
	}
}
