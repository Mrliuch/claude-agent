package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claude-agent/internal/config"
)

func testServer() *Server {
	return NewServer(config.Config{Token: "secret", UIEnabled: true})
}

func TestUIGate(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := &http.Client{}

	// 未认证 → 登录页
	resp, _ := c.Get(srv.URL + "/")
	body := readBody(resp)
	if !strings.Contains(body, "AGENT_TOKEN") || strings.Contains(body, "CLAUDE-AGENT") {
		t.Errorf("unauthed / should serve login page, got: %.80s", body)
	}

	// 带 token query → 控制台
	resp2, _ := c.Get(srv.URL + "/?token=secret")
	if b := readBody(resp2); !strings.Contains(b, "CLAUDE-AGENT") {
		t.Errorf("authed / should serve console")
	}
}

func TestLoginSetsCookie(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// 错误 token → 拒绝
	bad, _ := http.Post(srv.URL+"/agent/login?token=wrong", "", nil)
	if !strings.Contains(readBody(bad), "无效") {
		t.Errorf("wrong token should be rejected")
	}

	// 正确 token → 种 cookie
	ok, _ := http.Post(srv.URL+"/agent/login?token=secret", "", nil)
	var hasCookie bool
	for _, ck := range ok.Cookies() {
		if ck.Name == authCookie && ck.Value == "secret" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Error("valid login should set auth cookie")
	}
}

func TestWeChatEndpointAuth(t *testing.T) {
	s := testServer() // wechat 未注入
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// 无 token → 401(JSON code)
	r1, _ := http.Get(srv.URL + "/agent/wechat/accounts")
	if !strings.Contains(readBody(r1), "unauthorized") {
		t.Error("wechat accounts without token should be unauthorized")
	}
	// 带 token 但未启用 → 提示未启用
	r2, _ := http.Get(srv.URL + "/agent/wechat/accounts?token=secret")
	if !strings.Contains(readBody(r2), "未启用") {
		t.Error("wechat disabled should report not-enabled")
	}
}

func readBody(r *http.Response) string {
	if r == nil {
		return ""
	}
	defer r.Body.Close()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(b)
}
