package wechat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewRequestHeaders(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.SetToken("tok-xyz")
	if err := c.SendTyping(context.Background(), "u@im.wechat", "ticket-1", 1); err != nil {
		t.Fatalf("SendTyping: %v", err)
	}
	if got.Header.Get("Authorization") != "Bearer tok-xyz" {
		t.Errorf("Authorization=%q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("AuthorizationType") != "ilink_bot_token" {
		t.Errorf("AuthorizationType=%q", got.Header.Get("AuthorizationType"))
	}
	if got.Header.Get("iLink-App-Id") != "bot" {
		t.Errorf("iLink-App-Id=%q", got.Header.Get("iLink-App-Id"))
	}
	if got.Header.Get("X-WECHAT-UIN") == "" {
		t.Errorf("X-WECHAT-UIN should be set")
	}
}

func TestGetUpdatesRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/getupdates" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req getUpdatesReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.GetUpdatesBuf != "cursor-1" {
			t.Errorf("cursor not forwarded: %q", req.GetUpdatesBuf)
		}
		_ = json.NewEncoder(w).Encode(getUpdatesResp{
			Msgs: []Message{{
				FromUserID:   "u@im.wechat",
				ContextToken: "ctx-1",
				ItemList:     []Item{{Type: itemTypeText, TextItem: &TextItem{Text: "hi"}}},
			}},
			GetUpdatesBuf: "cursor-2",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	resp, err := c.GetUpdates(context.Background(), "cursor-1")
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if resp.GetUpdatesBuf != "cursor-2" {
		t.Errorf("cursor=%q want cursor-2", resp.GetUpdatesBuf)
	}
	if len(resp.Msgs) != 1 || resp.Msgs[0].Text() != "hi" {
		t.Errorf("unexpected msgs: %+v", resp.Msgs)
	}
}

func TestSendMessageBody(t *testing.T) {
	var body sendMessageReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.SendMessage(context.Background(), "u@im.wechat", "ctx-9", "回复内容"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if body.Msg.ToUserID != "u@im.wechat" || body.Msg.ContextToken != "ctx-9" {
		t.Errorf("unexpected body header: %+v", body.Msg)
	}
	if body.Msg.Text() != "回复内容" {
		t.Errorf("unexpected text: %q", body.Msg.Text())
	}
}

func TestUnauthorizedMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.GetUpdates(context.Background(), "")
	if err != ErrUnauthorized {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}
