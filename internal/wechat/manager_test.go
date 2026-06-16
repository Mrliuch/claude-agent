package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-agent/internal/config"
)

// mockILink 返回一个最小可用的 iLink 服务端:getupdates 空、出码、状态 wait。
func mockILink(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/getupdates"):
			_ = json.NewEncoder(w).Encode(getUpdatesResp{GetUpdatesBuf: ""})
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/get_bot_qrcode"):
			_ = json.NewEncoder(w).Encode(qrCodeResp{QRCode: "k", QRCodeImgContent: "https://liteapp/q/x?qrcode=k"})
		case strings.HasPrefix(r.URL.Path, "/ilink/bot/get_qrcode_status"):
			_ = json.NewEncoder(w).Encode(qrStatusResp{Status: "wait"})
		default:
			w.Write([]byte(`{"ret":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestManagerRestoreOnline(t *testing.T) {
	srv := mockILink(t)
	dir := t.TempDir()
	if err := saveToken(filepath.Join(dir, "acc1.token"), "tok-1"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{WeChatBaseURL: srv.URL, WeChatTokenPath: filepath.Join(dir, "_.token"), IdleTimeoutSec: 1800}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, cfg)
	m.Restore()

	waitFor(t, func() bool {
		for _, a := range m.List() {
			if a.ID == "acc1" && a.Status == StatusOnline {
				return true
			}
		}
		return false
	})
}

func TestManagerAddPendingQRAndRemove(t *testing.T) {
	srv := mockILink(t)
	dir := t.TempDir()
	cfg := config.Config{WeChatBaseURL: srv.URL, WeChatTokenPath: filepath.Join(dir, "_.token"), IdleTimeoutSec: 1800}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, cfg)

	id := m.Add("测试号")
	// 无 token → 进入 pending 并出码,QR 可取。
	waitFor(t, func() bool {
		_, ok := m.QR(id)
		return ok
	})
	var found bool
	for _, a := range m.List() {
		if a.ID == id {
			found = true
			if a.Name != "测试号" {
				t.Errorf("name=%q", a.Name)
			}
		}
	}
	if !found {
		t.Fatal("added account not listed")
	}
	// token 文件 / name 文件应已落盘
	if _, err := os.Stat(m.namePath(id)); err != nil {
		t.Errorf("name file missing: %v", err)
	}

	if !m.Remove(id) {
		t.Error("remove returned false")
	}
	for _, a := range m.List() {
		if a.ID == id {
			t.Error("account still listed after remove")
		}
	}
}

func TestManagerMigrateLegacy(t *testing.T) {
	srv := mockILink(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy_token")
	if err := os.WriteFile(legacy, []byte("old-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{WeChatBaseURL: srv.URL, WeChatTokenPath: legacy, IdleTimeoutSec: 1800}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, cfg)
	m.Restore()
	if loadToken(m.tokenPath("default")) != "old-tok" {
		t.Error("legacy token not migrated to default account")
	}
}
