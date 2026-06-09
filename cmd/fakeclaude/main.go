// fakeclaude 模拟 claude code CLI 的 stream-json 控制协议，仅供测试使用。
// 流程：init → 收到 user 消息 → 回 assistant(text+tool_use) + 权限请求 →
// 收到 allow → 回 tool_result + result；收到 deny → 回 result(已拒绝)。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

func emit(o map[string]any) {
	b, _ := json.Marshal(o)
	fmt.Println(string(b))
}

func main() {
	emit(map[string]any{"type": "system", "subtype": "init", "cwd": "/fake/dir", "tools": []string{"Bash"}})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		switch msg["type"] {
		case "control_request":
			req, _ := msg["request"].(map[string]any)
			switch req["subtype"] {
			case "initialize":
				emit(map[string]any{"type": "control_response", "response": map[string]any{
					"subtype": "success", "request_id": msg["request_id"], "response": map[string]any{},
				}})
			case "interrupt":
				emit(map[string]any{"type": "result", "subtype": "interrupted", "is_error": false, "result": "已中断"})
			}
		case "user":
			emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "On it — let me run that for you."},
				map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "echo hi"}},
			}}})
			emit(map[string]any{"type": "control_request", "request_id": "perm_1", "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "echo hi"},
			}})
		case "control_response":
			resp, _ := msg["response"].(map[string]any)
			inner, _ := resp["response"].(map[string]any)
			if inner["behavior"] == "allow" {
				emit(map[string]any{"type": "user", "message": map[string]any{"content": []any{
					map[string]any{"type": "tool_result", "content": "hi", "is_error": false},
				}}})
				emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "完成"})
			} else {
				emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "已拒绝"})
			}
		}
	}
}
