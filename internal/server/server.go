package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"claude-agent/internal/bridge"
	"claude-agent/internal/config"
	"claude-agent/internal/protocol"
	"claude-agent/internal/wechat"
)

// authCookie 是页面认证用的 cookie 名(复用 AGENT_TOKEN)。
const authCookie = "agent_token"

const (
	pongWait   = 60 * time.Second
	pingPeriod = pongWait * 9 / 10
	writeWait  = 10 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Server 持有配置，提供 HTTP/WebSocket 路由。
type Server struct {
	cfg               config.Config
	wechat            *wechat.Manager // 可空:未启用微信通道时为 nil
	claudeInfoOnce    sync.Once
	claudeVersion     string
	projectSkillsOkay bool
}

func NewServer(cfg config.Config) *Server {
	installBundledSystemSkills()
	return &Server{cfg: cfg}
}

// Routes 注册路由。
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/agent/version", s.handleVersion)
	mux.HandleFunc("/agent/chat", s.handleChat)
	mux.HandleFunc("/agent/fs/list", s.handleFsList)
	mux.HandleFunc("/agent/fs/read", s.handleFsRead)
	mux.HandleFunc("/agent/fs/write", s.handleFsWrite)
	mux.HandleFunc("/agent/fs/mkdir", s.handleFsMkdir)
	mux.HandleFunc("/agent/fs/delete", s.handleFsDelete)
	mux.HandleFunc("/agent/fs/tree", s.handleFsTree)
	mux.HandleFunc("/agent/fs/download", s.handleFsDownload)
	mux.HandleFunc("/agent/fs/upload", s.handleFsUpload)
	mux.HandleFunc("/agent/git/run", s.handleGitRun)
	mux.HandleFunc("/agent/release/run", s.handleReleaseRun)
	mux.HandleFunc("/agent/ai/catalog", s.handleAICatalog)
	mux.HandleFunc("/agent/sessions/list", s.handleSessionsList)
	mux.HandleFunc("/agent/sessions/read", s.handleSessionRead)
	mux.HandleFunc("/agent/login", s.handleLogin)
	mux.HandleFunc("/agent/logout", s.handleLogout)
	mux.HandleFunc("/agent/wechat/accounts", s.handleWeChatAccounts)
	mux.HandleFunc("/agent/wechat/qr", s.handleWeChatQR)
	if s.cfg.UIEnabled {
		mux.HandleFunc("/", s.handleUI)
	}
	return mux
}

// resolveWorkSubdir 校验客户端选择的工作目录子文件夹：
// 返回围栏内、已存在目录的绝对路径；空串/越界/非目录一律返回 ""（调用方回退到根）。
func (s *Server) resolveWorkSubdir(sub string) string {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return ""
	}
	target, err := s.safeResolve(sub)
	if err != nil {
		log.Printf("[claude-agent] work_subdir 越界已忽略: %s (%v)", sub, err)
		return ""
	}
	if fi, e := os.Stat(target); e != nil || !fi.IsDir() {
		log.Printf("[claude-agent] work_subdir 无效(非目录或不存在): %s", sub)
		return ""
	}
	return target
}

// authed 校验共享 token：接受 query 参数或认证 cookie（页面登录后种下）。
func (s *Server) authed(r *http.Request) bool {
	if s.cfg.Token == "" {
		return false
	}
	if r.URL.Query().Get("token") == s.cfg.Token {
		return true
	}
	if c, err := r.Cookie(authCookie); err == nil && c.Value == s.cfg.Token {
		return true
	}
	return false
}

// handleLogin 校验 token 并种下认证 cookie（页面认证,复用 AGENT_TOKEN）。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "method not allowed", nil)
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" || token != s.cfg.Token {
		writeJSON(w, 401, "token 无效", nil)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 3600,
	})
	writeJSON(w, 0, "ok", nil)
}

// handleLogout 清除认证 cookie。
func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: authCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 0, "ok", nil)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	v := s.cfg.Version
	if v == "" {
		v = "unknown"
	}
	claudeVersion, skillsOkay := s.claudeRuntimeInfo()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": v, "claude_version": claudeVersion, "project_skills_supported": skillsOkay,
	})
}

// claudeRuntimeInfo 缓存探测 Claude Code 是否支持 --setting-sources。该参数用于
// 让非交互 stream-json 会话显式加载当前工作目录的项目 Skills；旧 CLI 不支持时
// 仍可被平台明确提示升级，而不是在对话中静默丢失 Skills。
func (s *Server) claudeRuntimeInfo() (string, bool) {
	s.claudeInfoOnce.Do(func() {
		s.claudeVersion = claudeCommandOutput(s.cfg.ClaudeBin, "--version")
		help := claudeCommandOutput(s.cfg.ClaudeBin, "--help")
		s.projectSkillsOkay = strings.Contains(help, "--setting-sources")
	})
	return s.claudeVersion, s.projectSkillsOkay
}

func claudeCommandOutput(bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// handleChat 升级为 WebSocket，鉴权后桥接到本机 claude。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if s.cfg.Token == "" || token != s.cfg.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	cfg := s.cfg
	cfg.SessionID = r.URL.Query().Get("session_id")

	// 按连接选择工作目录子文件夹：必须落在配置根目录的围栏内且为已存在目录，
	// 校验通过后把 claude 进程 cwd 覆盖为该子目录绝对路径（文件管理围栏不受影响）。
	if dir := s.resolveWorkSubdir(r.URL.Query().Get("work_subdir")); dir != "" {
		cfg.WorkDir = dir
	}

	// 按连接注入用户私有凭据（来自上游中继的握手 Header）：非空则经 --settings 临时文件
	// 覆盖该连接 claude 子进程的 ANTHROPIC_* 配置，实现每用户独立 token；为空则用共享默认。
	if v := strings.TrimSpace(r.Header.Get("X-Claude-Auth-Token")); v != "" {
		cfg.ClaudeAuthToken = v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Claude-Base-Url")); v != "" {
		cfg.ClaudeBaseURL = v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Claude-Model")); v != "" {
		cfg.Model = v
	}
	if raw := strings.TrimSpace(r.Header.Get("X-Claude-User-Mcp")); raw != "" {
		if data, err := base64.StdEncoding.DecodeString(raw); err == nil && len(data) <= 24*1024 {
			cfg.UserMCPConfig = string(data)
		}
	}
	if raw := strings.TrimSpace(r.Header.Get("X-Claude-Development-Prompt")); raw != "" {
		if data, err := base64.StdEncoding.DecodeString(raw); err == nil && len(data) <= 12*1024 {
			cfg.DevelopmentSystemPrompt = string(data)
		}
	}
	applyTaskAuditHeaders(&cfg, r.Header)
	// 企业微信发送与任务归档同样只按当前连接注入，避免共享 agent 用户继承其他人的权限。
	if v := strings.TrimSpace(r.Header.Get("X-Claude-Wecom-Send-Url")); len(v) <= 512 &&
		strings.HasPrefix(v, "https://") {
		cfg.WecomSendURL = v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Claude-Wecom-Send-Token")); len(v) <= 2048 &&
		strings.HasPrefix(v, "cswecom_") {
		cfg.WecomSendToken = v
	}

	b := bridge.NewBridge(cfg)
	if err := b.Start(); err != nil {
		_ = conn.WriteJSON(map[string]any{"type": "error", "msg": "启动 claude 失败: " + err.Error()})
		return
	}
	defer b.Close()

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				b.Close()
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			conn.SetReadDeadline(time.Now().Add(pongWait))
			handleClientMessage(b, data)
		}
	}()

	idle := time.Duration(s.cfg.IdleTimeoutSec) * time.Second
	go func() {
		pingT := time.NewTicker(pingPeriod)
		idleT := time.NewTicker(idleCheckInterval(idle))
		defer pingT.Stop()
		defer idleT.Stop()
		for {
			select {
			case <-pingT.C:
				if err := conn.WriteControl(websocket.PingMessage, nil,
					time.Now().Add(writeWait)); err != nil {
					return
				}
			case <-idleT.C:
				if idle <= 0 {
					continue
				}
				last := time.Unix(0, lastActivity.Load())
				if time.Since(last) >= idle {
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, "idle timeout"),
						time.Now().Add(writeWait))
					conn.Close()
					return
				}
			}
		}
	}()

	for ev := range b.Events() {
		conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteJSON(ev); err != nil {
			return
		}
		if ev["type"] == "closed" {
			return
		}
	}
}

// applyTaskAuditHeaders 只接受平台在当前 WebSocket 握手中签发的 csactor_ 操作者证明。
// cstask_ 是受控本机提交器使用的服务令牌，绝不能作为浏览器会话的操作者身份传入。
// 两项缺一不可，避免不完整或过期的握手信息污染本次连接的临时 settings。
func applyTaskAuditHeaders(cfg *config.Config, headers http.Header) {
	cfg.TaskAuditURL = ""
	cfg.TaskAuditToken = ""

	url := strings.TrimSpace(headers.Get("X-Claude-Task-Audit-Url"))
	token := strings.TrimSpace(headers.Get("X-Claude-Task-Audit-Token"))
	validURL := len(url) <= 512 && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://"))
	validToken := len(token) <= 512 && strings.HasPrefix(token, "csactor_")
	if validURL && validToken {
		cfg.TaskAuditURL = url
		cfg.TaskAuditToken = token
	}
}

// idleCheckInterval 计算空闲检查间隔。
func idleCheckInterval(idle time.Duration) time.Duration {
	if idle <= 0 {
		return pingPeriod
	}
	d := idle / 3
	if d < time.Second {
		d = time.Second
	}
	if d > pingPeriod {
		d = pingPeriod
	}
	return d
}

// handleClientMessage 处理一条客户端消息。
func handleClientMessage(b *bridge.Bridge, data []byte) {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	switch msg["type"] {
	case "user_message":
		if text := protocol.StrOr(msg["text"], ""); text != "" {
			_ = b.SendUserMessage(text)
		}
	case "permission_response":
		if reqID := protocol.StrOr(msg["request_id"], ""); reqID != "" {
			allow, _ := msg["allow"].(bool)
			_ = b.RespondPermission(reqID, allow, msg["tool_input"])
		}
	case "interrupt":
		_ = b.Interrupt()
	case "question_response":
		if reqID := protocol.StrOr(msg["request_id"], ""); reqID != "" {
			answers, _ := msg["answers"].(map[string]any)
			if answers == nil {
				answers = map[string]any{}
			}
			_ = b.RespondAskUserQuestion(reqID, answers)
		}
	case "close":
		b.Close()
	}
}

// Run 启动 HTTP 服务，阻塞直到出错。
func (s *Server) Run() error {
	log.Printf("[claude-agent] 监听 %s，claude=%s，work_dir=%s，web_ui=%v",
		s.cfg.ListenAddr, s.cfg.ClaudeBin, s.cfg.ResolvedWorkDir(), s.cfg.UIEnabled)
	return http.ListenAndServe(s.cfg.ListenAddr, s.Routes())
}
