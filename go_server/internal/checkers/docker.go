package checkers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// DockerChecker 通过 Docker Engine API 查容器状态。
//
// 官方走 dockerode + axios：`GET /containers/<name>/json`，然后按
// State.Running / Paused / Restarting / Health.Status 判定。连接方式有两种：
//
//	socket → unix:///var/run/docker.sock（docker_daemon 存 socket 路径）
//	tcp    → tcp://host:2375 或 https://host:2376（docker_daemon 存 URL）
//
// 证书按官方约定放在 data/docker-tls/<hostname>/{ca.pem,cert.pem,key.pem}，
// 三个文件都在才启用 mTLS。
type DockerChecker struct{}

// Type 实现 Checker。
func (DockerChecker) Type() string { return "docker" }

func init() { Register(DockerChecker{}) }

// DockerHostInfo 是从 docker_host 表解析出来的连接信息。
// 由 store 在 hydrate 阶段写进 monitor.Raw，checker 不直接碰数据库。
type DockerHostInfo struct {
	Daemon string // socket 路径或 tcp://... URL
	Type   string // "socket" | "tcp"
}

// Raw 里的键名，store 在 hydrate 阶段用它把 docker_host 表的内容塞进监控。
const (
	RawDockerDaemon = "_docker_daemon"
	RawDockerType   = "_docker_type"
)

// SetDockerHost 把连接信息写进 monitor.Raw。
//
// checker 不直接访问数据库，所以由 store 在加载监控时预先解析好。
func SetDockerHost(m *model.Monitor, info DockerHostInfo) {
	if m == nil || m.Raw == nil {
		return
	}
	m.Raw[RawDockerDaemon] = info.Daemon
	m.Raw[RawDockerType] = info.Type
}

// dockerHostFrom 从 monitor.Raw 取出连接信息。
func dockerHostFrom(m *model.Monitor) DockerHostInfo {
	return DockerHostInfo{
		Daemon: m.Get(RawDockerDaemon),
		Type:   strings.ToLower(m.Get(RawDockerType)),
	}
}

// dockerTLSFileNames 与官方 DockerHost 的常量一致。
var dockerTLSFileNames = [3]string{"ca.pem", "cert.pem", "key.pem"}

// dockerTLSDir 返回证书目录，可用 DOCKER_TLS_DIR_PATH 覆盖（官方同款环境变量）。
func dockerTLSDir() string {
	if v := os.Getenv("DOCKER_TLS_DIR_PATH"); v != "" {
		return v
	}
	return filepath.Join("data", "docker-tls")
}

// patchDockerURL 把 tcp:// 换成 http://，与官方 patchDockerURL 一致。
//
// dockerode / axios 不接受 tcp:// 协议，官方是在运行时替换首个 tcp://。
func patchDockerURL(u string) string {
	return strings.Replace(u, "tcp://", "http://", 1)
}

// dockerHTTPClient 按 host 类型构造 http.Client。
func dockerHTTPClient(h DockerHostInfo, timeout time.Duration) (*http.Client, string, error) {
	tr := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
	}
	base := ""

	switch h.Type {
	case "socket", "":
		// 空类型按 socket 处理：老监控的 docker_type 可能为空
		sock := h.Daemon
		if sock == "" {
			sock = "/var/run/docker.sock"
		}
		sock = strings.TrimPrefix(sock, "unix://")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		base = "http://docker"

	case "tcp":
		if h.Daemon == "" {
			return nil, "", errors.New("Failed to load docker host config")
		}
		base = patchDockerURL(h.Daemon)
		if !strings.Contains(base, "://") {
			base = "http://" + base
		}
		if strings.HasPrefix(base, "https://") {
			// mTLS：三个证书文件齐了才挂上去，缺一个就只校验证书链
			host := hostOfURL(base)
			dir := filepath.Join(dockerTLSDir(), host)
			ca, cert, key :=
				filepath.Join(dir, dockerTLSFileNames[0]),
				filepath.Join(dir, dockerTLSFileNames[1]),
				filepath.Join(dir, dockerTLSFileNames[2])
			if fileExists(ca) && fileExists(cert) && fileExists(key) {
				pair, err := tls.LoadX509KeyPair(cert, key)
				if err != nil {
					return nil, "", fmt.Errorf("Failed to load docker TLS client certificate: %v", err)
				}
				pool := x509.NewCertPool()
				pem, err := os.ReadFile(ca)
				if err != nil {
					return nil, "", fmt.Errorf("Failed to load docker TLS CA certificate: %v", err)
				}
				pool.AppendCertsFromPEM(pem)
				tr.TLSClientConfig.RootCAs = pool
				tr.TLSClientConfig.Certificates = []tls.Certificate{pair}
			}
		}

	default:
		return nil, "", fmt.Errorf("Unsupported docker type: %s", h.Type)
	}

	return &http.Client{Transport: tr, Timeout: timeout}, strings.TrimRight(base, "/"), nil
}

// hostOfURL 取 URL 的 hostname（不带端口），用于定位证书目录。
func hostOfURL(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// dockerAPIGet 发一次 GET，返回响应体。
func dockerAPIGet(ctx context.Context, h DockerHostInfo, path string, timeout time.Duration) ([]byte, error) {
	client, base, err := dockerHTTPClient(h, timeout)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
			return nil, errors.New("Connection to Docker daemon timed out.")
		}
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("Docker API returned %d: %s", resp.StatusCode, msg)
	}
	return body, nil
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// dockerContainerState 是 /containers/<id>/json 里我们关心的部分。
type dockerContainerState struct {
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// Check 实现 Checker。
func (DockerChecker) Check(ctx context.Context, m *model.Monitor) Result {
	container := m.Get("docker_container")
	if container == "" {
		return Down("Container Name is required")
	}
	host := dockerHostFrom(m)
	if host.Type == "" && host.Daemon == "" {
		return Down("Failed to load docker host config")
	}

	// 官方超时 = interval * 1000 * 0.8
	timeout := time.Duration(float64(m.Interval) * 0.8 * float64(time.Second))
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if d := Timeout(m); d > 0 && d < timeout {
		timeout = d
	}

	start := time.Now()
	body, err := dockerAPIGet(ctx, host, "/containers/"+container+"/json", timeout)
	if err != nil {
		return Down("%s", err.Error())
	}

	var st dockerContainerState
	if err := json.Unmarshal(body, &st); err != nil {
		return Down("Invalid Docker response, is it Docker really a daemon?")
	}

	s := st.State
	if s.Status == "" && !s.Running {
		return Down("Container state is not available")
	}
	if !s.Running {
		return Down("Container State is %s", s.Status)
	}
	if s.Paused {
		return Down("Container is in a paused state")
	}

	elapsed := time.Since(start).Milliseconds()

	if s.Restarting {
		r := Result{Status: model.StatusPending, Msg: "Container is reporting it is currently restarting"}
		r.Ping = &elapsed
		return r
	}

	if s.Health != nil && s.Health.Status != "" && s.Health.Status != "none" {
		switch s.Health.Status {
		case "healthy":
			return Up("healthy", elapsed)
		case "unhealthy":
			return Down("Container State is unhealthy according to its healthcheck")
		default:
			r := Result{Status: model.StatusPending, Msg: s.Health.Status}
			r.Ping = &elapsed
			return r
		}
	}

	return Up(fmt.Sprintf(
		"Container has not reported health and is currently %s. As it is running, it is considered UP. "+
			"Consider adding a health check for better service visibility", s.Status), elapsed)
}

// TestDockerHost 供「测试连接」按钮使用，返回容器数量。
//
// 对齐官方 testDockerHost：`GET /containers/json?all=true`，
// 校验响应是数组，且非空时要带 ImageID 字段（否则说明对面不是 Docker）。
func TestDockerHost(ctx context.Context, h DockerHostInfo) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	body, err := dockerAPIGet(ctx, h, "/containers/json?all=true", 5*time.Second)
	if err != nil {
		return 0, err
	}

	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return 0, errors.New("Invalid Docker response, is it Docker really a daemon?")
	}
	if len(list) > 0 {
		if _, ok := list[0]["ImageID"]; !ok {
			return 0, errors.New("Invalid Docker response, is it Docker really a daemon?")
		}
	}
	return len(list), nil
}
