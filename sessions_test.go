package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/private/tmp":                   "-private-tmp",
		"/Users/liuchen/code/monitor_v2": "-Users-liuchen-code-monitor-v2",
	}
	for in, want := range cases {
		if got := projectSlug(in); got != want {
			t.Fatalf("projectSlug(%q)=%q, want %q", in, got, want)
		}
	}
}

// 准备一个假的 ~/.claude/projects/<slug>/<id>.jsonl，返回 server 与 session id。
func newSessionServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()

	dir := filepath.Join(home, ".claude", "projects", projectSlug(work))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "6e80182e-7297-49c9-b6e6-e4ecfc34d498"
	lines := []string{
		`{"type":"queue-operation","operation":"enqueue"}`,
		`{"type":"user","timestamp":"2026-05-01T00:00:00Z","message":{"role":"user","content":"看看磁盘"}}`,
		`{"type":"assistant","timestamp":"2026-05-01T00:00:01Z","message":{"content":[{"type":"text","text":"好的"},{"type":"tool_use","name":"Bash","input":{"command":"df -h"}}]}}`,
		`{"type":"user","timestamp":"2026-05-01T00:00:02Z","message":{"content":[{"type":"tool_result","content":"Filesystem ...","is_error":false}]}}`,
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewServer(Config{Token: "t", WorkDir: work}), id
}

func TestSessionsListAndRead(t *testing.T) {
	s, id := newSessionServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// list
	r := doFs(t, ts, "GET", "/agent/sessions/list?token=t", "")
	if r["code"].(float64) != 0 {
		t.Fatalf("list 失败: %v", r)
	}
	data := r["data"].(map[string]any)
	sess := data["sessions"].([]any)
	if len(sess) != 1 {
		t.Fatalf("应有 1 个会话: %v", sess)
	}
	first := sess[0].(map[string]any)
	if first["id"] != id {
		t.Fatalf("id 错误: %v", first["id"])
	}
	if first["title"] != "看看磁盘" {
		t.Fatalf("title 错误: %v", first["title"])
	}
	if first["messages"].(float64) != 3 {
		t.Fatalf("消息数应为 3: %v", first["messages"])
	}

	// read
	r = doFs(t, ts, "GET", "/agent/sessions/read?token=t&id="+id, "")
	if r["code"].(float64) != 0 {
		t.Fatalf("read 失败: %v", r)
	}
	items := r["data"].(map[string]any)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("应有 3 条记录: %v", items)
	}
	it0 := items[0].(map[string]any)
	if it0["role"] != "user" {
		t.Fatalf("首条应为 user: %v", it0)
	}
	// assistant 第二条应含 text + tool_use 两个 block
	it1 := items[1].(map[string]any)
	blocks := it1["blocks"].([]any)
	if it1["role"] != "assistant" || len(blocks) != 2 {
		t.Fatalf("assistant blocks 错误: %v", it1)
	}
}

func TestSessionReadRejectsBadID(t *testing.T) {
	s, _ := newSessionServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	for _, bad := range []string{"../escape", "a/b", "..", "x.jsonl/../y"} {
		r := doFs(t, ts, "GET", "/agent/sessions/read?token=t&id="+bad, "")
		if r["code"].(float64) == 0 {
			t.Fatalf("非法 id %q 不应通过", bad)
		}
	}
}
