package main

import "encoding/json"

// translate 把 claude CLI 的 stream-json 原生消息翻译成对前端友好的事件。
// 返回 (事件, true) 表示需要下发；(nil, false) 表示该消息无需下发（如 hook/初始化响应）。
//
// 协议字段取自官方 claude-agent-sdk 源码核准：
//   - 权限请求：control_request{subtype:can_use_tool, tool_name, input, ...}
//   - assistant：message.content[] 含 text / tool_use / thinking
//   - tool_result：user.message.content[]{type:tool_result, content, is_error}
//   - result：本轮结束汇总
//   - system/init：会话就绪（含 cwd）
func translate(raw []byte) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	mtype, _ := m["type"].(string)

	switch mtype {
	case "control_request":
		req, _ := m["request"].(map[string]any)
		if sub, _ := req["subtype"].(string); sub == "can_use_tool" {
			return map[string]any{
				"type":        "permission_request",
				"request_id":  m["request_id"],
				"tool_name":   strOr(req["tool_name"], ""),
				"tool_input":  req["input"],
				"title":       strOr(req["title"], ""),
				"description": strOr(req["description"], ""),
			}, true
		}
		return nil, false

	case "assistant":
		blocks := extractAssistantBlocks(m)
		if len(blocks) == 0 {
			return nil, false
		}
		return map[string]any{"type": "assistant", "blocks": blocks}, true

	case "user":
		results := extractToolResults(m)
		if len(results) == 0 {
			return nil, false
		}
		return map[string]any{"type": "tool_result", "results": results}, true

	case "result":
		return map[string]any{
			"type":           "result",
			"subtype":        strOr(m["subtype"], ""),
			"is_error":       boolOf(m["is_error"]),
			"duration_ms":    m["duration_ms"],
			"total_cost_usd": m["total_cost_usd"],
			"result":         strOr(m["result"], ""),
		}, true

	case "system":
		if sub, _ := m["subtype"].(string); sub == "init" {
			return map[string]any{
				"type":       "ready",
				"cwd":        strOr(m["cwd"], ""),
				"session_id": strOr(m["session_id"], ""),
			}, true
		}
		return nil, false
	}
	return nil, false
}

func extractAssistantBlocks(m map[string]any) []map[string]any {
	msg, _ := m["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var blocks []map[string]any
	for _, c := range content {
		block, _ := c.(map[string]any)
		switch block["type"] {
		case "text":
			if txt := strOr(block["text"], ""); txt != "" {
				blocks = append(blocks, map[string]any{"kind": "text", "text": txt})
			}
		case "tool_use":
			blocks = append(blocks, map[string]any{
				"kind":  "tool_use",
				"name":  strOr(block["name"], ""),
				"input": block["input"],
			})
		}
	}
	return blocks
}

func extractToolResults(m map[string]any) []map[string]any {
	msg, _ := m["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var results []map[string]any
	for _, c := range content {
		block, _ := c.(map[string]any)
		if block["type"] == "tool_result" {
			results = append(results, map[string]any{
				"content":  stringifyContent(block["content"]),
				"is_error": boolOf(block["is_error"]),
			})
		}
	}
	return results
}

// stringifyContent 把 tool_result 的 content（可能是 string / []block / map）统一成字符串。
func stringifyContent(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var s string
		for i, item := range c {
			if i > 0 {
				s += "\n"
			}
			if blk, ok := item.(map[string]any); ok {
				if txt := strOr(blk["text"], ""); txt != "" {
					s += txt
					continue
				}
			}
			b, _ := json.Marshal(item)
			s += string(b)
		}
		return s
	case map[string]any:
		if txt := strOr(c["text"], ""); txt != "" {
			return txt
		}
		b, _ := json.Marshal(c)
		return string(b)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}
