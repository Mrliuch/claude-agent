package server

import (
	"embed"
	"net/http"
)

//go:embed web/index.html web/login.html
var webFS embed.FS

// handleUI 在 / 提供内置 Web 控制台。页面需认证(复用 AGENT_TOKEN):
// 未携带有效认证 cookie/token 时下发登录页,认证后才下发控制台 HTML。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := "web/index.html"
	if !s.authed(r) {
		page = "web/login.html" // 未认证只给登录页,控制台内容不下发
	}
	data, err := webFS.ReadFile(page)
	if err != nil {
		http.Error(w, "web console not bundled", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
