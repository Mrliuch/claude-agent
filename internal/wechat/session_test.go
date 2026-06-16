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

// fakeBridge 是 claudeBridge 的测试替身,用通道暴露被调用情况。
type fakeBridge struct {
	events   chan map[string]any
	userMsgs chan string
	perms    chan permCall
	answers  chan ansCall
	closed   chan struct{}
}

type permCall struct {
	id    string
	allow bool
}
type ansCall struct {
	id      string
	answers map[string]any
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{
		events:   make(chan map[string]any, 8),
		userMsgs: make(chan string, 8),
		perms:    make(chan permCall, 8),
		answers:  make(chan ansCall, 8),
		closed:   make(chan struct{}, 1),
	}
}

func (f *fakeBridge) Events() <-chan map[string]any { return f.events }
func (f *fakeBridge) SendUserMessage(t string) error {
	f.userMsgs <- t
	return nil
}
func (f *fakeBridge) RespondPermission(id string, allow bool, _ any) error {
	f.perms <- permCall{id, allow}
	return nil
}
func (f *fakeBridge) RespondAskUserQuestion(id string, ans map[string]any) error {
	f.answers <- ansCall{id, ans}
	return nil
}
func (f *fakeBridge) Close() {
	select {
	case f.closed <- struct{}{}:
	default:
	}
}

func textItems(s string) []Item {
	return []Item{{Type: itemTypeText, TextItem: &TextItem{Text: s}}}
}

// newTestManager 构造一个把回复发往 mock 服务端的 sessionManager。
func newTestManager(t *testing.T, maxSess int) (*sessionManager, *fakeBridge, chan string, context.CancelFunc) {
	t.Helper()
	sent := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body sendMessageReq
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent <- body.Msg.Text()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Config{IdleTimeoutSec: 1800}
	sm := newSessionManager(ctx, cfg, NewClient(srv.URL), maxSess)
	fb := newFakeBridge()
	sm.newBridge = func(config.Config) (claudeBridge, error) { return fb, nil }
	return sm, fb, sent, cancel
}

func recvString(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message")
		return ""
	}
}

func TestDispatchFirstMessageSendsToBridge(t *testing.T) {
	sm, fb, _, cancel := newTestManager(t, 5)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hello")})
	select {
	case msg := <-fb.userMsgs:
		if msg != "hello" {
			t.Errorf("got %q want hello", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendUserMessage not called")
	}
}

func TestPermissionFlow(t *testing.T) {
	sm, fb, sent, cancel := newTestManager(t, 5)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("do it")})
	<-fb.userMsgs // 首条消息

	// claude 请求权限 → 应转成微信提示并进入待确认。
	fb.events <- map[string]any{
		"type": "permission_request", "request_id": "req-1",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"},
	}
	if prompt := recvString(t, sent); prompt == "" {
		t.Fatal("expected permission prompt sent")
	}

	// 用户回复"允许" → 应调用 RespondPermission(allow=true)。
	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c2", ItemList: textItems("允许")})
	select {
	case p := <-fb.perms:
		if p.id != "req-1" || !p.allow {
			t.Errorf("got %+v want {req-1 true}", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RespondPermission not called")
	}
}

func TestQuestionFlow(t *testing.T) {
	sm, fb, sent, cancel := newTestManager(t, 5)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	<-fb.userMsgs

	fb.events <- map[string]any{
		"type": "user_question", "request_id": "q-1",
		"questions": sampleQuestions(),
	}
	recvString(t, sent) // 问卷提示

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c2", ItemList: textItems("2")})
	select {
	case a := <-fb.answers:
		if a.id != "q-1" || a.answers["选哪个?"] != "乙" {
			t.Errorf("got %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RespondAskUserQuestion not called")
	}
}

func TestCapacityLimit(t *testing.T) {
	sm, _, sent, cancel := newTestManager(t, 1)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	// 第二个用户超出上限 → 收到忙提示。
	sm.Dispatch(Message{FromUserID: "u2", ContextToken: "c2", ItemList: textItems("hi")})
	if msg := recvString(t, sent); msg == "" {
		t.Fatal("expected capacity message")
	}
}

func TestReapIdle(t *testing.T) {
	sm, fb, _, cancel := newTestManager(t, 5)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	<-fb.userMsgs

	// 强制空闲过期并回收。
	sm.idle = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	sm.reapIdle()

	select {
	case <-fb.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle session not closed")
	}
	sm.mu.Lock()
	n := len(sm.byUser)
	sm.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 sessions after reap, got %d", n)
	}
}
