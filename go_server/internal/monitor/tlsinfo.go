package monitor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"

	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// TLS 证书信息处理，对齐官方 Monitor.handleTlsInfo（server/model/monitor.js:2229）：
//
//	updateTlsInfo（落库）→ prometheus → 证书到期通知
//
// 官方在 https 握手成功（secureConnect）后采集，valid 但状态 DOWN 也照采。

// handleTLSInfo 一次心跳的 TLS 信息落库 + 推送 + 到期检查。
func (mgr *Manager) handleTLSInfo(ctx context.Context, mon *model.Monitor, state *tls.ConnectionState) {
	info := checkers.BuildTLSInfo(state, checkers.HostnameOf(mon.URL()))
	if info == nil {
		return
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		logx.Warnf("monitor", "序列化证书信息失败 monitor #%d: %v", mon.ID, err)
		return
	}

	// updateTlsInfo：upsert + 换证书清空通知历史（store 层实现官方语义）
	if changed, err := mgr.tls.Upsert(ctx, mon.ID, string(infoJSON)); err != nil {
		logx.Warnf("monitor", "证书信息落库失败 monitor #%d: %v", mon.ID, err)
	} else if changed {
		logx.Infof("monitor", "[%s] 检测到证书变更", mon.Name)
	}

	// sendCertInfo：推送给 dashboard（前端 socket.js 监听 certInfo）
	mgr.hub.Broadcast("certInfo", mon.ID, string(infoJSON))

	// 官方：!ignoreTls && expiryNotification 时检查到期提醒
	if !mon.GetBool("ignore_tls") && mon.GetBool("expiry_notification") {
		mgr.checkCertExpiryNotifications(ctx, mon, info, checkers.KnownRootFPs(state))
	}
}

// checkCertExpiryNotifications 对齐官方 util.ts:843 checkCertExpiryNotifications：
//
//   - tlsExpiryNotifyDays 缺失时种子默认 [7, 14, 21]（官方同款回置行为）
//   - 遍历链上每张证书 × 每个目标天数
//   - 已知系统根证书 → 跳过（官方 monitor.rootCertificates）
//   - daysRemaining > targetDays → 不发
//   - 其余 → 按 targetDays 去重后发送
func (mgr *Manager) checkCertExpiryNotifications(ctx context.Context, mon *model.Monitor, info map[string]any, rootFPs map[string]bool) {
	certHead, _ := info["certInfo"].(map[string]any)
	if certHead == nil {
		return
	}
	if days, ok := numField(certHead, "daysRemaining"); !ok || days <= 0 {
		// 官方：!tlsInfoObject.certInfo.daysRemaining 直接返回
		return
	}

	// fail-fast：没绑通知渠道就别白跑
	if !mgr.notify.HasNotifications(ctx, mon.ID) {
		return
	}

	notifyDays := mgr.tlsExpiryNotifyDays(ctx)

	for _, targetDays := range notifyDays {
		cert := certHead
		for cert != nil {
			fp, _ := cert["fingerprint256"].(string)
			if rootFPs[fp] {
				// 官方：已知根证书不提醒
				break
			}
			days, ok := numField(cert, "daysRemaining")
			if !ok {
				break
			}
			if days > int64(targetDays) {
				cert = childCert(cert)
				continue
			}
			mgr.sendCertNotificationByTargetDays(ctx, mon, cert, days, int64(targetDays))
			cert = childCert(cert)
		}
	}
}

// sendCertNotificationByTargetDays 对齐官方 monitor.sendCertNotificationByTargetDays：
// notification_sent_history 去重 → 全渠道纯文本 → 成功才记历史。
func (mgr *Manager) sendCertNotificationByTargetDays(ctx context.Context, mon *model.Monitor, cert map[string]any, days, targetDays int64) {
	sent, err := mgr.tls.HasCertNotification(ctx, mon.ID, targetDays)
	if err == nil && sent {
		return
	}

	subjectCN := subjectCNOf(cert)
	certType, _ := cert["certType"].(string)

	// 官方文案：[name][url] {certType} certificate {CN} will expire in {n} days
	msg := fmt.Sprintf("[%s][%s] %s certificate %s will expire in %d days",
		mon.Name, mon.URL(), certType, subjectCN, days)

	if mgr.notify.NotifyCertificate(ctx, mon.ID, msg) {
		if err := mgr.tls.MarkCertNotification(ctx, mon.ID, targetDays); err != nil {
			logx.Warnf("monitor", "记录证书提醒历史失败 monitor #%d: %v", mon.ID, err)
		}
	}
}

// tlsExpiryNotifyDays 读取提醒天数设置；缺失时按官方回置默认 [7,14,21]。
func (mgr *Manager) tlsExpiryNotifyDays(ctx context.Context) []int64 {
	const def = "[7,14,21]"
	raw, ok, err := mgr.set.Raw(ctx, "tlsExpiryNotifyDays")
	if err != nil || !ok || raw == "" || raw == "null" {
		// 官方：Settings.set("tlsExpiryNotifyDays", [7,14,21], "general")
		if err := mgr.set.Set(ctx, "tlsExpiryNotifyDays", json.RawMessage(def), "general"); err != nil {
			logx.Warnf("monitor", "回置 tlsExpiryNotifyDays 默认值失败: %v", err)
		}
		raw = def
	}

	var days []int64
	if err := json.Unmarshal([]byte(raw), &days); err != nil || len(days) == 0 {
		days = []int64{7, 14, 21}
	}
	return days
}

// childCert 取链上的下一张证书（官方 issuerCertificate 遍历）。
func childCert(cert map[string]any) map[string]any {
	next, _ := cert["issuerCertificate"].(map[string]any)
	return next
}

// numField 从 map 里取数字（JSON 数字是 float64）。
func numField(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// subjectCNOf 取证书 subject.CN（官方 subjectCN = certInfo.subject["CN"]）。
func subjectCNOf(cert map[string]any) string {
	subject, _ := cert["subject"].(map[string]any)
	if subject == nil {
		return ""
	}
	cn, _ := subject["CN"].(string)
	return cn
}
