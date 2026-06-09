package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"claude-agent/internal/bridge"
	"claude-agent/internal/config"
	"claude-agent/internal/protocol"
)

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
	cfg config.Config
}

func NewServer(cfg config.Config) *Server {
	return &Server{cfg: cfg}
}

// Routes 注册路由。
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/agent/chat", s.handleChat)
	mux.HandleFunc("/agent/fs/list", s.handleFsList)
	mux.HandleFunc("/agent/fs/read", s.handleFsRead)
	mux.HandleFunc("/agent/fs/write", s.handleFsWrite)
	mux.HandleFunc("/agent/fs/mkdir", s.handleFsMkdir)
	mux.HandleFunc("/agent/fs/delete", s.handleFsDelete)
	mux.HandleFunc("/agent/fs/tree", s.handleFsTree)
	mux.HandleFunc("/agent/fs/download", s.handleFsDownload)
	mux.HandleFunc("/agent/fs/upload", s.handleFsUpload)
	mux.HandleFunc("/agent/sessions/list", s.handleSessionsList)
	mux.HandleFunc("/agent/sessions/read", s.handleSessionRead)
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

// authed 校验共享 token（query 参数）。
func (s *Server) authed(r *http.Request) bool {
	return s.cfg.Token != "" && r.URL.Query().Get("token") == s.cfg.Token
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
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
