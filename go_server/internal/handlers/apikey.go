package handlers

// API Key 管理的三个 socket 事件，对齐官方 server/socket-handlers/api-key-socket-handler.js：
//
//	addAPIKey(key, cb)      → {ok, msg:"successAdded", msgi18n:true, key: 明文, keyID}
//	getAPIKeyList(cb)        → {ok, apiKeyList:[better-auth keys + legacy keys]}
//	deleteAPIKey(keyID, cb) → {ok, msg:"successDeleted", msgi18n:true}
//
// 细节对齐：
//   - 新 key 走 better-auth 存储（明文只回显一次，库里只存 SHA-256/base64url 哈希）；
//   - 创建成功后把 apiKeysEnabled 设置打开（官方同款："用户建了 key 就说明要用"）；
//   - legacy 表（2.x 的 api_key）只读删不写，id 加 "legacy-" 前缀防撞；
//   - 前端传来的 expires 是日期串，已过期/无效按"永不过期"处理（官方同款）。

import (
	"errors"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// apiKeyPayload 是前端 APIKeyDialog 提交的字段。
type apiKeyPayload struct {
	Name    string  `json:"name"`
	Expires *string `json:"expires"`
	Active  int     `json:"active"`
}

// addAPIKey 创建一把新 key。
func addAPIKey(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var key apiKeyPayload
	if err := c.Arg(0, &key); err != nil {
		return nil, errors.New("参数格式错误")
	}
	if strings.TrimSpace(key.Name) == "" {
		return nil, errors.New("名称不能为空")
	}

	// 官方逻辑：expires 解析失败或已在过去 → 不设过期
	var expiresAt *time.Time
	if key.Expires != nil && *key.Expires != "" {
		for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05", store.TimeLayout} {
			if t, err := time.ParseInLocation(layout, strings.TrimSpace(*key.Expires), time.Local); err == nil {
				if t.After(time.Now()) {
					expiresAt = &t
				}
				break
			}
		}
	}

	id, clear, err := c.Auth.CreateAPIKey(c.Ctx, c.RequireUserID(), key.Name, expiresAt)
	if err != nil {
		return nil, err
	}

	// 官方：用户创建 key 后自动启用 API key 鉴权
	if err := c.Set.Set(c.Ctx, "apiKeysEnabled", true, "general"); err != nil {
		// 设置失败不影响 key 本身，但要让日志知道
		return nil, errors.New("创建成功但写入设置失败: " + err.Error())
	}

	return map[string]any{
		"ok":      true,
		"msg":     "successAdded",
		"msgi18n": true,
		"key":     clear,
		"keyID":   id,
	}, nil
}

// getAPIKeyList 返回 better-auth keys + legacy keys。
func getAPIKeyList(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	keys, err := c.Auth.ListAPIKeys(c.Ctx)
	if err != nil {
		return nil, err
	}

	// legacy 表（2.x）：id 加前缀防与 better-auth 的字符串 id 相撞
	legacyRows, err := c.DB.Query(c.Ctx,
		"SELECT id, name, active, expires, created_date FROM api_key ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	for _, r := range legacyRows {
		keys = append(keys, map[string]any{
			"id":        "legacy-" + r.Str("id"),
			"name":      r.Str("name"),
			"enabled":   r.Int("active") != 0,
			"expiresAt": nullableStr(r.Str("expires")),
			"createdAt": nullableStr(r.Str("created_date")),
		})
	}

	return map[string]any{"ok": true, "apiKeyList": keys}, nil
}

// deleteAPIKey 删除一把 key（兼容 legacy- 前缀）。
func deleteAPIKey(c *Ctx) (any, error) {
	if err := c.RequireLogin(); err != nil {
		return nil, err
	}

	var keyID string
	if err := c.Arg(0, &keyID); err != nil {
		return nil, errors.New("参数格式错误")
	}

	if rest, ok := strings.CutPrefix(keyID, "legacy-"); ok {
		if _, err := c.DB.Exec(c.Ctx, "DELETE FROM api_key WHERE id = ?", rest); err != nil {
			return nil, err
		}
	} else {
		if err := c.Auth.DeleteAPIKey(c.Ctx, keyID); err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"ok":      true,
		"msg":     "successDeleted",
		"msgi18n": true,
	}, nil
}

// nullableStr 空串转 nil（前端按 null 显示 "Never"）。
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
