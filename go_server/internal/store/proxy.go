package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SupportedProxyProtocols 与官方 server/proxy.js 的 SUPPORTED_PROXY_PROTOCOLS 一致。
var SupportedProxyProtocols = []string{"http", "https", "socks", "socks5", "socks5h", "socks4"}

// Proxy 对应官方 proxy 表（官方 knex_init_db.js:37，全 snake_case 列）。
type Proxy struct {
	ID          int64
	UserID      any
	Protocol    string
	Host        string
	Port        int64
	Auth        bool
	Username    string
	Password    string
	Active      bool
	Default     bool
	CreatedDate string

	// ApplyExisting 是保存动作的附加选项（官方 Proxy.save 读 proxy.applyExisting）：
	// 勾选后把所有监控的 proxy_id 批量指到本代理。不落库、不下发前端。
	ApplyExisting bool
}

// JSON 返回给前端的形态，字段名与官方 model/proxy.js toJSON() 一致。
func (p Proxy) JSON() map[string]any {
	return map[string]any{
		"id":          p.ID,
		"userId":      p.UserID,
		"protocol":    p.Protocol,
		"host":        p.Host,
		"port":        p.Port,
		"auth":        p.Auth,
		"username":    p.Username,
		"password":    p.Password,
		"active":      p.Active,
		"default":     p.Default,
		"createdDate": p.CreatedDate,
	}
}

// ProxyURL 拼出 scheme://user:pass@host:port 形式的代理地址。
// 官方 createAgents 用 new URL(`${protocol}://${host}:${port}`) + userinfo。
func (p Proxy) ProxyURL() string {
	u := url.URL{
		Scheme: strings.ToLower(p.Protocol),
		Host:   p.Host + ":" + strconv.FormatInt(p.Port, 10),
	}
	if p.Auth {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

// Proxies 列出全部代理（官方 sendProxyList 用 R.find("proxy")，无过滤）。
func (s *MonitorStore) Proxies(ctx context.Context) ([]Proxy, error) {
	if !s.db.HasTable(ctx, "proxy") {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, "SELECT * FROM proxy ORDER BY id")
	if err != nil {
		return nil, err
	}
	return proxyRows(rows), nil
}

// SaveProxy 新建或更新代理，语义对齐官方 Proxy.save：
//   - proxyID > 0 时更新，找不到报 "proxy not found"
//   - protocol 必须在支持列表内
//   - default=1 时先把旧 default 全部置 0
//   - active 恒存 1：官方 `bean.active = proxy.active || true` 是恒真表达式
//     （false || true === true），前端开关实际不生效，这里保持一致
//   - applyExisting 时把所有 proxy_id 不等于本代理的监控批量指过来
func (s *MonitorStore) SaveProxy(ctx context.Context, id int64, userID int64, p Proxy) (int64, error) {
	if !s.db.HasTable(ctx, "proxy") {
		return 0, errors.New("proxy table is missing")
	}

	p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
	if !supportedProxyProtocol(p.Protocol) {
		return 0, fmt.Errorf(
			"Unsupported proxy protocol %q. Supported protocols are %s.",
			p.Protocol, strings.Join(SupportedProxyProtocols, ", "))
	}

	if p.Default {
		if _, err := s.db.Exec(ctx, "UPDATE proxy SET [default] = 0 WHERE [default] = 1"); err != nil {
			return 0, err
		}
	}

	values := map[string]any{
		"protocol": p.Protocol,
		"host":     p.Host,
		"port":     p.Port,
		"auth":     p.Auth,
		"username": p.Username,
		"password": p.Password,
		"active":   true, // 官方 `proxy.active || true` 恒真，见函数注释
		"default":  p.Default,
	}
	// user_id 是 INT NOT NULL：better-auth 的用户 ID 是随机字符串，
	// 直接写进去在 MySQL 严格模式下会 1265 Data truncated（SQLite 会宽容地存成文本）。
	// 统一按 maintenance.go 里 numericUserID 的既定策略：非纯数字记 0。
	values["user_id"] = userID

	if id > 0 {
		found, err := s.db.Query(ctx, "SELECT id FROM proxy WHERE id = ?", id)
		if err != nil {
			return 0, err
		}
		if len(found) == 0 {
			return 0, errors.New("proxy not found")
		}
		if err := s.db.Update(ctx, "proxy", id, values); err != nil {
			return 0, err
		}
	} else {
		newID, err := s.db.Insert(ctx, "proxy", values)
		if err != nil {
			return 0, err
		}
		id = newID
	}

	if p.ApplyExisting {
		if err := s.applyProxyEveryMonitor(ctx, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// DeleteProxy 删除代理，并把引用它的监控置空（官方 Proxy.delete 同语义）。
func (s *MonitorStore) DeleteProxy(ctx context.Context, id int64) error {
	if !s.db.HasTable(ctx, "proxy") {
		return errors.New("proxy table is missing")
	}
	found, err := s.db.Query(ctx, "SELECT id FROM proxy WHERE id = ?", id)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return errors.New("proxy not found")
	}

	if col := s.monitorProxyColumn(ctx); col != "" {
		if _, err := s.db.Exec(ctx,
			fmt.Sprintf("UPDATE monitor SET %s = NULL WHERE %s = ?", col, col), id); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(ctx, "DELETE FROM proxy WHERE id = ?", id)
	return err
}

// ActiveProxyByID 按 ID 取启用中的代理，供探测时解析（官方 R.load + proxy.active 判定）。
// 找不到或未启用都返回错误，探测侧回退直连。
func (s *MonitorStore) ActiveProxyByID(ctx context.Context, id int64) (Proxy, error) {
	if id <= 0 || !s.db.HasTable(ctx, "proxy") {
		return Proxy{}, fmt.Errorf("proxy %d not found", id)
	}
	rows, err := s.db.Query(ctx, "SELECT * FROM proxy WHERE id = ?", id)
	if err != nil {
		return Proxy{}, err
	}
	if len(rows) == 0 {
		return Proxy{}, fmt.Errorf("proxy %d not found", id)
	}
	list := proxyRows(rows)
	if !list[0].Active {
		return Proxy{}, fmt.Errorf("proxy %d is not active", id)
	}
	return list[0], nil
}

// applyProxyEveryMonitor 官方 applyProxyEveryMonitor：把 proxy_id 不等于本代理的监控全部指过来。
// 官方逐行比较 `monitor.proxy_id !== proxyID`（null 也算不等），等价于
// `WHERE proxy_id IS NULL OR proxy_id <> ?`。
func (s *MonitorStore) applyProxyEveryMonitor(ctx context.Context, proxyID int64) error {
	col := s.monitorProxyColumn(ctx)
	if col == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, fmt.Sprintf(
		"UPDATE monitor SET %s = ? WHERE %s IS NULL OR %s <> ?", col, col, col), proxyID, proxyID)
	return err
}

// monitorProxyColumn 解析 monitor 表里 proxy 外键的真实列名（proxy_id / proxyId）。
func (s *MonitorStore) monitorProxyColumn(ctx context.Context) string {
	cols := s.db.Columns(ctx, "monitor")
	return resolveColumn(cols, "proxy_id")
}

func proxyRows(rows []Row) []Proxy {
	out := make([]Proxy, 0, len(rows))
	for _, r := range rows {
		out = append(out, Proxy{
			ID:          r.Int("id"),
			UserID:      r["user_id"],
			Protocol:    strings.ToLower(r.Str("protocol")),
			Host:        r.Str("host"),
			Port:        r.Int("port"),
			Auth:        r.Bool("auth"),
			Username:    r.Str("username"),
			Password:    r.Str("password"),
			Active:      r.Bool("active"),
			Default:     r.Bool("default"),
			CreatedDate: r.Str("created_date"),
		})
	}
	return out
}

func supportedProxyProtocol(p string) bool {
	for _, s := range SupportedProxyProtocols {
		if s == p {
			return true
		}
	}
	return false
}
