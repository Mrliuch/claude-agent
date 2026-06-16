package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-agent/internal/config"
)

func doneAfter(sec int) <-chan time.Time {
	return time.After(time.Duration(sec) * time.Second)
}

func TestGetBotQRCodeAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/get_bot_qrcode"):
			_ = json.NewEncoder(w).Encode(qrCodeResp{QRCode: "wxp://x", Key: "k1"})
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/get_qrcode_status"):
			if r.URL.Query().Get("qrcode") != "k1" {
				t.Errorf("qrcode param=%q", r.URL.Query().Get("qrcode"))
			}
			_ = json.NewEncoder(w).Encode(qrStatusResp{Status: "confirmed", BotToken: "tok-abc"})
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	qr, err := c.GetBotQRCode(context.Background())
	if err != nil || qr.Key != "k1" {
		t.Fatalf("GetBotQRCode err=%v qr=%+v", err, qr)
	}
	st, err := c.GetQRCodeStatus(context.Background(), qr.Key)
	if err != nil || st.BotToken != "tok-abc" {
		t.Fatalf("GetQRCodeStatus err=%v st=%+v", err, st)
	}
}

func TestEnsureLoginFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	if err := saveToken(path, "saved-tok"); err != nil {
		t.Fatal(err)
	}
	ch := NewChannel(config.Config{WeChatTokenPath: path, WeChatBaseURL: "https://example.invalid"})
	if err := ch.ensureLogin(context.Background()); err != nil {
		t.Fatalf("ensureLogin: %v", err)
	}
	if ch.client.Token() != "saved-tok" {
		t.Errorf("token=%q want saved-tok", ch.client.Token())
	}
}

func TestReloginScanFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/get_bot_qrcode"):
			_ = json.NewEncoder(w).Encode(qrCodeResp{QRCode: "wxp://login", Key: "key-9"})
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/get_qrcode_status"):
			_ = json.NewEncoder(w).Encode(qrStatusResp{Status: "confirmed", BotToken: "fresh-tok"})
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "tok")
	ch := NewChannel(config.Config{WeChatTokenPath: path, WeChatBaseURL: srv.URL})
	if err := ch.relogin(context.Background()); err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if ch.client.Token() != "fresh-tok" {
		t.Errorf("token=%q want fresh-tok", ch.client.Token())
	}
	if loadToken(path) != "fresh-tok" {
		t.Errorf("token not persisted")
	}
}

func TestReloginCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(qrCodeResp{QRCode: "wxp://x", Key: "k"})
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消,relogin 应在首个 sleep 处返回。
	ch := NewChannel(config.Config{WeChatBaseURL: srv.URL})
	if err := ch.relogin(ctx); err == nil {
		t.Error("expected error on cancelled ctx")
	}
}

func TestRunDispatchesAndStops(t *testing.T) {
	fb := newFakeBridge()
	gotMsg := make(chan string, 1)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ilink/bot/getupdates") {
			calls++
			if calls == 1 {
				_ = json.NewEncoder(w).Encode(getUpdatesResp{
					Msgs:          []Message{{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("ping")}},
					GetUpdatesBuf: "cur",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(getUpdatesResp{GetUpdatesBuf: "cur"})
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "tok")
	_ = saveToken(tokenPath, "tok")
	ch := NewChannel(config.Config{WeChatBaseURL: srv.URL, WeChatTokenPath: tokenPath, WeChatMaxSessions: 5, IdleTimeoutSec: 1800})
	ch.newBridge = func(config.Config) (claudeBridge, error) { return fb, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = ch.Run(ctx); close(done) }()

	select {
	case m := <-fb.userMsgs:
		gotMsg <- m
	case <-doneAfter(2):
		t.Fatal("message not dispatched via Run")
	}
	if m := <-gotMsg; m != "ping" {
		t.Errorf("got %q want ping", m)
	}
	cancel()
	select {
	case <-done:
	case <-doneAfter(2):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestCloseAll(t *testing.T) {
	sm, fb, _, cancel := newTestManager(t, 5)
	defer cancel()
	sm.Dispatch(Message{FromUserID: "u1", ContextToken: "c1", ItemList: textItems("hi")})
	<-fb.userMsgs
	sm.closeAll()
	select {
	case <-fb.closed:
	case <-doneAfter(2):
		t.Fatal("closeAll did not close bridge")
	}
}

func TestPrintQRCode(t *testing.T) {
	printQRCode(qrCodeResp{QRCode: "wxp://test-content"}) // 渲染到 stdout,不应 panic
	printQRCode(qrCodeResp{URL: "https://example.com/qr"})
	printQRCode(qrCodeResp{}) // 空内容分支
}

func TestDefaultTokenPath(t *testing.T) {
	if p := defaultTokenPath(); p != "" && !strings.Contains(p, filepath.Join(".config", "claude-agent")) {
		t.Errorf("unexpected default path: %q", p)
	}
}
