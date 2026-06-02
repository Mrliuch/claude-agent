package main

import (
	"encoding/json"
	"testing"
)

func mustTranslate(t *testing.T, raw string) (map[string]any, bool) {
	t.Helper()
	return translate([]byte(raw))
}

func TestTranslateCanUseToolToPermissionRequest(t *testing.T) {
	ev, ok := mustTranslate(t, `{"type":"control_request","request_id":"req_9","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /tmp/x"}}}`)
	if !ok {
		t.Fatal("应翻译为事件")
	}
	if ev["type"] != "permission_request" {
		t.Fatalf("type=%v", ev["type"])
	}
	if ev["request_id"] != "req_9" || ev["tool_name"] != "Bash" {
		t.Fatalf("字段错误: %+v", ev)
	}
	in, _ := ev["tool_input"].(map[string]any)
	if in["command"] != "rm -rf /tmp/x" {
		t.Fatalf("tool_input 错误: %+v", in)
	}
}

func TestTranslateInitializeControlIgnored(t *testing.T) {
	if _, ok := mustTranslate(t, `{"type":"control_request","request_id":"x","request":{"subtype":"initialize"}}`); ok {
		t.Fatal("initialize 控制请求不应下发")
	}
}

func TestTranslateAssistantTextAndToolUse(t *testing.T) {
	ev, ok := mustTranslate(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"我来看看"},{"type":"tool_use","name":"Bash","input":{"command":"df -h"}}]}}`)
	if !ok {
		t.Fatal("应翻译为事件")
	}
	blocks, _ := ev["blocks"].([]map[string]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%+v", blocks)
	}
	if blocks[0]["kind"] != "text" || blocks[0]["text"] != "我来看看" {
		t.Fatalf("block0=%+v", blocks[0])
	}
	if blocks[1]["kind"] != "tool_use" || blocks[1]["name"] != "Bash" {
		t.Fatalf("block1=%+v", blocks[1])
	}
}

func TestTranslateAssistantEmptyReturnsFalse(t *testing.T) {
	if _, ok := mustTranslate(t, `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"x"}]}}`); ok {
		t.Fatal("纯 thinking 不应下发")
	}
}

func TestTranslateToolResult(t *testing.T) {
	ev, ok := mustTranslate(t, `{"type":"user","message":{"content":[{"type":"tool_result","content":"Filesystem Size","is_error":false}]}}`)
	if !ok {
		t.Fatal("应翻译为事件")
	}
	results, _ := ev["results"].([]map[string]any)
	if len(results) != 1 || results[0]["content"] != "Filesystem Size" {
		t.Fatalf("results=%+v", results)
	}
}

func TestTranslateResult(t *testing.T) {
	ev, ok := mustTranslate(t, `{"type":"result","subtype":"success","is_error":false,"result":"完成"}`)
	if !ok || ev["type"] != "result" || ev["result"] != "完成" || boolOf(ev["is_error"]) {
		t.Fatalf("result 翻译错误: %+v", ev)
	}
}

func TestTranslateSystemInitToReady(t *testing.T) {
	ev, ok := mustTranslate(t, `{"type":"system","subtype":"init","cwd":"/root"}`)
	if !ok || ev["type"] != "ready" || ev["cwd"] != "/root" {
		t.Fatalf("ready 翻译错误: %+v", ev)
	}
}

func TestTranslateUnknownReturnsFalse(t *testing.T) {
	if _, ok := mustTranslate(t, `{"type":"system","subtype":"api_retry"}`); ok {
		t.Fatal("api_retry 不应下发")
	}
	if _, ok := mustTranslate(t, `not json`); ok {
		t.Fatal("非法 JSON 不应下发")
	}
}

func TestStringifyContent(t *testing.T) {
	if stringifyContent(nil) != "" {
		t.Fatal("nil")
	}
	if stringifyContent("hello") != "hello" {
		t.Fatal("string")
	}
	var blocks []any
	_ = json.Unmarshal([]byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`), &blocks)
	if got := stringifyContent(blocks); got != "a\nb" {
		t.Fatalf("list=%q", got)
	}
}
