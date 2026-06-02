package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakeClaudePath 指向测试期编译出的假 claude 二进制。
var fakeClaudePath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeclaude")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	fakeClaudePath = filepath.Join(dir, "fakeclaude")
	build := exec.Command("go", "build", "-o", fakeClaudePath, "./cmd/fakeclaude")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic("构建 fakeclaude 失败: " + err.Error())
	}
	os.Exit(m.Run())
}

func collectUntil(t *testing.T, ch <-chan map[string]any, target string, max int) []map[string]any {
	t.Helper()
	var events []map[string]any
	for i := 0; i < max; i++ {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
			if ev["type"] == target {
				return events
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("等待 %q 超时，已收到: %+v", target, events)
		}
	}
	return events
}

func findEvent(events []map[string]any, typ string) map[string]any {
	for _, e := range events {
		if e["type"] == typ {
			return e
		}
	}
	return nil
}

func TestBridgeFullPermissionRoundtrip(t *testing.T) {
	b := NewBridge(Config{ClaudeBin: fakeClaudePath, WorkDir: "/tmp"})
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	// 1) ready
	ev := <-b.Events()
	if ev["type"] != "ready" || ev["cwd"] != "/fake/dir" {
		t.Fatalf("首个事件应为 ready: %+v", ev)
	}

	// 2) 发用户消息 → assistant + permission_request
	if err := b.SendUserMessage("帮我执行 echo hi"); err != nil {
		t.Fatalf("SendUserMessage: %v", err)
	}
	events := collectUntil(t, b.Events(), "permission_request", 10)
	if findEvent(events, "assistant") == nil {
		t.Fatalf("缺少 assistant 事件: %+v", events)
	}
	perm := findEvent(events, "permission_request")
	if perm == nil || perm["tool_name"] != "Bash" || perm["request_id"] != "perm_1" {
		t.Fatalf("权限请求错误: %+v", perm)
	}

	// 3) 放行 → tool_result + result
	if err := b.RespondPermission("perm_1", true, map[string]any{"command": "echo hi"}); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	events = collectUntil(t, b.Events(), "result", 10)
	tr := findEvent(events, "tool_result")
	if tr == nil {
		t.Fatalf("缺少 tool_result: %+v", events)
	}
	res := findEvent(events, "result")
	if res == nil || boolOf(res["is_error"]) || res["result"] != "完成" {
		t.Fatalf("result 错误: %+v", res)
	}
}

func TestBridgeDenyPermission(t *testing.T) {
	b := NewBridge(Config{ClaudeBin: fakeClaudePath, WorkDir: "/tmp"})
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	if ev := <-b.Events(); ev["type"] != "ready" {
		t.Fatalf("应为 ready: %+v", ev)
	}
	_ = b.SendUserMessage("执行点啥")
	collectUntil(t, b.Events(), "permission_request", 10)
	_ = b.RespondPermission("perm_1", false, nil)
	events := collectUntil(t, b.Events(), "result", 10)
	res := findEvent(events, "result")
	if res == nil || res["result"] != "已拒绝" {
		t.Fatalf("拒绝结果错误: %+v", res)
	}
}
