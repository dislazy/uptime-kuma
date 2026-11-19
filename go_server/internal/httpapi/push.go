package httpapi

import (
	"net/http"
	"strconv"
	"strings"
)

// handlePush 处理被动推送监控。
//
// 用法：GET/POST /api/push/<token>?status=up&msg=OK&ping=12
func (a *API) handlePush(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	q := r.URL.Query()
	msg := strings.TrimSpace(q.Get("msg"))
	status := strings.TrimSpace(q.Get("status"))

	var ping *int64
	if raw := q.Get("ping"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil {
			p := int64(v)
			ping = &p
		}
	}

	if err := a.Mgr.HandlePush(r.Context(), token, status, msg, ping); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// 官方返回 "OK" 纯文本，便于 curl / 探针脚本判断
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OK"))
}
