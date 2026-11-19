package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/songbo/uptime-kuma/go_server/internal/auth"
	"github.com/songbo/uptime-kuma/go_server/internal/logx"
)

// 本文件复刻 better-auth 的 HTTP 接口面。
//
// 前端用的是 better-auth 客户端（src/auth-client.ts），它只会打这几个端点。
// 只要请求/响应形状与 Cookie 行为对上，前端完全感知不到后端换了语言。

type signInRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	RememberMe bool   `json:"rememberMe"`
}

// handleSignIn 对应 POST /api/auth/sign-in/username
func (a *API) handleSignIn(w http.ResponseWriter, r *http.Request) {
	// 官方开启了 60 秒 10 次的限流，这里做同等保护
	if !loginLimiter.allow(a.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "Too many requests. Please try again later.")
		return
	}

	var req signInRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, cookieValue, err := a.Auth.Login(r.Context(), req.Username, req.Password, a.clientIP(r), r.UserAgent(), req.RememberMe)
	if err != nil {
		switch err {
		case auth.ErrTwoFactorRequired:
			// 前端看到这个字段会弹 2FA 对话框。此时还不能建正式会话，
			// 只下发一个 5 分钟的 pending cookie 作为"密码已验证"的凭证。
			if user != nil {
				a.setTwoFAPending(w, user.ID)
			}
			writeJSON(w, http.StatusOK, map[string]any{"twoFactorRedirect": true})
			return
		default:
			logx.Debugf("http", "登录失败 %s: %v", req.Username, err)
			writeError(w, http.StatusUnauthorized, "Invalid username or password")
			return
		}
	}

	a.Auth.SetCookie(w, cookieValue, req.RememberMe, a.Cfg.IsTLS())

	writeJSON(w, http.StatusOK, map[string]any{
		"redirect": false,
		"token":    cookieValue,
		"user": map[string]any{
			"id":               user.ID,
			"username":         user.Username,
			"name":             user.Name,
			"email":            "",
			"emailVerified":    true,
			"twoFactorEnabled": user.TwoFactorEnabled,
		},
	})
}

// handleSignUpDisabled 官方禁用了注册，这里保持一致。
func (a *API) handleSignUpDisabled(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusForbidden, "Sign up is disabled")
}

// handleGetSession 对应 GET /api/auth/get-session
func (a *API) handleGetSession(w http.ResponseWriter, r *http.Request) {
	user, err := a.Auth.UserFromRequest(r.Context(), r)
	if err != nil || user == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session": map[string]any{
			"id":        "",
			"userId":    user.ID,
			"expiresAt": "",
		},
		"user": map[string]any{
			"id":               user.ID,
			"username":         user.Username,
			"name":             user.Name,
			"email":            "",
			"emailVerified":    true,
			"twoFactorEnabled": user.TwoFactorEnabled,
		},
	})
}

// handleSignOut 对应 POST /api/auth/sign-out
func (a *API) handleSignOut(w http.ResponseWriter, r *http.Request) {
	raw := auth.CookieFromRequest(r)
	if raw != "" {
		_ = a.Auth.Logout(r.Context(), raw)
	}
	a.Auth.ClearCookie(w, a.Cfg.IsTLS())
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleOK 对应 GET /api/auth/ok（better-auth 的健康检查）
func (a *API) handleOK(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthFallback 未实现的 better-auth 端点（2FA、API Key 等）统一返回 501，
// 前端会把它当普通错误处理，不会白屏。
func (a *API) handleAuthFallback(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/auth/")
	writeError(w, http.StatusNotImplemented, "Endpoint not implemented yet: "+p)
}

// handleSetup 对应 POST /api/setup
func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	has, err := a.Auth.HasUser(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if has {
		writeError(w, http.StatusBadRequest,
			"Uptime Kuma has been initialized. If you want to run setup again, please delete the database.")
		return
	}

	var req signInRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	user, err := a.Auth.CreateUser(r.Context(), req.Username, req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 初始化完成直接给一个会话，省得前端再登录一次
	_, cookieValue, err := a.Auth.Login(r.Context(), req.Username, req.Password, a.clientIP(r), r.UserAgent(), true)
	if err == nil {
		a.Auth.SetCookie(w, cookieValue, true, a.Cfg.IsTLS())
	}

	logx.Infof("http", "初始化完成，管理员账号: %s", user.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleEntryPage 对应 GET /api/entry-page
func (a *API) handleEntryPage(w http.ResponseWriter, r *http.Request) {
	entry := a.Set.GetString(r.Context(), "entryPage", "statusPage-default")
	writeJSON(w, http.StatusOK, map[string]any{
		"type":      "entryPage",
		"entryPage": entry,
	})
}
