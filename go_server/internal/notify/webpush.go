package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// Web Push（浏览器推送）。
//
// 前端在"通知"里新建 Webpush 渠道时，会先问后端要 VAPID 公钥
// （socket 事件 getWebpushVapidPublicKey），订阅成功后把整个
// PushSubscription 存进 notification.config.subscription。
// 所以这里只需要：拿到 subscription + 服务器私钥，发一条加密推送。
//
// 密钥在首次使用时生成并写进设置表，与官方行为一致
// （webpushPublicVapidKey / webpushPrivateVapidKey）。

// vapidSubject 是 VAPID 的 contact，官方用的是项目地址。
const vapidSubject = "https://github.com/louislam/uptime-kuma"

func init() {
	registerFunc("Webpush", sendWebpush)
}

// EnsureVAPIDKeys 确保设置里有 VAPID 密钥对，返回公钥。
func EnsureVAPIDKeys(ctx context.Context, settings *store.Settings) (string, error) {
	pub := settings.GetString(ctx, "webpushPublicVapidKey", "")
	priv := settings.GetString(ctx, "webpushPrivateVapidKey", "")
	if pub != "" && priv != "" {
		return pub, nil
	}

	privKey, pubKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", err
	}
	if err := settings.Set(ctx, "webpushPublicVapidKey", pubKey, "internal"); err != nil {
		return "", err
	}
	if err := settings.Set(ctx, "webpushPrivateVapidKey", privKey, "internal"); err != nil {
		return "", err
	}
	return pubKey, nil
}

func sendWebpush(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	sub, err := webpushSubscription(cfg)
	if err != nil {
		return "", err
	}

	pub := Setting(ctx, "webpushPublicVapidKey", "")
	priv := Setting(ctx, "webpushPrivateVapidKey", "")
	if pub == "" || priv == "" {
		return "", errors.New("Web Push VAPID keys are not configured. Open Settings and save the Web Push notification once to generate them.")
	}

	payload, _ := json.Marshal(map[string]string{
		"title": "Uptime Kuma",
		"body":  msgBody(m),
	})

	// TTL 30 秒：告警过期就没意义了，官方默认也是 30
	res, err := webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{
		Subscriber:      vapidSubject,
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             30,
	})
	if err != nil {
		return "", err
	}
	if res != nil {
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			// 404/410 表示订阅已失效，提示用户重新订阅
			if res.StatusCode == 404 || res.StatusCode == 410 {
				return "", fmt.Errorf("Web Push subscription is no longer valid (HTTP %d), please re-subscribe in the browser", res.StatusCode)
			}
			return "", fmt.Errorf("Web Push failed: HTTP %d", res.StatusCode)
		}
	}
	return "Sent Successfully.", nil
}

// webpushSubscription 解析前端存下来的 PushSubscription。
//
// 官方结构：{ endpoint, expirationTime, keys: { p256dh, auth } }
// 有些浏览器会省略 expirationTime，所以这里不要求它存在。
func webpushSubscription(cfg map[string]any) (*webpush.Subscription, error) {
	raw := cfgRaw(cfg, "subscription")
	if raw == nil {
		return nil, errors.New("Web Push subscription is required")
	}

	// config 里的 subscription 可能是对象，也可能是 JSON 字符串
	var obj map[string]any
	switch t := raw.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, errors.New("Web Push subscription is required")
		}
		if err := json.Unmarshal([]byte(t), &obj); err != nil {
			return nil, fmt.Errorf("invalid Web Push subscription: %v", err)
		}
	case map[string]any:
		obj = t
	default:
		return nil, errors.New("invalid Web Push subscription")
	}

	endpoint := cfgValueString(obj["endpoint"])
	if endpoint == "" {
		return nil, errors.New("Web Push subscription endpoint is missing")
	}

	keys, _ := obj["keys"].(map[string]any)
	p256dh := cfgValueString(keys["p256dh"])
	auth := cfgValueString(keys["auth"])
	if p256dh == "" || auth == "" {
		return nil, errors.New("Web Push subscription keys are missing")
	}

	return &webpush.Subscription{
		Endpoint: endpoint,
		Keys: webpush.Keys{
			P256dh: p256dh,
			Auth:   auth,
		},
	}, nil
}
