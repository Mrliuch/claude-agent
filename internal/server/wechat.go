package server

import (
	"net/http"
	"strings"

	"claude-agent/internal/wechat"
)

// SetWeChat 注入微信多账号管理器(为空则不启用相关路由功能)。
func (s *Server) SetWeChat(m *wechat.Manager) { s.wechat = m }

// handleWeChatAccounts 处理账号的增/删/查。
//
//	GET    列出账号
//	POST   新增账号(?name= 可选)→ 返回 id,随后前端轮询 /qr 与 /accounts 状态
//	DELETE 移除账号(?id=)
func (s *Server) handleWeChatAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	if s.wechat == nil {
		writeJSON(w, 400, "微信通道未启用", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 0, "ok", map[string]any{"accounts": s.wechat.List()})
	case http.MethodPost:
		id := s.wechat.Add(strings.TrimSpace(r.URL.Query().Get("name")))
		writeJSON(w, 0, "ok", map[string]any{"id": id})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, 400, "缺少 id", nil)
			return
		}
		s.wechat.Remove(id)
		writeJSON(w, 0, "ok", nil)
	default:
		writeJSON(w, 405, "method not allowed", nil)
	}
}

// handleWeChatQR 返回某账号当前登录二维码 PNG。
func (s *Server) handleWeChatQR(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.wechat == nil {
		http.Error(w, "wechat disabled", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	content, ok := s.wechat.QR(id)
	if !ok {
		http.Error(w, "no qr (online or unknown account)", http.StatusNotFound)
		return
	}
	png, err := wechat.QRPNG(content)
	if err != nil {
		http.Error(w, "qr encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}
