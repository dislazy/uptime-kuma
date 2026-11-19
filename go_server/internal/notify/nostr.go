package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/nbd-wtf/go-nostr/nip44"
	"github.com/nbd-wtf/go-nostr/nip59"
)

// nostr（NIP-17 私信 + NIP-59 gift wrap）。
//
// 官方用 nostr-tools 的 nip59.wrapEvent：把一条 kind 14 的 rumor 先用
// 收发双方的会话密钥加密成 seal（kind 13），再用一次性的随机密钥加密成
// gift wrap（kind 1059）。这样中继只能看到"某个随机公钥发了一条给 P 的消息"，
// 看不到真正的发送者。
//
// 字段格式与官方一致：
//
//	sender     nsec1...（发送者私钥）
//	recipients 每行一个 npub1...
//	relays     每行一个 wss://...
//
// 返回值是 "2/3 relays connected."，与官方文案一致。

func init() { registerFunc("nostr", sendNostr) }

func sendNostr(ctx context.Context, cfg map[string]any, m Message) (string, error) {
	sender := strings.TrimSpace(cfgStr(cfg, "sender"))
	if sender == "" {
		return "", errors.New("Sender is required")
	}
	recipientsRaw := strings.TrimSpace(cfgStr(cfg, "recipients"))
	if recipientsRaw == "" {
		return "", errors.New("Recipients is required")
	}
	relaysRaw := strings.TrimSpace(cfgStr(cfg, "relays"))
	if relaysRaw == "" {
		return "", errors.New("Relays is required")
	}

	sk, err := nostrDecodePrivateKey(sender)
	if err != nil {
		return "", err
	}

	var recipients []string
	for _, line := range strings.Split(recipientsRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		prefix, value, err := nip19.Decode(line)
		if err != nil {
			return "", fmt.Errorf("Error decoding recipient %s: %v", line, err)
		}
		if prefix != "npub" {
			return "", fmt.Errorf("Recipient %s is not an npub", line)
		}
		pk, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("Recipient %s is not a valid npub", line)
		}
		recipients = append(recipients, pk)
	}
	if len(recipients) == 0 {
		return "", errors.New("Recipients is required")
	}

	var relays []string
	for _, line := range strings.Split(relaysRaw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			relays = append(relays, line)
		}
	}
	if len(relays) == 0 {
		return "", errors.New("Relays is required")
	}

	// 每个收件人一个 gift wrap
	events := make([]nostr.Event, 0, len(recipients))
	for _, pk := range recipients {
		ev, err := nostrGiftWrap(sk, pk, msgBody(m))
		if err != nil {
			return "", fmt.Errorf("Failed to create gift-wrapped event for recipient: %v", err)
		}
		events = append(events, ev)
	}

	successful := 0
	lastErr := ""
	for _, relayURL := range relays {
		relayCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		relay, err := nostr.RelayConnect(relayCtx, relayURL)
		if err != nil {
			lastErr = err.Error()
			cancel()
			continue
		}

		ok := true
		for _, ev := range events {
			if err := relay.Publish(relayCtx, ev); err != nil {
				ok = false
				lastErr = err.Error()
				break
			}
		}
		_ = relay.Close()
		cancel()
		if ok {
			successful++
		}
	}

	if successful == 0 {
		if lastErr != "" {
			return "", fmt.Errorf("Failed to connect to any relays: %s", lastErr)
		}
		return "", errors.New("Failed to connect to any relays.")
	}
	return fmt.Sprintf("%d/%d relays connected.", successful, len(relays)), nil
}

// nostrDecodePrivateKey 解析 nsec1...（也允许直接给 64 位 hex）。
func nostrDecodePrivateKey(sender string) (string, error) {
	if len(sender) == 64 && isHex(sender) {
		return sender, nil
	}
	prefix, value, err := nip19.Decode(sender)
	if err != nil {
		return "", fmt.Errorf("Failed to decode private key for sender %s: %v", sender, err)
	}
	if prefix != "nsec" {
		return "", fmt.Errorf("Sender %s is not an nsec", sender)
	}
	sk, ok := value.(string)
	if !ok || len(sk) != 64 {
		return "", errors.New("Invalid private key")
	}
	return sk, nil
}

// nostrGiftWrap 生成一条 kind 1059 的 gift wrap。
func nostrGiftWrap(privateKey, recipient, content string) (nostr.Event, error) {
	rumor := nostr.Event{
		Kind:      14, // NIP-17 private direct message
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"p", recipient}},
		Content:   content,
	}

	conversationKey, err := nip44.GenerateConversationKey(recipient, privateKey)
	if err != nil {
		return nostr.Event{}, err
	}

	encrypt := func(plaintext string) (string, error) {
		return nip44.Encrypt(plaintext, conversationKey)
	}
	sign := func(ev *nostr.Event) error {
		return ev.Sign(privateKey)
	}

	return nip59.GiftWrap(rumor, recipient, encrypt, sign, nil)
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
