package checkers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"golang.org/x/net/proxy"
)

// Proxy 管理：监控探测请求走指定代理（官方 server/proxy.js + monitor.js:654）。
//
// 官方协议语义（socks-proxy-agent 的 lookup 规则）：
//   - http / https ：CONNECT 隧道代理
//   - socks5      ：本地解析 DNS，再把 IP 交给代理
//   - socks5h/socks：域名直接交给代理解析（bare "socks" 官方按 SOCKSv5 处理）
//   - socks4      ：本地解析 DNS 后按 SOCKSv4 协议连接（仅 IPv4）

// Raw 注入键，manager 在每次探测前把 proxy 表内容解析进 monitor.Raw。
// 下划线前缀保证不会下发前端（monitorJSON 过滤 _ 开头键）。
const (
	RawProxyProtocol = "_proxy_protocol"
	RawProxyHost     = "_proxy_host"
	RawProxyPort     = "_proxy_port"
	RawProxyAuth     = "_proxy_auth"
	RawProxyUser     = "_proxy_user"
	RawProxyPass     = "_proxy_pass"
	RawProxyActive   = "_proxy_active"
)

// ProxyInfo 是 proxy 表一行的探测侧投影。
type ProxyInfo struct {
	Protocol string
	Host     string
	Port     int64
	Auth     bool
	Username string
	Password string
	Active   bool
}

// SetProxy 把代理解析结果写进 monitor.Raw（仿 SetDockerHost 的注入模式）。
func SetProxy(m *model.Monitor, info ProxyInfo) {
	if m == nil || m.Raw == nil {
		return
	}
	m.Raw[RawProxyProtocol] = info.Protocol
	m.Raw[RawProxyHost] = info.Host
	m.Raw[RawProxyPort] = info.Port
	m.Raw[RawProxyAuth] = info.Auth
	m.Raw[RawProxyUser] = info.Username
	m.Raw[RawProxyPass] = info.Password
	m.Raw[RawProxyActive] = info.Active
}

// ClearProxy 清掉注入的代理信息（proxy_id 被删/置空时调用，防止读到旧值）。
func ClearProxy(m *model.Monitor) {
	if m == nil || m.Raw == nil {
		return
	}
	for _, k := range []string{
		RawProxyProtocol, RawProxyHost, RawProxyPort, RawProxyAuth,
		RawProxyUser, RawProxyPass, RawProxyActive,
	} {
		delete(m.Raw, k)
	}
}

// ProxyFrom 从 monitor.Raw 取出代理信息；没有代理返回 ok=false。
func ProxyFrom(m *model.Monitor) (ProxyInfo, bool) {
	if m == nil {
		return ProxyInfo{}, false
	}
	proto := m.Get(RawProxyProtocol)
	if proto == "" {
		return ProxyInfo{}, false
	}
	return ProxyInfo{
		Protocol: proto,
		Host:     m.Get(RawProxyHost),
		Port:     m.GetInt(RawProxyPort),
		Auth:     m.GetBool(RawProxyAuth),
		Username: m.Get(RawProxyUser),
		Password: m.Get(RawProxyPass),
		Active:   m.GetBool(RawProxyActive),
	}, true
}

// ProxyConfigError 表示代理协议不可用，错误文案对齐官方 createAgents 的抛错。
type ProxyConfigError struct{ msg string }

func (e *ProxyConfigError) Error() string { return e.msg }

// ApplyProxy 把代理配置到 http.Transport 上。
//
// 官方 createAgents 为每个代理新建 httpAgent/httpsAgent 挂到 axios；
// Go 侧等价做法是配置单个 Transport：
//   - http/https → Transport.Proxy（CONNECT 隧道，https scheme 自动走 TLS-to-proxy）
//   - socks 家族 → Transport.DialContext（隧道内原样承载 http 与 https 流量）
func ApplyProxy(t *http.Transport, info ProxyInfo) error {
	if t == nil {
		return nil
	}

	hostport := net.JoinHostPort(info.Host, strconv.FormatInt(info.Port, 10))

	switch info.Protocol {
	case "http", "https":
		u := &url.URL{Scheme: info.Protocol, Host: hostport}
		if info.Auth {
			u.User = url.UserPassword(info.Username, info.Password)
		}
		t.Proxy = http.ProxyURL(u)

	case "socks5":
		d, err := socks5Dialer(info, hostport)
		if err != nil {
			return err
		}
		// 官方 socks5 是本地解析 DNS 后连 IP（socks-proxy-agent lookup=true）
		t.Proxy = nil
		t.DialContext = localResolve{inner: d}.DialContext

	case "socks", "socks5h":
		d, err := socks5Dialer(info, hostport)
		if err != nil {
			return err
		}
		t.Proxy = nil
		t.DialContext = d.(proxy.ContextDialer).DialContext

	case "socks4":
		t.Proxy = nil
		t.DialContext = (&socks4Dialer{proxyAddr: hostport, userID: proxyUser(info)}).DialContext

	default:
		return &ProxyConfigError{msg: fmt.Sprintf(
			"Unsupported proxy protocol provided. %s", info.Protocol)}
	}
	return nil
}

func proxyUser(info ProxyInfo) string {
	if info.Auth {
		return info.Username
	}
	return ""
}

func socks5Dialer(info ProxyInfo, hostport string) (proxy.Dialer, error) {
	var auth *proxy.Auth
	if info.Auth {
		auth = &proxy.Auth{User: info.Username, Password: info.Password}
	}
	forward := &net.Dialer{Timeout: 10 * time.Second}
	return proxy.SOCKS5("tcp", hostport, auth, forward)
}

// localResolve 包一层本地 DNS：socks5 协议要求先在本机解析域名再连 IP。
type localResolve struct{ inner proxy.Dialer }

func (d localResolve) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d localResolve) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	resolved, err := resolveLocal(ctx, addr)
	if err != nil {
		return nil, err
	}
	if cd, ok := d.inner.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, resolved)
	}
	return d.inner.Dial(network, resolved)
}

// resolveLocal 把 host:port 里的域名部分解析成本地 IP（IP 直通）。
func resolveLocal(ctx context.Context, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, nil // 形如 :80 的极端写法，交给底层处理
	}
	if net.ParseIP(host) != nil {
		return addr, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("socks5: 本地解析 %s 失败: %v", host, err)
	}
	return net.JoinHostPort(ips[0].IP.String(), port), nil
}

// socks4Dialer 是 SOCKSv4 客户端（官方经 socks-proxy-agent 支持 socks4，无现成 Go 库）。
// 协议：VN=4 CMD=1 + DSTPORT(2B) + DSTIP(4B) + USERID\0 → 8 字节应答，CD=0x5A 即成功。
type socks4Dialer struct {
	proxyAddr string
	userID    string
}

func (d *socks4Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("socks4: 无效目标 %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("socks4: 无效端口 %q", portStr)
	}

	// socks4 只支持 IPv4，域名先本地解析（官方 lookup=true 同语义）
	var ip net.IP
	if parsed := net.ParseIP(host); parsed != nil {
		ip = parsed.To4()
	} else {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("socks4: 本地解析 %s 失败: %v", host, err)
		}
		ip = ips[0].To4()
	}
	if ip == nil {
		return nil, fmt.Errorf("socks4: 目标 %s 不是 IPv4", host)
	}

	nd := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := nd.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks4: 连接代理 %s 失败: %v", d.proxyAddr, err)
	}

	req := []byte{0x04, 0x01, byte(port >> 8), byte(port), ip[0], ip[1], ip[2], ip[3]}
	if d.userID != "" {
		req = append(req, d.userID...)
	}
	req = append(req, 0x00)

	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks4: 发送请求失败: %v", err)
	}

	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks4: 读取应答失败: %v", err)
	}
	if resp[1] != 0x5A {
		_ = conn.Close()
		return nil, fmt.Errorf("socks4: 代理拒绝连接 (code=%d)", resp[1])
	}
	return conn, nil
}
