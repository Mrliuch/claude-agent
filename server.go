package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pongWait   = 60 * time.Second  // 超过此时长未收到 pong 判定断开
	pingPeriod = pongWait * 9 / 10 // 心跳发送周期（< pongWait）
	writeWait  = 10 * time.Second  // 单次写超时
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// 仅内网服务端调用，跨域校验交给 token，放开 Origin
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server 持有配置，提供 HTTP/WebSocket 路由。
type Server struct {
	cfg Config
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Routes 注册路由。
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/agent/chat", s.handleChat)
	// 文件管理（围栏在 work_dir 子树内）
	mux.HandleFunc("/agent/fs/list", s.handleFsList)
	mux.HandleFunc("/agent/fs/read", s.handleFsRead)
	mux.HandleFunc("/agent/fs/write", s.handleFsWrite)
	mux.HandleFunc("/agent/fs/mkdir", s.handleFsMkdir)
	mux.HandleFunc("/agent/fs/delete", s.handleFsDelete)
	mux.HandleFunc("/agent/fs/tree", s.handleFsTree)
	mux.HandleFunc("/agent/fs/download", s.handleFsDownload)
	mux.HandleFunc("/agent/fs/upload", s.handleFsUpload)
	// 内置 Web 控制台（同源，免 CORS）；AGENT_UI=off 可关闭
	if s.cfg.UIEnabled {
		mux.HandleFunc("/", s.handleUI)
	}
	return mux
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
	// 共享 token 鉴权（WebSocket 不便带 header，沿用 query 约定）
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

	// 按连接注入 session_id（用于 --resume 续接上下文）
	cfg := s.cfg
	cfg.SessionID = r.URL.Query().Get("session_id")

	bridge := NewBridge(cfg)
	if err := bridge.Start(); err != nil {
		_ = conn.WriteJSON(map[string]any{"type": "error", "msg": "启动 claude 失败: " + err.Error()})
		return
	}
	defer bridge.Close()

	// 最近一次“用户消息”时间（用于空闲超时；pong 不计入活跃）
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	// 心跳：读超时 + pong 续期；任何 pong 都把读 deadline 续上
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// reader：WebSocket → claude（独占读端）
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				bridge.Close()
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			conn.SetReadDeadline(time.Now().Add(pongWait))
			handleClientMessage(bridge, data)
		}
	}()

	// 心跳 + 空闲超时守护：定期 ping；空闲超限则主动关闭（回收 claude）
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
					conn.Close() // 触发 reader 报错 → bridge.Close 回收 claude
					return
				}
			}
		}
	}()

	// writer：claude 事件 → WebSocket（独占写端，gorilla 要求单写者；
	// ping/close 走 WriteControl，可与 WriteMessage 并发）
	for ev := range bridge.Events() {
		conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteJSON(ev); err != nil {
			return
		}
		if ev["type"] == "closed" {
			return
		}
	}
}

// idleCheckInterval 计算空闲检查间隔：禁用时退化为长周期，
// 否则取 idle/3 并夹在 [1s, pingPeriod] 之间（便于小超时场景及时回收/测试）。
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
func handleClientMessage(bridge *Bridge, data []byte) {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	switch msg["type"] {
	case "user_message":
		if text := strOr(msg["text"], ""); text != "" {
			_ = bridge.SendUserMessage(text)
		}
	case "permission_response":
		if reqID := strOr(msg["request_id"], ""); reqID != "" {
			allow, _ := msg["allow"].(bool)
			_ = bridge.RespondPermission(reqID, allow, msg["tool_input"])
		}
	case "close":
		bridge.Close()
	}
}

// Run 启动 HTTP 服务，阻塞直到出错。
func (s *Server) Run() error {
	log.Printf("[claude-agent] 监听 %s，claude=%s，work_dir=%s，web_ui=%v",
		s.cfg.ListenAddr, s.cfg.ClaudeBin, s.cfg.resolvedWorkDir(), s.cfg.UIEnabled)
	return http.ListenAndServe(s.cfg.ListenAddr, s.Routes())
}
