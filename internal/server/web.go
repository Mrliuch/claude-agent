package server

import (
	"embed"
	"net/http"
)

//go:embed web/index.html
var webFS embed.FS

// handleUI 在 / 提供内置 Web 控制台。控制台本身不需要鉴权即可加载，
// 但所有特权操作（WebSocket 对话、文件管理）仍由用户在页面内填入的
// 共享 token 驱动——token 不写死在被下发的 HTML 里。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "web console not bundled", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
