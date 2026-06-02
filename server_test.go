package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func startTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	cfg := Config{Token: token, ClaudeBin: fakeClaudePath, WorkDir: "/tmp", PermissionMode: "default"}
	return httptest.NewServer(NewServer(cfg).Routes())
}

func wsURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

func readJSON(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m map[string]any
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	return m
}

func TestHealthz(t *testing.T) {
	ts := startTestServer(t, "secret")
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz 异常: err=%v", err)
	}
}

func TestChatRejectsBadToken(t *testing.T) {
	ts := startTestServer(t, "secret")
	defer ts.Close()
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/agent/chat?token=wrong"), nil)
	if err == nil {
		t.Fatal("错误 token 应被拒绝")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应返回 401，实际: %+v", resp)
	}
}

func TestChatRejectsMissingToken(t *testing.T) {
	ts := startTestServer(t, "secret")
	defer ts.Close()
	if _, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/agent/chat"), nil); err == nil {
		t.Fatal("缺失 token 应被拒绝")
	}
}

func TestChatIdleTimeoutClosesConnection(t *testing.T) {
	cfg := Config{Token: "secret", ClaudeBin: fakeClaudePath, WorkDir: "/tmp", IdleTimeoutSec: 1}
	ts := httptest.NewServer(NewServer(cfg).Routes())
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/agent/chat?token=secret"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// 不发任何用户消息 → 空闲；持续读取以处理 ping/close，期望被服务端关闭
	c.SetReadDeadline(time.Now().Add(8 * time.Second))
	closed := false
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			closed = true // 服务端因空闲主动关闭
			break
		}
	}
	if !closed {
		t.Fatal("空闲超时未关闭连接")
	}
}

func TestChatFullRoundtripOverWebSocket(t *testing.T) {
	ts := startTestServer(t, "secret")
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/agent/chat?token=secret"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// ready
	if ev := readJSON(t, c); ev["type"] != "ready" {
		t.Fatalf("应为 ready: %+v", ev)
	}

	// 发用户消息
	if err := c.WriteJSON(map[string]any{"type": "user_message", "text": "执行 echo hi"}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// 收到 assistant + permission_request（顺序读取直到拿到权限请求）
	var perm map[string]any
	for i := 0; i < 10; i++ {
		ev := readJSON(t, c)
		if ev["type"] == "permission_request" {
			perm = ev
			break
		}
	}
	if perm == nil || perm["request_id"] != "perm_1" {
		t.Fatalf("未收到权限请求: %+v", perm)
	}

	// 放行
	if err := c.WriteJSON(map[string]any{
		"type": "permission_response", "request_id": "perm_1", "allow": true,
	}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// 收到 tool_result + result
	var result map[string]any
	for i := 0; i < 10; i++ {
		ev := readJSON(t, c)
		if ev["type"] == "result" {
			result = ev
			break
		}
	}
	if result == nil || result["result"] != "完成" {
		t.Fatalf("未收到正确 result: %+v", result)
	}
}
