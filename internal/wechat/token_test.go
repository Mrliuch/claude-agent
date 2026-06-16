package wechat

import (
	"path/filepath"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "wechat_token")
	if got := loadToken(path); got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
	if err := saveToken(path, "tok-123"); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	if got := loadToken(path); got != "tok-123" {
		t.Errorf("loadToken=%q want tok-123", got)
	}
}

func TestSaveTokenEmptyPathNoop(t *testing.T) {
	if err := saveToken("", "x"); err != nil {
		t.Errorf("empty path should be noop, got %v", err)
	}
	if got := loadToken(""); got != "" {
		t.Errorf("empty path load should be empty, got %q", got)
	}
}

func TestMessageText(t *testing.T) {
	m := Message{ItemList: []Item{
		{Type: itemTypeText, TextItem: &TextItem{Text: "ab"}},
		{Type: itemTypeText, TextItem: &TextItem{Text: "cd"}},
		{Type: itemTypeText}, // 无 TextItem,应被跳过
	}}
	if m.Text() != "abcd" {
		t.Errorf("Text()=%q want abcd", m.Text())
	}
}

func TestTextMessage(t *testing.T) {
	m := textMessage("u@im.wechat", "ctx", "hi")
	if m.ToUserID != "u@im.wechat" || m.ContextToken != "ctx" {
		t.Errorf("unexpected message header: %+v", m)
	}
	if len(m.ItemList) != 1 || m.ItemList[0].TextItem.Text != "hi" {
		t.Errorf("unexpected item list: %+v", m.ItemList)
	}
	if m.MessageType != msgTypeReply || m.MessageState != msgStateFinal {
		t.Errorf("unexpected type/state: %+v", m)
	}
}
