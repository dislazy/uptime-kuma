package checkers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// SteamChecker 通过 Steam Web API 的 GetServerList 判断服务器是否在线。
//
// 官方要求先在设置里填 Steam API Key，然后用 `addr\ip:port` 过滤，
// 命中后 heartbeat.msg 就是服务器名（服务器的 sv_hostname）。
type SteamChecker struct{}

// Type 实现 Checker。
func (SteamChecker) Type() string { return "steam" }

func init() { Register(SteamChecker{}) }

const steamAPIURL = "https://api.steampowered.com/IGameServersService/GetServerList/v1/"

// Check 实现 Checker。
func (SteamChecker) Check(ctx context.Context, m *model.Monitor) Result {
	apiKey := Setting(ctx, "steamAPIKey", "")
	if apiKey == "" {
		return Down("Steam API Key not found")
	}

	host := m.Hostname()
	if host == "" {
		return Down("Hostname is required")
	}
	port := m.Port()
	if port <= 0 {
		return Down("Port is required")
	}

	resolved, err := resolveIPv4(ctx, host)
	if err != nil {
		return Down("Unable to resolve Steam server hostname %q: %v", host, err)
	}

	filter := fmt.Sprintf(`addr\%s:%d`, resolved, port)

	q := url.Values{}
	q.Set("filter", filter)
	q.Set("key", apiKey)

	client, err := buildClient(m)
	if err != nil {
		return Down("%s", err.Error())
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, steamAPIURL+"?"+q.Encode(), nil)
	if err != nil {
		return Down("%s", err.Error())
	}
	req.Header.Set("Accept", "*/*")

	res, err := client.Do(req)
	if err != nil {
		return Down("%s", cleanHTTPError(err))
	}
	defer res.Body.Close()

	if !statusAccepted(res.StatusCode, m.Get("accepted_statuscodes_json")) {
		return Down("%d - %s", res.StatusCode, http.StatusText(res.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Down("%s", err.Error())
	}

	var payload struct {
		Response struct {
			Servers []struct {
				Name string `json:"name"`
			} `json:"servers"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Down("Invalid response from Steam API: %v", err)
	}

	if len(payload.Response.Servers) == 0 {
		return Down("Server not found on Steam")
	}

	name := payload.Response.Servers[0].Name
	ping := time.Since(start).Milliseconds()

	// 官方随后用 ICMP 再量一次延迟，失败就保留原值
	if r := (PingChecker{}).Check(ctx, m); r.Status == model.StatusUp && r.Ping != nil {
		ping = *r.Ping
	}

	return Up(name, ping)
}

// resolveIPv4 解析主机名，优先取 IPv4（Steam 的 addr 过滤对 IPv4 更友好）。
func resolveIPv4(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("DNS lookup returned no addresses")
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			return a.IP.String(), nil
		}
	}
	return addrs[0].IP.String(), nil
}
