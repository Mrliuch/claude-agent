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
		switch {
		case r.URL.Path == "/ilink/bot/sendmessage":
			var body sendMessageReq
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent <- body.Msg.Text()
			w.Write([]byte(`{}`))
		case r.URL.Path == "/ilink/bot/getconfig":
			_ = json.NewEncoder(w).Encode(getConfigResp{TypingTicket: "tkt"})
		default: // sendtyping 等
			w.Write([]byte(`{"ret":0}`))
		}
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

	// claude 请求危险操作权限 → 应转成微信提示并进入待确认(只读命令会被白名单放行,故用 rm)。
	fb.events <- map[string]any{
		"type": "permission_request", "request_id": "req-1",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf /tmp/x"},
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

// TestMultiplePermissionRequestsQueued 覆盖回归 bug:claude 一轮内连续发出多个
// permission_request 时,必须逐个排队确认——每个 reqID 都要被应答,不能被后来的覆盖丢失。
func TestMultiplePermissionRequestsQueued(t *testing.T) {
	sm, fb, sent, cancel := newTestManager(t, 5)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("do it")})
	<-fb.userMsgs

	// claude 一轮内连发两个危险操作权限请求。
	fb.events <- map[string]any{
		"type": "permission_request", "request_id": "req-1",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf /tmp/a"},
	}
	fb.events <- map[string]any{
		"type": "permission_request", "request_id": "req-2",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf /tmp/b"},
	}

	// 仅应先推送第一条确认(第二条排队,暂不打扰用户)。
	if prompt := recvString(t, sent); prompt == "" {
		t.Fatal("expected first permission prompt")
	}

	// 用户确认第一个 → 应答 req-1,随后自动推送第二条确认。
	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c2", ItemList: textItems("y")})
	select {
	case p := <-fb.perms:
		if p.id != "req-1" || !p.allow {
			t.Fatalf("first ack got %+v want {req-1 true}", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("req-1 not answered")
	}
	if prompt := recvString(t, sent); prompt == "" {
		t.Fatal("expected second permission prompt after first ack")
	}

	// 用户确认第二个 → 应答 req-2(此前会被覆盖丢失,导致会话卡死)。
	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c3", ItemList: textItems("y")})
	select {
	case p := <-fb.perms:
		if p.id != "req-2" || !p.allow {
			t.Fatalf("second ack got %+v want {req-2 true}", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("req-2 not answered — queued permission lost (regression)")
	}
}

func TestPermissionAutoApprove(t *testing.T) {
	sm, fb, _, cancel := newTestManager(t, 5)
	defer cancel()

	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("巡检")})
	<-fb.userMsgs

	// 只读命令 → 应自动放行(直接 RespondPermission true),不发提示、不进入待确认。
	fb.events <- map[string]any{
		"type": "permission_request", "request_id": "ro-1",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "df -h && free -m"},
	}
	select {
	case p := <-fb.perms:
		if p.id != "ro-1" || !p.allow {
			t.Errorf("got %+v want auto-allow ro-1", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read-only permission not auto-approved")
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
