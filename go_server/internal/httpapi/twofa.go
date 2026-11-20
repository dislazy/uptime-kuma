package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/auth"
)

// better-auth twoFactor 插件的 HTTP 端点。
//
// 前端只用四个：
//
//	POST /api/auth/two-factor/enable                 {password, issuer} → {totpURI, backupCodes}
//	POST /api/auth/two-factor/verify-totp            {code}            → 绑定确认 / 登录放行
//	POST /api/auth/two-factor/disable                {password}
//	POST /api/auth/two-factor/generate-backup-codes
//
// 另外补一个 verify-backup-code：官方用备用码登录走的是这个路径，
// 前端的"使用备用码"入口会调它。

const twoFAPendingCookie = "kuma-2fa-pending"

type codeRequest struct {
	Code     string `json:"code"`
	Password string `json:"password"`
	Issuer   string `json:"issuer"`
	Remember bool   `json:"rememberMe"`
}

// handleTwoFAEnable 生成密钥并返回 otpauth URI。
func (a *API) handleTwoFAEnable(w http.ResponseWriter, r *http.Request) {
	user := a.currentUser(w, r)
	if user == nil {
		return
	}

	var req codeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// 官方要求重新输入密码，防止会话被劫持后直接改 2FA
	if err := a.Auth.CheckPassword(r.Context(), user.ID, req.Password); err != nil {
		writeError(w, http.StatusUnauthorized, "Invalid password")
		return
	}

	uri, codes, err := a.Auth.BeginTwoFA(r.Context(), user.ID, user.Username, req.Issuer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totpURI":     uri,
		"backupCodes": codes,
	})
}

// handleTwoFAVerifyTOTP 同时处理"绑定确认"和"登录第二步"。
func (a *API) handleTwoFAVerifyTOTP(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Code = strings.TrimSpace(req.Code)

	// 1) 有会话：这是绑定确认
	if user := a.currentUserNoWrite(w, r); user != nil {
		if err := a.Auth.ConfirmTwoFA(r.Context(), user.ID, req.Code); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	// 2) 无会话但有 pending cookie：这是登录第二步
	userID, ok := a.pendingTwoFAUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Two-factor session expired. Please sign in again.")
		return
	}
	if err := a.Auth.VerifyTwoFA(r.Context(), userID, req.Code); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	a.completeTwoFALogin(w, r, userID, req.Remember)
}

// handleTwoFAVerifyBackupCode 用备用码登录。
func (a *API) handleTwoFAVerifyBackupCode(w http.ResponseWriter, r *http.Request) {
	a.handleTwoFAVerifyTOTP(w, r)
}

// handleTwoFADisable 关闭 2FA（需要密码）。
func (a *API) handleTwoFADisable(w http.ResponseWriter, r *http.Request) {
	user := a.currentUser(w, r)
	if user == nil {
		return
	}

	var req codeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := a.Auth.CheckPassword(r.Context(), user.ID, req.Password); err != nil {
		writeError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	if err := a.Auth.DisableTwoFA(r.Context(), user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": true})
}

// handleTwoFAGenerateBackupCodes 重新生成备用码。
func (a *API) handleTwoFAGenerateBackupCodes(w http.ResponseWriter, r *http.Request) {
	user := a.currentUser(w, r)
	if user == nil {
		return
	}
	codes, err := a.Auth.RegenerateBackupCodes(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backupCodes": codes})
}

// ---------------- 辅助 ----------------

// currentUser 取当前登录用户，未登录时写 401 并返回 nil。
func (a *API) currentUser(w http.ResponseWriter, r *http.Request) *auth.User {
	user, err := a.Auth.UserFromRequest(r.Context(), r)
	if err != nil || user == nil {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return nil
	}
	return user
}

// currentUserNoWrite 只查询，不写响应。
func (a *API) currentUserNoWrite(w http.ResponseWriter, r *http.Request) *auth.User {
	user, err := a.Auth.UserFromRequest(r.Context(), r)
	if err != nil {
		return nil
	}
	return user
}

// setTwoFAPending 下发短期 pending cookie。
func (a *API) setTwoFAPending(w http.ResponseWriter, userID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     twoFAPendingCookie,
		Value:    a.Auth.SignTwoFAPending(userID),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.Cfg.IsTLS(),
		MaxAge:   int(auth.TwoFAPendingTTL / time.Second),
	})
}

func (a *API) clearTwoFAPending(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     twoFAPendingCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

func (a *API) pendingTwoFAUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(twoFAPendingCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	return a.Auth.ParseTwoFAPending(c.Value)
}

// completeTwoFALogin 验证码通过后补发正式会话。
func (a *API) completeTwoFALogin(w http.ResponseWriter, r *http.Request, userID string, remember bool) {
	user, err := a.Auth.UserByID(r.Context(), userID)
	if err != nil || user == nil {
		writeError(w, http.StatusUnauthorized, "Invalid user")
		return
	}

	_, cookieValue, err := a.Auth.IssueSession(r.Context(), user, a.clientIP(r), r.UserAgent(), remember)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.Auth.SetCookie(w, cookieValue, remember, a.Cfg.IsTLS())
	a.clearTwoFAPending(w)

	writeJSON(w, http.StatusOK, map[string]any{
		"redirect": false,
		"token":    cookieValue,
		"user": map[string]any{
			"id":               user.ID,
			"username":         user.Username,
			"name":             user.Name,
			"email":            "",
			"emailVerified":    true,
			"twoFactorEnabled": true,
		},
	})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}
