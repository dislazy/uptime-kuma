// Package domainexpiry 对齐官方 server/model/domain_expiry.js：
// RDAP 域名到期查询 + checkSupport 校验 + IANA RDAP 服务映射（周缓存 + 落库兜底）。
//
// 官方链路（monitor.js:1046 心跳钩子）：
//
//	domainExpiryNotification 开启时：
//	  checkSupport（类型清单 → 空目标 → 非 ICANN → 无 RDAP 四层校验）
//	  → checkExpiry（RDAP 查到期日，一天内不重查）
//	  → sendNotifications（阈值 [7,14,21]，lastExpiryNotificationSent 去重）
package domainexpiry

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

//go:embed rdap-dns.json
var fallbackRDAPData []byte

// IANADataURL 官方 getRdapDnsData 拉取的映射表地址。
const IANADataURL = "https://data.iana.org/rdap/dns.json"

// ianaRefreshInterval 映射表缓存一周；失败时降级后 24 小时再试（官方同款节奏）。
const (
	ianaRefreshInterval = 7 * 24 * time.Hour
	ianaRetryInterval   = 24 * time.Hour
	ianaFetchTimeout    = 30 * time.Second
)

// TypesWithDomainExpirySupportViaField 官方 TYPES_WITH_DOMAIN_EXPIRY_SUPPORT_VIA_FIELD
// （src/util.ts:647）：监控类型 → 取目标值的字段名。
var TypesWithDomainExpirySupportViaField = map[string]string{
	"http":              "url",
	"keyword":           "url",
	"json-query":        "url",
	"real-browser":      "url",
	"websocket-upgrade": "url",
	"port":              "hostname",
	"ping":              "hostname",
	"dns":               "hostname",
	"smtp":              "hostname",
	"snmp":              "hostname",
	"gamedig":           "hostname",
	"steam":             "hostname",
	"mqtt":              "hostname",
	"radius":            "hostname",
	"tailscale-ping":    "hostname",
	"sip-options":       "hostname",
	"grpc-keyword":      "grpcUrl",
}

// SupportError 对齐官方 TranslatableError：Msg 是 i18n key，前端按 msgi18n 翻译。
type SupportError struct {
	Msg  string
	Meta map[string]any
}

// Error 实现 error。
func (e *SupportError) Error() string { return e.Msg }

// 支持性校验失败的官方 i18n key（en.json 1522-1525）。
const (
	ErrUnsupportedMonitorType = "domain_expiry_unsupported_monitor_type"
	ErrMissingTarget          = "domain_expiry_unsupported_missing_target"
	ErrNotICANN               = "domain_expiry_unsupported_is_icann"
	ErrNoRDAPEndpoint         = "domain_expiry_unsupported_unsupported_tld_no_rdap_endpoint"
)

// fetchFunc 可注入的 HTTP GET，便于测试。
type fetchFunc func(ctx context.Context, url string) ([]byte, error)

// Service 持有 IANA 映射表缓存与设置持久化。
type Service struct {
	set   *store.Settings
	fetch fetchFunc

	mu   sync.Mutex
	data *rdapData
	next time.Time
}

// rdapData 是 IANA dns.json 的形状：services: [ [tlds...], [urls...] ]。
type rdapData struct {
	Services [][][]string `json:"services"`
}

// NewService 构造。
func NewService(set *store.Settings) *Service {
	return &Service{set: set, fetch: httpFetch}
}

// httpFetch 默认抓取实现。
func httpFetch(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: ianaFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// CheckSupport 对齐官方 DomainExpiry.checkSupport（domain_expiry.js:198）：
// 四层校验，全过返回 {domain, rootTld}。
func (s *Service) CheckSupport(ctx context.Context, typ, url, hostname, grpcURL string) (string, string, *SupportError) {
	field, ok := TypesWithDomainExpirySupportViaField[typ]
	if !ok {
		return "", "", &SupportError{Msg: ErrUnsupportedMonitorType}
	}

	var target string
	switch field {
	case "url":
		target = url
	case "hostname":
		target = hostname
	case "grpcUrl":
		target = grpcURL
	}
	if strings.TrimSpace(target) == "" {
		return "", "", &SupportError{Msg: ErrMissingTarget}
	}

	host := hostFromValue(field, target)

	// 官方用 tldts 解析；Go 侧 x/net/publicsuffix 语义等价：
	// publicSuffix + isIcann + eTLD+1（domain）。
	suffix, icann := publicSuffixOf(host)
	domain := registeredDomain(host)

	// 官方最先过滤非 ICANN 域名（内网 IP、PSL 私有段等）。
	if !icann || suffix == "" {
		// 官方 meta：domain ?? hostname ?? "EMPTY DOMAIN"
		d := domain
		if d == "" {
			d = host
		}
		if d == "" {
			d = "EMPTY DOMAIN"
		}
		return "", "", &SupportError{
			Msg:  ErrNotICANN,
			Meta: map[string]any{"domain": d, "publicSuffix": suffix},
		}
	}

	// RDAP 服务端点必须存在，否则该 TLD 无法查询（官方 if (!rdap) throw）。
	if endpoint, err := s.serverFor(ctx, suffix); err != nil || endpoint == "" {
		return "", "", &SupportError{
			Msg:  ErrNoRDAPEndpoint,
			Meta: map[string]any{"publicSuffix": suffix},
		}
	}

	return domain, lastLabel(suffix), nil
}

// ExpiryDate 对齐官方 getRdapDomainExpiryDate：查 RDAP 的 expiration 事件。
// 任何失败（无服务端点、HTTP 非 200、无 events）都返回 nil（官方静默 null）。
func (s *Service) ExpiryDate(ctx context.Context, domain string) *time.Time {
	suffix, _ := publicSuffixOf(domain)
	endpoint, err := s.serverFor(ctx, suffix)
	if err != nil || endpoint == "" {
		return nil
	}

	body, err := s.fetch(ctx, strings.TrimSuffix(endpoint, "/")+"/domain/"+url.PathEscape(domain))
	if err != nil {
		logx.Debugf("rdap", "Not able to get expiry date from RDAP: %v", err)
		return nil
	}

	var doc struct {
		Events []struct {
			EventAction string `json:"eventAction"`
			EventDate   string `json:"eventDate"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	for _, ev := range doc.Events {
		if ev.EventAction == "expiration" {
			if t, err := time.Parse(time.RFC3339, ev.EventDate); err == nil {
				return &t
			}
		}
	}
	return nil
}

// CheckExpiry 对齐官方 DomainExpiry.checkExpiry：
//   - lastCheck 未满一天 → 直接用库里的 expiry（可能为 nil）
//   - 否则查 RDAP；新到期日在旧值之后 → 重置已发通知标记
//   - 查不到（nil）也照写 lastCheck，避免每拍都打 RDAP
func (s *Service) CheckExpiry(ctx context.Context, ds *store.DomainExpiryStore, domain string) *time.Time {
	bean, err := ds.FindByName(ctx, domain)
	if err != nil {
		logx.Debugf("domain_expiry", "读取 %s 失败: %v", domain, err)
	}
	now := time.Now().UTC()

	if bean != nil && bean.LastCheck != nil && now.Sub(*bean.LastCheck) < 24*time.Hour {
		// 官方：dayjs diff("day") < 1，即未满 24 小时不重查
		return bean.Expiry
	}

	expiry := s.ExpiryDate(ctx, domain)
	if bean == nil {
		bean = &store.DomainExpiry{Domain: domain}
	}
	// 官方：新到期日在旧值之后 → 重置 lastExpiryNotificationSent
	if bean.Expiry != nil && expiry != nil && expiry.After(*bean.Expiry) {
		bean.LastExpiryNotificationSent = nil
	}
	bean.Expiry = expiry
	bean.LastCheck = &now
	if err := ds.Save(ctx, bean); err != nil {
		logx.Warnf("domain_expiry", "保存 %s 失败: %v", domain, err)
	}
	return expiry
}

// serverFor 找 publicSuffix 对应的 RDAP 服务端点（官方 getRdapServer）。
// 无匹配返回空串（官方返回 null）。
func (s *Service) serverFor(ctx context.Context, publicSuffix string) (string, error) {
	rootTld := lastLabel(publicSuffix)
	if rootTld == "" {
		return "", nil
	}
	data, err := s.getRdapDnsData(ctx)
	if err != nil {
		return "", err
	}
	for _, svc := range data.Services {
		if len(svc) < 2 || len(svc[1]) == 0 {
			continue
		}
		for _, t := range svc[0] {
			if strings.EqualFold(t, rootTld) {
				return svc[1][0], nil
			}
		}
	}
	logx.Debugf("rdap", "No RDAP server found for TLD %s", rootTld)
	return "", nil
}

// data 返回 IANA 映射表：内存缓存 → 网络更新 → 落库旧值 → 内嵌兜底。
func (s *Service) getRdapDnsData(ctx context.Context) (*rdapData, error) {
	s.mu.Lock()
	if s.data != nil && time.Now().Before(s.next) {
		d := s.data
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()

	// 抓取期间持锁串行（官方 running 标志防并发的简化等价）。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data != nil && time.Now().Before(s.next) {
		return s.data, nil
	}

	raw, err := s.fetch(ctx, IANADataURL)
	if err == nil {
		var d rdapData
		if jerr := json.Unmarshal(raw, &d); jerr == nil && len(d.Services) > 0 {
			s.data, s.next = &d, time.Now().Add(ianaRefreshInterval)
			// 官方：Settings.set("rdapDnsData", data) 持久化，供重启后离线兜底
			if serr := s.set.Set(ctx, "rdapDnsData", json.RawMessage(raw), "general"); serr != nil {
				logx.Warnf("rdap", "落库 rdapDnsData 失败: %v", serr)
			} else {
				logx.Infof("rdap", "RDAP DNS data updated successfully. Number of services: %d", len(d.Services))
			}
			return s.data, nil
		}
		err = fmt.Errorf("invalid RDAP DNS data structure")
	}

	logx.Infof("rdap", "Unable to update RDAP DNS data from source: %v", err)
	d := s.offlineData(ctx)
	s.data, s.next = d, time.Now().Add(ianaRetryInterval)
	return s.data, nil
}

// offlineData 官方 getOfflineRdapDnsData：设置里的旧值 → 内嵌文件。
func (s *Service) offlineData(ctx context.Context) *rdapData {
	if s.set != nil {
		if raw, ok, err := s.set.Raw(ctx, "rdapDnsData"); err == nil && ok && raw != "" && raw != "null" {
			var d rdapData
			if err := json.Unmarshal([]byte(raw), &d); err == nil && len(d.Services) > 0 {
				return &d
			}
		}
	}
	var d rdapData
	if err := json.Unmarshal(fallbackRDAPData, &d); err != nil || len(d.Services) == 0 {
		return &rdapData{}
	}
	return &d
}

// hostFromValue 从目标字段值里提取裸主机名。
//
// 官方 tldts 能直接吃 URL / host:port；Go 侧需先归一化：
//   - url 字段：完整 URL（https://a.b/x）→ Hostname；退化写法（a.b:8080）→ 剥端口
//   - hostname / grpcUrl 字段：host 或 host:port → 剥端口
func hostFromValue(field, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if field == "url" {
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			return strings.Trim(u.Hostname(), "[]")
		}
	}
	if h, _, err := net.SplitHostPort(v); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(v, "[]")
}

// publicSuffixOf 取 PSL 公共后缀与 ICANN 归属。
//
// tldts 对 IP 的 publicSuffix 返回空；x/net 会把整个 IP 当后缀，特判对齐
// （影响 ErrNotICANN 报错文案 ".{publicSuffix}" 的显示）。
func publicSuffixOf(host string) (string, bool) {
	if net.ParseIP(host) != nil {
		return "", false
	}
	return publicsuffix.PublicSuffix(host)
}

// registeredDomain 取 eTLD+1（官方 tldts 的 domain 字段）。
func registeredDomain(host string) string {
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return d
}

// lastLabel 取后缀的最后一个标签（官方 rootTld = publicSuffix.split(".").pop()）。
func lastLabel(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ".")
	return parts[len(parts)-1]
}

// DaysRemaining 对齐官方 dayjs.utc(expiry).diff(now, "day") 的截断语义。
func DaysRemaining(expiry time.Time) int64 {
	return int64(time.Until(expiry).Hours() / 24)
}
