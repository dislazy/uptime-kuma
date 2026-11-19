package checkers

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// RedisChecker 用 RESP 协议的 PING 判断实例是否可用。
//
// 这里没有引 redis 客户端库：一次探测只需要 AUTH → SELECT → PING 三条命令，
// 自己实现协议比拉一个几千行的依赖更划算，也避免多一个需要跟进的安全通告源。
type RedisChecker struct{}

// Type 实现 Checker。
func (RedisChecker) Type() string { return "redis" }

func init() { Register(RedisChecker{}) }

// redisTarget 是从 DSN 解析出的连接参数。
type redisTarget struct {
	network  string // tcp / unix
	addr     string
	username string
	password string
	db       int
	useTLS   bool
}

// Check 建立连接并 PING 一次。
func (RedisChecker) Check(ctx context.Context, m *model.Monitor) Result {
	dsn := strings.TrimSpace(m.Get("database_connection_string"))
	if dsn == "" {
		return Down("Connection string is empty")
	}

	tgt, err := parseRedisDSN(dsn)
	if err != nil {
		return Down("%s", err.Error())
	}

	start := time.Now()

	// ignoreTls=true 表示"跳过证书校验"，与官方的 rejectUnauthorized 取反语义一致
	insecure := m.GetBool("ignore_tls")

	dialer := &net.Dialer{}
	var conn net.Conn
	if tgt.network == "unix" {
		conn, err = dialer.DialContext(ctx, "unix", tgt.addr)
	} else if tgt.useTLS {
		td := &tls.Dialer{
			NetDialer: dialer,
			Config: &tls.Config{
				ServerName:         hostOnly(tgt.addr),
				InsecureSkipVerify: insecure,
			},
		}
		conn, err = td.DialContext(ctx, "tcp", tgt.addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", tgt.addr)
	}
	if err != nil {
		return Down("%s", cleanDialError(err))
	}
	defer conn.Close()

	// 连接上设置截止时间，防止服务端接了连接却不回包（半开连接）
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	r := bufio.NewReader(conn)

	authUser, authPass := tgt.username, tgt.password
	// 监控表单里单独填的密码覆盖 DSN 里的，和官方 radiusPassword 的用法一致
	if p := m.Get("radius_password"); strings.TrimSpace(p) != "" {
		authPass = p
	}

	if authPass != "" {
		var reply string
		if authUser != "" {
			reply, err = redisCommand(conn, r, "AUTH", authUser, authPass)
		} else {
			reply, err = redisCommand(conn, r, "AUTH", authPass)
		}
		if err != nil {
			return Down("%s", err.Error())
		}
		if !strings.EqualFold(reply, "OK") {
			return Down("AUTH failed: %s", reply)
		}
	}

	if tgt.db != 0 {
		reply, err := redisCommand(conn, r, "SELECT", strconv.Itoa(tgt.db))
		if err != nil {
			return Down("%s", err.Error())
		}
		if !strings.EqualFold(reply, "OK") {
			return Down("SELECT %d failed: %s", tgt.db, reply)
		}
	}

	reply, err := redisCommand(conn, r, "PING")
	if err != nil {
		return Down("%s", err.Error())
	}

	ping := time.Since(start).Milliseconds()
	return Up(reply, ping)
}

// parseRedisDSN 解析 redis:// / rediss:// / redis+unix:// 三种写法。
//
// 不带 scheme 的裸 host:port 也接受，方便用户直接填 127.0.0.1:6379。
func parseRedisDSN(dsn string) (redisTarget, error) {
	var tgt redisTarget

	if !strings.Contains(dsn, "://") {
		tgt.network = "tcp"
		tgt.addr = ensurePort(dsn, "6379")
		return tgt, nil
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return tgt, fmt.Errorf("invalid connection string: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "redis":
		tgt.network = "tcp"
	case "rediss":
		tgt.network = "tcp"
		tgt.useTLS = true
	case "redis+unix", "unix":
		tgt.network = "unix"
		// unix 的路径在 host 部分（redis+unix:///var/run/redis.sock）或 path 里
		tgt.addr = u.Path
		if u.Host != "" {
			tgt.addr = u.Host + u.Path
		}
		if strings.TrimSpace(tgt.addr) == "" {
			return tgt, fmt.Errorf("unix socket path is empty")
		}
		if u.User != nil {
			tgt.username = u.User.Username()
			tgt.password, _ = u.User.Password()
		}
		return tgt, nil
	default:
		return tgt, fmt.Errorf("unsupported redis scheme %q", u.Scheme)
	}

	tgt.addr = ensurePort(u.Host, "6379")

	if u.User != nil {
		tgt.username = u.User.Username()
		if p, ok := u.User.Password(); ok {
			tgt.password = p
		} else if tgt.username != "" {
			// redis://pass@host 这种老写法里，唯一的值就是密码
			tgt.password = tgt.username
			tgt.username = ""
		}
	}

	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return tgt, fmt.Errorf("invalid database index %q", p)
		}
		tgt.db = n
	}

	return tgt, nil
}

// redisCommand 发送一条命令并读取回复，返回其文本形式。
func redisCommand(w io.Writer, r *bufio.Reader, args ...string) (string, error) {
	var sb strings.Builder
	sb.WriteString("*")
	sb.WriteString(strconv.Itoa(len(args)))
	sb.WriteString("\r\n")
	for _, a := range args {
		sb.WriteString("$")
		sb.WriteString(strconv.Itoa(len(a)))
		sb.WriteString("\r\n")
		sb.WriteString(a)
		sb.WriteString("\r\n")
	}

	if _, err := io.WriteString(w, sb.String()); err != nil {
		return "", fmt.Errorf("send %s failed: %w", args[0], err)
	}

	return readRESP(r)
}

// readRESP 读一条 RESP 回复。只处理探测场景会用到的几种类型。
func readRESP(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read reply failed: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", fmt.Errorf("empty reply")
	}

	switch line[0] {
	case '+': // 简单字符串
		return line[1:], nil
	case '-': // 错误
		return "", fmt.Errorf("%s", line[1:])
	case ':': // 整数
		return line[1:], nil
	case '$': // 批量字符串
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", fmt.Errorf("bad bulk length %q", line[1:])
		}
		if n < 0 { // $-1 表示 nil
			return "", nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("read bulk failed: %w", err)
		}
		return string(buf[:n]), nil
	case '*': // 数组：拼成空格分隔的文本，足够展示
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return "", nil
		}
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			s, err := readRESP(r)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, " "), nil
	default:
		return "", fmt.Errorf("unexpected reply %q", line)
	}
}

// ensurePort 在缺少端口时补上默认端口。
func ensurePort(hostport, defaultPort string) string {
	if hostport == "" {
		return net.JoinHostPort("127.0.0.1", defaultPort)
	}
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	// IPv6 字面量且无端口时 SplitHostPort 也会失败，这里按裸主机处理
	return net.JoinHostPort(strings.Trim(hostport, "[]"), defaultPort)
}

// hostOnly 去掉端口，TLS ServerName 只接受主机名。
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}
