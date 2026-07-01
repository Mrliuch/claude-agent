package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"claude-agent/internal/config"
)

// newStoreTestManager 构造一个带 sessionDir 的 manager,并把每次 newBridge 收到的
// cfg 记录到 gotCfgs 通道,便于断言是否带 --resume(cfg.SessionID)。
func newStoreTestManager(t *testing.T, dir string) (*sessionManager, chan *fakeBridge, chan config.Config, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getconfig":
			_ = json.NewEncoder(w).Encode(getConfigResp{TypingTicket: "tkt"})
		default:
			w.Write([]byte(`{"ret":0}`))
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	sm := newSessionManager(ctx, config.Config{IdleTimeoutSec: 1800}, NewClient(srv.URL), 5)
	sm.sessionDir = dir

	bridges := make(chan *fakeBridge, 8)
	gotCfgs := make(chan config.Config, 8)
	sm.newBridge = func(c config.Config) (claudeBridge, error) {
		fb := newFakeBridge()
		gotCfgs <- c
		bridges <- fb
		return fb, nil
	}
	return sm, bridges, gotCfgs, cancel
}

// TestResetCommandClearsContext:重置口令清除持久化 sid、关闭当前会话进程。
func TestResetCommandClearsContext(t *testing.T) {
	dir := t.TempDir()
	if err := saveSessionID(dir, "u1", "sess-x"); err != nil {
		t.Fatal(err)
	}
	sm, bridges, _, cancel := newStoreTestManager(t, dir)
	defer cancel()

	// 先建立一个活跃会话。
	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	fb := <-bridges
	<-fb.userMsgs

	// 发重置口令。
	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c2", ItemList: textItems("/new")})

	// 会话进程应被关闭。
	select {
	case <-fb.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("重置未关闭当前会话进程")
	}
	// 持久化 sid 应被清除。
	if got := loadSessionID(dir, "u1"); got != "" {
		t.Fatalf("重置后 sid 未清除,got %q", got)
	}
	// 会话映射应被移除。
	sm.mu.Lock()
	_, exists := sm.byUser["u1"]
	sm.mu.Unlock()
	if exists {
		t.Fatal("重置后会话仍在 byUser 中")
	}
}

// TestIsResetCommand:口令识别覆盖中英文与大小写。
func TestIsResetCommand(t *testing.T) {
	yes := []string{"/new", "/NEW", " /reset ", "/清空", "/重置", "/新对话"}
	no := []string{"new", "重置", "/newx", "你好", "/"}
	for _, s := range yes {
		if !isResetCommand(s) {
			t.Errorf("isResetCommand(%q)=false, want true", s)
		}
	}
	for _, s := range no {
		if isResetCommand(s) {
			t.Errorf("isResetCommand(%q)=true, want false", s)
		}
	}
}

// TestSessionIDPersistedOnReady:收到 ready 帧后,session_id 落盘。
func TestSessionIDPersistedOnReady(t *testing.T) {
	dir := t.TempDir()
	sm, bridges, _, cancel := newStoreTestManager(t, dir)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	fb := <-bridges
	<-fb.userMsgs

	fb.events <- map[string]any{"type": "ready", "session_id": "sess-abc"}

	// 轮询等待落盘。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if loadSessionID(dir, "u1") == "sess-abc" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session_id 未持久化,got %q", loadSessionID(dir, "u1"))
}

// TestResumeUsesStoredSessionID:磁盘已有 sid 时,新建会话带 --resume(cfg.SessionID)。
func TestResumeUsesStoredSessionID(t *testing.T) {
	dir := t.TempDir()
	if err := saveSessionID(dir, "u1", "sess-old"); err != nil {
		t.Fatal(err)
	}
	sm, bridges, gotCfgs, cancel := newStoreTestManager(t, dir)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	<-bridges

	select {
	case c := <-gotCfgs:
		if c.SessionID != "sess-old" {
			t.Fatalf("cfg.SessionID = %q, want sess-old (应带 --resume)", c.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("newBridge 未被调用")
	}
}

// TestResumeFailureFallsBackToFresh:resume 会话在收到 ready 前退出 → 清 sid、
// 开全新会话(不带 resume)并补发首条消息。
func TestResumeFailureFallsBackToFresh(t *testing.T) {
	dir := t.TempDir()
	if err := saveSessionID(dir, "u1", "sess-bad"); err != nil {
		t.Fatal(err)
	}
	sm, bridges, gotCfgs, cancel := newStoreTestManager(t, dir)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("原始问题")})
	fb1 := <-bridges
	c1 := <-gotCfgs
	if c1.SessionID != "sess-bad" {
		t.Fatalf("首次应 resume, cfg.SessionID=%q", c1.SessionID)
	}
	<-fb1.userMsgs // 首条消息已发给(失败的)resume 会话

	// 模拟 resume 失败:未发 ready 直接关闭事件通道(进程退出)。
	fb1.Close()
	close(fb1.events)

	// 应自动重建全新会话:第二次 newBridge 不带 SessionID。
	select {
	case c2 := <-gotCfgs:
		if c2.SessionID != "" {
			t.Fatalf("降级会话不应带 resume, cfg.SessionID=%q", c2.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume 失败后未重建新会话")
	}
	fb2 := <-bridges

	// 首条消息应补发到新会话。
	select {
	case m := <-fb2.userMsgs:
		if m != "原始问题" {
			t.Fatalf("补发消息=%q, want 原始问题", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未向新会话补发首条消息")
	}

	// 坏 sid 应已被清除。
	if got := loadSessionID(dir, "u1"); got != "" {
		t.Fatalf("坏 session_id 未清除,got %q", got)
	}
}
