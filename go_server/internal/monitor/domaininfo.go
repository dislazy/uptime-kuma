package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/domainexpiry"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// 域名到期处理，对齐官方 monitor.js:1046 心跳钩子与 sendDomainInfo：
//
//   - 心跳后（!MAINTENANCE && domainExpiryNotification）：
//     checkSupport → checkExpiry（RDAP，一天缓存）→ sendNotifications
//   - domainInfo 推送：官方 sendStats 里 60 秒节流（INFO_REFRESH_INTERVAL_MS），
//     只读库里的 expiry，不发 RDAP
const domainInfoRefreshInterval = 60 * time.Second

// handleDomainExpiry 心跳后的域名到期检查 + 通知（官方 monitor.js:1046）。
func (mgr *Manager) handleDomainExpiry(ctx context.Context, mon *model.Monitor) {
	if mgr.domainSvc == nil {
		return
	}

	domain, _, serr := mgr.domainSvc.CheckSupport(ctx, mon.Type, mon.URL(), mon.Hostname(), mon.Get("grpc_url"))
	if serr != nil {
		// 官方：仅 no-rdap 打 warn，其余静默（catch 后按类型分流）
		if serr.Msg == domainexpiry.ErrNoRDAPEndpoint {
			logx.Warnf("domain_expiry",
				"Domain expiry unsupported for '.%s' because it lacks an RDAP endpoint in the IANA database."+
					" This isn't an Uptime Kuma bug, a limitation of your registry.",
				serr.Meta["publicSuffix"])
		} else {
			logx.Debugf("domain_expiry", "[%s] checkSupport: %s", mon.Name, serr.Msg)
		}
		return
	}

	expiry := mgr.domainSvc.CheckExpiry(ctx, mgr.domains, domain)
	if expiry == nil {
		logx.Debugf("monitor", "Failed getting expiration date for domain %s", domain)
		return
	}
	mgr.sendDomainExpiryNotifications(ctx, mon, domain)
}

// sendDomainExpiryNotifications 对齐官方 DomainExpiry.sendNotifications：
//
//   - notifyDays 升序遍历；daysRemaining > targetDays 不发
//   - lastSent <= targetDays 视为已发过（升序保证只发一档）
//   - 任一渠道成功 → 记录 lastSent 并返回
func (mgr *Manager) sendDomainExpiryNotifications(ctx context.Context, mon *model.Monitor, domain string) {
	bean, err := mgr.domains.FindByName(ctx, domain)
	if err != nil || bean == nil || bean.Expiry == nil {
		return
	}

	days := domainexpiry.DaysRemaining(*bean.Expiry)
	logx.Debugf("domain_expiry", "%s expires in %d days", domain, days)

	notifyDays := mgr.domainExpiryNotifyDays(ctx)
	sort.Slice(notifyDays, func(i, j int) bool { return notifyDays[i] < notifyDays[j] })

	for _, targetDays := range notifyDays {
		if days > targetDays {
			logx.Debugf("domain_expiry",
				"No need to send domain notification for %s (%d days valid) on %d deadline.",
				domain, days, targetDays)
			continue
		}
		if bean.LastExpiryNotificationSent != nil && *bean.LastExpiryNotificationSent <= targetDays {
			logx.Debugf("domain_expiry",
				"Notification for %s on %d deadline sent already, no need to send again.",
				domain, targetDays)
			continue
		}

		// 官方文案：Domain name {domain} will expire in {n} days
		msg := fmt.Sprintf("Domain name %s will expire in %d days", domain, days)
		if mgr.notify.NotifyCertificate(ctx, mon.ID, msg) {
			sent := targetDays
			bean.LastExpiryNotificationSent = &sent
			if err := mgr.domains.Save(ctx, bean); err != nil {
				logx.Warnf("domain_expiry", "记录域名提醒历史失败 %s: %v", domain, err)
			}
			return
		}
	}
}

// domainExpiryNotifyDays 读取提醒天数设置；缺失时按官方回置默认 [7,14,21]。
func (mgr *Manager) domainExpiryNotifyDays(ctx context.Context) []int64 {
	const def = "[7,14,21]"
	raw, ok, err := mgr.set.Raw(ctx, "domainExpiryNotifyDays")
	if err != nil || !ok || raw == "" || raw == "null" {
		// 官方：setSetting("domainExpiryNotifyDays", [7, 14, 21], "general")
		if err := mgr.set.Set(ctx, "domainExpiryNotifyDays", json.RawMessage(def), "general"); err != nil {
			logx.Warnf("monitor", "回置 domainExpiryNotifyDays 默认值失败: %v", err)
		}
		raw = def
	}

	var days []int64
	if err := json.Unmarshal([]byte(raw), &days); err != nil || len(days) == 0 {
		days = []int64{7, 14, 21}
	}
	return days
}

// maybeSendDomainInfo 对齐官方 Monitor.sendDomainInfo（monitor.js:1432）：
// 有客户端在线时 60 秒节流推送 domainInfo(monitorID, daysRemaining, expiresOn)。
// 只读 domain_expiry 已存的到期日，不触发 RDAP。
func (mgr *Manager) maybeSendDomainInfo(ctx context.Context, mon *model.Monitor) {
	if mgr.domainSvc == nil || mgr.hub.Count() == 0 {
		return
	}

	mgr.domainInfoMu.Lock()
	last, ok := mgr.domainInfoSent[mon.ID]
	if ok && time.Since(last) < domainInfoRefreshInterval {
		mgr.domainInfoMu.Unlock()
		return
	}
	if mgr.domainInfoSent == nil {
		mgr.domainInfoSent = map[int64]time.Time{}
	}
	mgr.domainInfoSent[mon.ID] = time.Now()
	mgr.domainInfoMu.Unlock()

	domain, _, serr := mgr.domainSvc.CheckSupport(ctx, mon.Type, mon.URL(), mon.Hostname(), mon.Get("grpc_url"))
	if serr != nil {
		// 官方：catch 后整体吞掉
		return
	}
	bean, err := mgr.domains.FindByName(ctx, domain)
	if err != nil || bean == nil || bean.Expiry == nil {
		return
	}

	// 官方 emit new Date(expiry)：ISO 字符串带毫秒
	expiresOn := bean.Expiry.UTC().Format("2006-01-02T15:04:05.000Z")
	mgr.hub.Broadcast("domainInfo", mon.ID, domainexpiry.DaysRemaining(*bean.Expiry), expiresOn)
}
