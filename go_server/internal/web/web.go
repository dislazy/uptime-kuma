// Package web 负责托管前端静态资源，并做 SPA 回退。
package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
)

// Handler 是一个带 SPA 回退的静态文件处理器。
type Handler struct {
	dir        string
	fileServer http.Handler
}

// New 构造。dir 为空或不存在时退化为"提示页"，不会导致进程起不来。
func New(dir string) *Handler {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		logx.Warnf("web", "前端目录不存在: %s（API 仍可用，但页面无法访问）", dir)
	}
	return &Handler{
		dir:        dir,
		fileServer: http.FileServer(http.Dir(dir)),
	}
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clean := filepath.Clean("/" + r.URL.Path)

	// 带扩展名的资源：命中就返回，不命中给 404，避免把 js/css 请求喂成 html
	if ext := filepath.Ext(clean); ext != "" && ext != ".html" {
		full := filepath.Join(h.dir, filepath.FromSlash(clean))
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			h.setCacheHeaders(w, clean)
			http.StripPrefix("", h.fileServer).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	// 其余路径一律交给 SPA 路由
	index := filepath.Join(h.dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(placeholderHTML))
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}

func (h *Handler) setCacheHeaders(w http.ResponseWriter, clean string) {
	// Vite 产物带内容哈希，可以长缓存；其它文件保持短缓存
	if strings.HasPrefix(clean, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
}

const placeholderHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>go_server</title>
<style>
  body{font-family:system-ui,sans-serif;background:#f6f7f9;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;color:#333}
  .box{background:#fff;padding:40px 48px;border-radius:12px;box-shadow:0 2px 16px rgba(0,0,0,.08);max-width:560px}
  code{background:#f0f1f3;padding:2px 6px;border-radius:4px}
  h1{margin-top:0;font-size:20px}
  p{line-height:1.7;font-size:14px;color:#555}
</style>
</head>
<body>
<div class="box">
  <h1>go_server 已启动</h1>
  <p>后端接口正常，但没有找到前端静态资源（<code>dist/</code>）。</p>
  <p>请先执行 <code>npm run build</code> 生成前端，然后用 <code>-web-dir</code>
     参数或 <code>UPTIME_KUMA_WEB_DIR</code> 环境变量指向该目录。</p>
  <p>健康检查：<code>/api/health</code></p>
</div>
</body>
</html>`
