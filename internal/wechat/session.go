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

// typingInterval 处理中续"正在输入"的间隔(略短于微信端 ~10s 的 typing 失效,保持回复窗口打开)。
const typingInterval = 6 * time.Second

// permPending 是一条待用户确认的权限请求(FIFO 排队)。
// claude 一轮内可能连发多个 permission_request,必须逐个确认,不能只记最后一个。
type permPending struct {
	reqID  string
	prompt string // 渲染好的确认提示文本,轮到时再推送给用户
}

// userSession 对应一个微信用户的一条 claude 会话(= 一个 claude 子进程)。
type userSession struct {
	userID string
	br     claudeBridge

	mu           sync.Mutex
	lastActive   time.Time
	contextToken string
	pKind        pendingKind
	pReqID       string // 问卷(pendingQuestion)的 request_id;权限走 permQueue
	pQuestions   []any
	permQueue    []permPending // 待确认权限请求队列(队首为当前正在询问用户的一条)
	ticket       string        // typing_ticket(getconfig 缓存)
	busy         bool          // claude 正在处理本轮,期间需 typing 保活
	quit         chan struct{} // 会话结束信号,停掉 keepalive

	sessionID  string // claude 会话 id(收到 ready 帧后记录,用于跨进程 --resume 续接)
	resuming   bool   // 本次 claude 进程以 --resume 启动(用于检测 resume 失败)
	gotReady   bool   // 本次进程已收到过 ready(收到后 resume 视为成功)
	firstText  string // resume 模式下暂存的首条用户消息(resume 失败时自动补发到新会话)
	firstToken string // 首条消息的 context_token(resume 失败后回发提示/补发需要)
}

// sessionManager 管理 from_user_id → userSession 映射,含并发上限与空闲回收。
type sessionManager struct {
	cfg        config.Config
	client     *Client
	ctx        context.Context
	maxSess    int
	idle       time.Duration
	sessionDir string // claude session_id 持久化目录(空=不持久化,仅内存)

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
	// 重置口令:清掉持久化上下文并关闭当前会话进程,下条消息即全新对话。
	if isResetCommand(text) {
		sm.resetSession(msg.FromUserID)
		sm.send(msg.FromUserID, msg.ContextToken, "✅ 已清空上下文，下一条消息将开启全新对话。")
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
		// resume 模式下暂存该消息,一旦 resume 失败(收到 ready 前进程即退出)可补发到新会话。
		us.mu.Lock()
		if us.resuming {
			us.firstText = text
			us.firstToken = msg.ContextToken
		}
		us.mu.Unlock()
		sm.markBusy(us)
		_ = us.br.SendUserMessage(text)
		return
	}

	switch kind {
	case pendingPermission:
		sm.handlePermissionReply(us, text)
	case pendingQuestion:
		sm.handleQuestionReply(us, text)
	default:
		sm.markBusy(us)
		_ = us.br.SendUserMessage(text)
	}
}

// markBusy 标记会话进入处理中,并立即发一次 typing 保活(随后由 keepalive 维持)。
func (sm *sessionManager) markBusy(us *userSession) {
	us.mu.Lock()
	us.busy = true
	us.mu.Unlock()
	go sm.poke(us)
}

// poke 确保拿到 typing_ticket 并发一次 sendtyping(status:1)。
func (sm *sessionManager) poke(us *userSession) {
	ticket := sm.ensureTicket(us)
	if ticket == "" {
		return
	}
	ctx, cancel := context.WithTimeout(sm.ctx, sendTimeout)
	defer cancel()
	_ = sm.client.SendTyping(ctx, us.userID, ticket, 1)
}

// ensureTicket 返回缓存的 typing_ticket,无则通过 getconfig 获取并缓存。
func (sm *sessionManager) ensureTicket(us *userSession) string {
	us.mu.Lock()
	t := us.ticket
	us.mu.Unlock()
	if t != "" {
		return t
	}
	ctx, cancel := context.WithTimeout(sm.ctx, sendTimeout)
	defer cancel()
	t, err := sm.client.GetConfig(ctx, us.userID)
	if err != nil {
		log.Printf("[wechat] getconfig 失败 user=%s: %v", us.userID, err)
		return ""
	}
	us.mu.Lock()
	us.ticket = t
	us.mu.Unlock()
	return t
}

// typingKeepalive 在会话处理中每隔 typingInterval 续一次"正在输入",保持回复窗口打开。
func (sm *sessionManager) typingKeepalive(us *userSession) {
	t := time.NewTicker(typingInterval)
	defer t.Stop()
	for {
		select {
		case <-us.quit:
			return
		case <-sm.ctx.Done():
			return
		case <-t.C:
			us.mu.Lock()
			busy := us.busy
			ticket := us.ticket
			us.mu.Unlock()
			if busy && ticket != "" {
				ctx, cancel := context.WithTimeout(sm.ctx, sendTimeout)
				_ = sm.client.SendTyping(ctx, us.userID, ticket, 1)
				cancel()
			}
		}
	}
}

func (sm *sessionManager) handlePermissionReply(us *userSession, text string) {
	allow, ok := parsePermissionReply(text)
	if !ok {
		sm.send(us.userID, us.token(), "未识别，请回复 y / 允许 或 n / 拒绝。")
		return
	}
	// 应答队首(用户当前正在确认的那一条),出队;若队列还有下一个则继续询问用户,
	// 否则清空待确认状态、交回 claude 继续本轮。
	us.mu.Lock()
	if len(us.permQueue) == 0 { // 理论不达:pendingPermission 必有队首
		us.pKind = pendingNone
		us.mu.Unlock()
		return
	}
	head := us.permQueue[0]
	us.permQueue = us.permQueue[1:]
	var next *permPending
	if len(us.permQueue) > 0 {
		next = &us.permQueue[0]
	} else {
		us.pKind = pendingNone
	}
	us.mu.Unlock()

	_ = us.br.RespondPermission(head.reqID, allow, nil)
	if next != nil {
		// 还有排队中的权限请求:推送下一条,继续等用户确认(不解禁发送)。
		sm.send(us.userID, us.token(), next.prompt)
		return
	}
	sm.markBusy(us) // 队列清空,claude 将继续本轮,恢复 typing 保活
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
	sm.markBusy(us)
	_ = us.br.RespondAskUserQuestion(reqID, answers)
}

// resetSession 主动重置某用户的对话:关闭当前活跃 claude 进程(若有)并清除持久化
// session_id,使下一条消息开启全新上下文。关闭会话时抑制其 resume 失败自愈逻辑
// (置 gotReady,避免 consume defer 误判为 resume 失败而重建)。
func (sm *sessionManager) resetSession(userID string) {
	sm.mu.Lock()
	us, ok := sm.byUser[userID]
	if ok {
		delete(sm.byUser, userID)
	}
	sm.mu.Unlock()
	clearSessionID(sm.sessionDir, userID)
	if ok && us.br != nil {
		us.mu.Lock()
		us.gotReady = true // 抑制 consume defer 的 resume 失败自愈
		us.firstText = ""
		us.mu.Unlock()
		us.br.Close()
	}
}

// recreateFresh 在 resume 失败后开一个全新 claude 会话(不带 --resume),
// 补发用户的首条消息,并提示上下文已重置。sid 已在调用前被清除,故此处必是全新会话。
func (sm *sessionManager) recreateFresh(userID, firstText, firstToken string) {
	us, created := sm.getOrCreate(userID)
	if us == nil || !created {
		return // 容量已满,或期间已被其它消息重建
	}
	us.mu.Lock()
	us.contextToken = firstToken
	us.lastActive = time.Now()
	us.mu.Unlock()
	sm.send(userID, firstToken, "⚠️ 上一段对话的上下文已失效，已为你开启新的对话。")
	if firstText != "" {
		sm.markBusy(us)
		_ = us.br.SendUserMessage(firstText)
	}
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
	// 若该用户有已持久化的 session_id,则带 --resume 续接,恢复完整上下文。
	cfg := sm.cfg
	sid := loadSessionID(sm.sessionDir, userID)
	if sid != "" {
		cfg.SessionID = sid
	}
	br, err := sm.newBridge(cfg)
	if err != nil {
		log.Printf("[wechat] 启动 claude 失败 user=%s: %v", userID, err)
		return nil, false
	}
	us = &userSession{
		userID:     userID,
		br:         br,
		lastActive: time.Now(),
		quit:       make(chan struct{}),
		sessionID:  sid,
		resuming:   sid != "",
	}
	sm.byUser[userID] = us
	go sm.consume(us)
	go sm.typingKeepalive(us)
	return us, true
}

// setBusy 设置处理中标志。
func (sm *sessionManager) setBusy(us *userSession, busy bool) {
	us.mu.Lock()
	us.busy = busy
	us.mu.Unlock()
}

// consume 消费 claude 事件,翻译为微信文本并回发;权限/问卷转入待确认状态。
func (sm *sessionManager) consume(us *userSession) {
	defer func() {
		close(us.quit) // 停掉 typing keepalive
		sm.remove(us.userID)
		// resume 失败自愈:以 --resume 启动却在收到 ready 前就退出,说明旧 session_id
		// 已失效(claude 清过历史等)。清掉坏 sid,自动开一个全新会话并补发首条消息,
		// 提示用户上下文已重置,避免用户"发了没反应"。
		us.mu.Lock()
		failedResume := us.resuming && !us.gotReady
		firstText := us.firstText
		firstToken := us.firstToken
		us.mu.Unlock()
		if failedResume {
			log.Printf("[wechat] resume 失败,降级为新会话 user=%s", us.userID)
			clearSessionID(sm.sessionDir, us.userID)
			sm.recreateFresh(us.userID, firstText, firstToken)
		}
	}()
	for ev := range us.br.Events() {
		switch ev["type"] {
		case "ready":
			// 记录并持久化 session_id,供后续进程重建时 --resume 续接。
			sid := protocol.StrOr(ev["session_id"], "")
			us.mu.Lock()
			us.gotReady = true
			if sid != "" {
				us.sessionID = sid
			}
			us.mu.Unlock()
			if sid != "" {
				if err := saveSessionID(sm.sessionDir, us.userID, sid); err != nil {
					log.Printf("[wechat] 保存 session_id 失败 user=%s: %v", us.userID, err)
				}
			}
		case "permission_request":
			reqID := protocol.StrOr(ev["request_id"], "")
			// 白名单:只读操作自动放行,不打扰用户;其余转入聊天内确认。
			if autoApprove(protocol.StrOr(ev["tool_name"], ""), ev["tool_input"]) {
				_ = us.br.RespondPermission(reqID, true, nil)
				continue
			}
			// 排队:claude 一轮内可能连发多个权限请求,逐个入队,只有队列此前为空
			// (即无正在询问用户的确认)时才立即推送队首,其余静默排队,前一个应答后再推。
			prompt := renderPermissionPrompt(ev)
			us.mu.Lock()
			us.pKind = pendingPermission
			wasEmpty := len(us.permQueue) == 0
			us.permQueue = append(us.permQueue, permPending{reqID: reqID, prompt: prompt})
			us.mu.Unlock()
			if wasEmpty {
				sm.send(us.userID, us.token(), prompt)
			}
			sm.setBusy(us, false) // 转为等待用户确认,暂停 typing
		case "user_question":
			questions, _ := ev["questions"].([]any)
			us.mu.Lock()
			us.pKind = pendingQuestion
			us.pReqID = protocol.StrOr(ev["request_id"], "")
			us.pQuestions = questions
			us.mu.Unlock()
			sm.send(us.userID, us.token(), renderQuestionPrompt(questions))
			sm.setBusy(us, false)
		case "result":
			sm.setBusy(us, false) // 本轮结束,停 typing
			sm.stopTyping(us)
		default:
			if text := renderEvent(ev); text != "" {
				sm.send(us.userID, us.token(), text)
			}
		}
	}
}

// stopTyping 发送 sendtyping(status:2) 取消"正在输入"。
func (sm *sessionManager) stopTyping(us *userSession) {
	us.mu.Lock()
	ticket := us.ticket
	us.mu.Unlock()
	if ticket == "" {
		return
	}
	ctx, cancel := context.WithTimeout(sm.ctx, sendTimeout)
	defer cancel()
	_ = sm.client.SendTyping(ctx, us.userID, ticket, 2)
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
