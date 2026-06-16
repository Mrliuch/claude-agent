package wechat

import (
	"strings"
	"testing"
)

func TestParsePermissionReply(t *testing.T) {
	cases := []struct {
		in        string
		wantAllow bool
		wantOK    bool
	}{
		{"y", true, true},
		{"允许", true, true},
		{" OK ", true, true},
		{"n", false, true},
		{"拒绝", false, true},
		{"否", false, true},
		{"maybe", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		allow, ok := parsePermissionReply(c.in)
		if allow != c.wantAllow || ok != c.wantOK {
			t.Errorf("parsePermissionReply(%q)=(%v,%v) want (%v,%v)", c.in, allow, ok, c.wantAllow, c.wantOK)
		}
	}
}

func sampleQuestions() []any {
	return []any{
		map[string]any{
			"question": "选哪个?",
			"options": []any{
				map[string]any{"label": "甲"},
				map[string]any{"label": "乙"},
			},
		},
	}
}

func TestParseQuestionReply_Single(t *testing.T) {
	qs := sampleQuestions()
	ans, ok := parseQuestionReply("2", qs)
	if !ok {
		t.Fatalf("expected ok")
	}
	if ans["选哪个?"] != "乙" {
		t.Errorf("got %v want 乙", ans["选哪个?"])
	}
}

func TestParseQuestionReply_Invalid(t *testing.T) {
	qs := sampleQuestions()
	for _, in := range []string{"3", "0", "x", "1 2"} {
		if _, ok := parseQuestionReply(in, qs); ok {
			t.Errorf("parseQuestionReply(%q) expected not ok", in)
		}
	}
}

func TestParseQuestionReply_Multi(t *testing.T) {
	qs := []any{
		map[string]any{"question": "Q1", "options": []any{
			map[string]any{"label": "a"}, map[string]any{"label": "b"}}},
		map[string]any{"question": "Q2", "options": []any{
			map[string]any{"label": "c"}, map[string]any{"label": "d"}}},
	}
	ans, ok := parseQuestionReply("1，2", qs)
	if !ok {
		t.Fatalf("expected ok")
	}
	if ans["Q1"] != "a" || ans["Q2"] != "d" {
		t.Errorf("got %v", ans)
	}
}

func TestRenderAssistantText(t *testing.T) {
	ev := map[string]any{
		"type": "assistant",
		"blocks": []map[string]any{
			{"kind": "text", "text": "你好"},
			{"kind": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls -la"}},
		},
	}
	out := renderEvent(ev)
	if !strings.Contains(out, "你好") || !strings.Contains(out, "Bash: ls -la") {
		t.Errorf("unexpected render: %q", out)
	}
}

func TestRenderToolResultOnlyError(t *testing.T) {
	ok := map[string]any{"type": "tool_result", "results": []map[string]any{{"content": "fine", "is_error": false}}}
	if renderEvent(ok) != "" {
		t.Errorf("non-error tool_result should be silent")
	}
	bad := map[string]any{"type": "tool_result", "results": []map[string]any{{"content": "boom", "is_error": true}}}
	if !strings.Contains(renderEvent(bad), "boom") {
		t.Errorf("error tool_result should be reported")
	}
}

func TestRenderClosedAndReadySilent(t *testing.T) {
	if renderEvent(map[string]any{"type": "ready"}) != "" {
		t.Errorf("ready should be silent")
	}
	if renderEvent(map[string]any{"type": "closed", "stderr": ""}) != "" {
		t.Errorf("clean close should be silent")
	}
	if !strings.Contains(renderEvent(map[string]any{"type": "closed", "stderr": "panic"}), "panic") {
		t.Errorf("close with stderr should report")
	}
}

func TestToolSummary(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"Bash", map[string]any{"command": "ls -la"}, "Bash: ls -la"},
		{"Write", map[string]any{"file_path": "/a/b.go"}, "Write: /a/b.go"},
		{"Read", map[string]any{"file_path": "/x.txt"}, "Read: /x.txt"},
		{"Grep", map[string]any{"pattern": "foo"}, "Grep: "},
		{"Bash", nil, "Bash"},
	}
	for _, c := range cases {
		got := toolSummary(c.name, c.input)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("toolSummary(%s,%v)=%q want prefix %q", c.name, c.input, got, c.want)
		}
	}
}

func TestRenderPermissionPrompt(t *testing.T) {
	ev := map[string]any{
		"type":       "permission_request",
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "rm -rf /tmp/x"},
	}
	out := renderPermissionPrompt(ev)
	if !strings.Contains(out, "rm -rf /tmp/x") || !strings.Contains(out, "允许") {
		t.Errorf("unexpected prompt: %q", out)
	}
}

func TestRenderQuestionPrompt(t *testing.T) {
	out := renderQuestionPrompt(sampleQuestions())
	if !strings.Contains(out, "1. 甲") || !strings.Contains(out, "2. 乙") {
		t.Errorf("unexpected question prompt: %q", out)
	}
}
