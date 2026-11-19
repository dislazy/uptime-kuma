package notify

import (
	"testing"
)

// frontendTypes 是前端 src/components/notifications/index.js 里 NotificationFormList
// 的全部键名。数据库里 notification.type 存的就是这些字符串——少一个，
// 对应的渠道在前端就会"测试"时报"类型尚未实现"。
//
// 这个测试的意义：新增渠道时忘了注册、或者注册名大小写写错（AliyunSMS vs AliyunSms），
// 都会在这里立刻暴露，而不是等用户点测试才发现。
var frontendTypes = []string{
	"alerta", "AlertNow", "AliyunSMS", "apprise", "bale", "Bark", "bearsms", "Bitrix24",
	"ClickUp", "clicksendsms", "CallMeBot", "amootsms", "smsc", "smsir", "DingDing", "discord",
	"fluxer", "Elks", "egosms", "Feishu", "FreeMobile", "GoogleChat", "GoogleSheets", "gorush",
	"gotify", "GrafanaOncall", "HomeAssistant", "Indigo", "HeiiOnCall", "Keep", "Kook", "line",
	"lunasea", "matrix", "mattermost", "nextcloudtalk", "nostr", "notifyapp", "ntfy", "octopush",
	"OneChat", "OneBot", "Onesender", "Ooredoo", "Opsgenie", "OpsiMate", "JiraServiceManagement",
	"PagerDuty", "FlashDuty", "Flowtriq", "PagerTree", "pinglet", "plivo", "promosms", "pumble",
	"pushbullet", "PushByTechulus", "PushDeer", "pushover", "PushPlus", "pushy", "serwersms",
	"signal", "signalgrid", "SIGNL4", "SMSGateway", "SMSManager", "SMSPartner", "slack",
	"squadcast", "SMSEagle", "smtp", "MicrosoftGraph", "stackfield", "teams", "telegram",
	"Teltonika", "telnyx", "threema", "trigv", "twilio", "Splunk", "SpugPush", "webhook", "WeCom",
	"GoAlert", "ServerChan", "ZohoCliq", "SevenIO", "whapi", "evolution", "openwa", "notifery",
	"waha", "Whatsapp360messenger", "gtxmessaging", "Cellsynt", "WPush", "WxPusher", "SendGrid",
	"TurboSMTP", "Brevo", "Resend", "YZJ", "SMSPlanet", "Webpush", "HaloPSA", "max", "VK",
	"VKTeams", "Milky",
}

func TestAllFrontendTypesRegistered(t *testing.T) {
	var missing []string
	for _, typ := range frontendTypes {
		if _, ok := Get(typ); !ok {
			missing = append(missing, typ)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d 个前端渠道未注册: %v", len(missing), missing)
	}
}

func TestRegisteredProvidersHaveType(t *testing.T) {
	for _, typ := range Types() {
		p, ok := Get(typ)
		if !ok {
			t.Fatalf("%s: registered but not found", typ)
		}
		if p.Type() != typ {
			t.Fatalf("registry key %q != provider.Type() %q", typ, p.Type())
		}
	}
}
