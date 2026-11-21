package httpapi

import (
	"net/http"
)

// 周边小路由，对齐官方 server.js 里非 /api 前缀的挂载点。

// changePasswordWikiURL 官方 /.well-known/change-password 的跳转目标。
const changePasswordWikiURL = "https://github.com/louislam/uptime-kuma/wiki/Reset-Password-via-CLI"

// handleRobotsTxt 对齐官方 server.js:317：
//
//	User-agent: *
//	Disallow: [ / ]
//
// Search Engine Visibility（searchEngineIndex）未开启时全站禁抓；
// 官方无默认种子，未设置即视为关闭（前端开关同样默认 false）。
func (a *API) handleRobotsTxt(w http.ResponseWriter, r *http.Request) {
	txt := "User-agent: *\nDisallow:"
	if !a.Set.GetBool(r.Context(), "searchEngineIndex", false) {
		txt += " /"
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(txt))
}

// handleChangePasswordWellKnown 对齐官方 server.js:356：
// 浏览器密码管理器的「更改密码」入口，302 到官方 wiki 的重置密码页。
func (a *API) handleChangePasswordWellKnown(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, changePasswordWikiURL, http.StatusFound)
}
