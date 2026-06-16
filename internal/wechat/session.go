package wechat

import (
	"context"
	"log"
	"sync"
	"time"

	"claude-agent/internal/bridge"
	"claude-agent/internal/config"
	"claude-agent/internal/protocol"
)

// claudeBridge 是 sessionManager 依赖的 claude 驱动接口(便于测试注入)。
// 生产实现为 *bridge.Bridge。
type claudeBridge interface {
	Events() <-chan map[string]any
	SendUserMessage(text string) error
	RespondPermission(requestID string, allow bool, updatedInput any) error
	RespondAskUserQuestion(requestID string, answers map[string]any) error
	Close()
}

// bridgeFactory 创建并启动一个 claude 桥接。
type bridgeFactory func(cfg config.Config) (claudeBridge, error)

// defaultBridgeFactory 用真实 bridge.Bridge 实现,创建后立即 Start。
func defaultBridgeFactory(cfg config.Config) (claudeBridge, error) {
	br := bridge.NewBridge(cfg)
	if err := br.Start(); err != nil {
		return nil, err
	}
	return br, nil
}

// pendingKind 标记某用户会话当前是否在等待一个交互式回复。
type pendingKind int

const (
	pendingNone pendingKind = iota
	pendingPermission
	pendingQuestion
)

// sendTimeout 单条 sendmessage 的超时。
const sendTimeout = 15 * time.Second

// userSession 对应一个微信用户的一条 claude 会话(= 一个 claude 子进程)。
type userSession struct {
	userID string
	br     claudeBridge

	mu           sync.Mutex
	lastActive   time.Time
	contextToken string
	pKind        pendingKind
	pReqID       string
	pQuestions   []any
}

// sessionManager 管理 from_user_id → userSession 映射,含并发上限与空闲回收。
type sessionManager struct {
	cfg     config.Config
	client  *Client
	ctx     context.Context
	maxSess int
	idle    time.Duration

	newBridge bridgeFactory

	mu     sync.Mutex
	byUser map[string]*userSession
}

func newSessionManager(ctx context.Context, cfg config.Config, client *Client, maxSess int) *sessionManager {
	return &sessionManager{
		cfg:       cfg,
		client:    client,
		ctx:       ctx,
		maxSess:   maxSess,
		idle:      time.Duration(cfg.IdleTimeoutSec) * time.Second,
		newBridge: defaultBridgeFactory,
		byUser:    make(map[string]*userSession),
	}
}

// Dispatch 处理一条收到的微信消息。
func (sm *sessionManager) Dispatch(msg Message) {
	text := msg.Text()
	if text == "" {
		return
	}
	us, created := sm.getOrCreate(msg.FromUserID)
	if us == nil {
		sm.send(msg.FromUserID, msg.ContextToken, "⚠️ 当前会话数已达上限，请稍后再试。")
		return
	}

	us.mu.Lock()
	us.contextToken = msg.ContextToken
	us.lastActive = time.Now()
	kind := us.pKind
	us.mu.Unlock()

	if created {
		// 新会话:首条消息直接作为用户输入发给 claude。
		_ = us.br.SendUserMessage(text)
		return
	}

	switch kind {
	case pendingPermission:
		sm.handlePermissionReply(us, text)
	case pendingQuestion:
		sm.handleQuestionReply(us, text)
	default:
		_ = us.br.SendUserMessage(text)
	}
}

func (sm *sessionManager) handlePermissionReply(us *userSession, text string) {
	allow, ok := parsePermissionReply(text)
	if !ok {
		sm.send(us.userID, us.token(), "未识别，请回复 y / 允许 或 n / 拒绝。")
		return
	}
	us.mu.Lock()
	reqID := us.pReqID
	us.pKind = pendingNone
	us.pReqID = ""
	us.mu.Unlock()
	_ = us.br.RespondPermission(reqID, allow, nil)
}

func (sm *sessionManager) handleQuestionReply(us *userSession, text string) {
	us.mu.Lock()
	questions := us.pQuestions
	reqID := us.pReqID
	us.mu.Unlock()

	answers, ok := parseQuestionReply(text, questions)
	if !ok {
		sm.send(us.userID, us.token(), "未识别选项，请按提示回复对应编号。")
		return
	}
	us.mu.Lock()
	us.pKind = pendingNone
	us.pReqID = ""
	us.pQuestions = nil
	us.mu.Unlock()
	_ = us.br.RespondAskUserQuestion(reqID, answers)
}

// getOrCreate 返回用户会话;created 表示本次新建。容量已满返回 (nil,false)。
func (sm *sessionManager) getOrCreate(userID string) (us *userSession, created bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if existing, ok := sm.byUser[userID]; ok {
		return existing, false
	}
	if len(sm.byUser) >= sm.maxSess {
		return nil, false
	}
	br, err := sm.newBridge(sm.cfg)
	if err != nil {
		log.Printf("[wechat] 启动 claude 失败 user=%s: %v", userID, err)
		return nil, false
	}
	us = &userSession{userID: userID, br: br, lastActive: time.Now()}
	sm.byUser[userID] = us
	go sm.consume(us)
	return us, true
}

// consume 消费 claude 事件,翻译为微信文本并回发;权限/问卷转入待确认状态。
func (sm *sessionManager) consume(us *userSession) {
	defer sm.remove(us.userID)
	for ev := range us.br.Events() {
		switch ev["type"] {
		case "permission_request":
			us.mu.Lock()
			us.pKind = pendingPermission
			us.pReqID = protocol.StrOr(ev["request_id"], "")
			us.mu.Unlock()
			sm.send(us.userID, us.token(), renderPermissionPrompt(ev))
		case "user_question":
			questions, _ := ev["questions"].([]any)
			us.mu.Lock()
			us.pKind = pendingQuestion
			us.pReqID = protocol.StrOr(ev["request_id"], "")
			us.pQuestions = questions
			us.mu.Unlock()
			sm.send(us.userID, us.token(), renderQuestionPrompt(questions))
		default:
			if text := renderEvent(ev); text != "" {
				sm.send(us.userID, us.token(), text)
			}
		}
	}
}

// token 返回该用户最近一次消息的 context_token。
func (us *userSession) token() string {
	us.mu.Lock()
	defer us.mu.Unlock()
	return us.contextToken
}

// send 发送一条文本回复(best-effort,失败仅记日志)。
func (sm *sessionManager) send(userID, contextToken, text string) {
	if text == "" {
		return
	}
	ctx, cancel := context.WithTimeout(sm.ctx, sendTimeout)
	defer cancel()
	if err := sm.client.SendMessage(ctx, userID, contextToken, text); err != nil {
		log.Printf("[wechat] 发送失败 user=%s: %v", userID, err)
	}
}

// remove 关闭并移除一个用户会话。
func (sm *sessionManager) remove(userID string) {
	sm.mu.Lock()
	us, ok := sm.byUser[userID]
	if ok {
		delete(sm.byUser, userID)
	}
	sm.mu.Unlock()
	if ok && us.br != nil {
		us.br.Close()
	}
}

// reapLoop 周期性回收空闲会话,直到 ctx 取消。
func (sm *sessionManager) reapLoop() {
	if sm.idle <= 0 {
		return
	}
	t := time.NewTicker(sm.idle / 3)
	defer t.Stop()
	for {
		select {
		case <-sm.ctx.Done():
			return
		case <-t.C:
			sm.reapIdle()
		}
	}
}

func (sm *sessionManager) reapIdle() {
	now := time.Now()
	var stale []string
	sm.mu.Lock()
	for id, us := range sm.byUser {
		us.mu.Lock()
		idleFor := now.Sub(us.lastActive)
		us.mu.Unlock()
		if idleFor >= sm.idle {
			stale = append(stale, id)
		}
	}
	sm.mu.Unlock()
	for _, id := range stale {
		log.Printf("[wechat] 回收空闲会话 user=%s", id)
		sm.remove(id)
	}
}

// closeAll 关闭全部会话(进程退出时调用)。
func (sm *sessionManager) closeAll() {
	sm.mu.Lock()
	sessions := make([]*userSession, 0, len(sm.byUser))
	for _, us := range sm.byUser {
		sessions = append(sessions, us)
	}
	sm.byUser = make(map[string]*userSession)
	sm.mu.Unlock()
	for _, us := range sessions {
		us.br.Close()
	}
}
