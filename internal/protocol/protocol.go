package protocol

import "encoding/json"

// Translate 把 claude CLI 的 stream-json 原生消息翻译成对前端友好的事件。
// 返回 (事件, true) 表示需要下发；(nil, false) 表示该消息无需下发。
func Translate(raw []byte) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	mtype, _ := m["type"].(string)

	switch mtype {
	case "control_request":
		req, _ := m["request"].(map[string]any)
		if sub, _ := req["subtype"].(string); sub == "can_use_tool" {
			toolName := StrOr(req["tool_name"], "")
			// AskUserQuestion：答案必须随 control_response.updatedInput 一起返回，
			// 不走权限弹窗，直接转为 user_question 事件等待用户填写。
			if toolName == "AskUserQuestion" {
				input, _ := req["input"].(map[string]any)
				questions, _ := input["questions"].([]any)
				return map[string]any{
					"type":       "user_question",
					"request_id": m["request_id"],
					"questions":  questions,
				}, true
			}
			return map[string]any{
				"type":        "permission_request",
				"request_id":  m["request_id"],
				"tool_name":   toolName,
				"tool_input":  req["input"],
				"title":       StrOr(req["title"], ""),
				"description": StrOr(req["description"], ""),
			}, true
		}
		return nil, false

	case "assistant":
		blocks := ExtractAssistantBlocks(m)
		if len(blocks) == 0 {
			return nil, false
		}
		return map[string]any{"type": "assistant", "blocks": blocks}, true

	case "user":
		results := ExtractToolResults(m)
		if len(results) == 0 {
			return nil, false
		}
		return map[string]any{"type": "tool_result", "results": results}, true

	case "result":
		return map[string]any{
			"type":           "result",
			"subtype":        StrOr(m["subtype"], ""),
			"is_error":       BoolOf(m["is_error"]),
			"duration_ms":    m["duration_ms"],
			"total_cost_usd": m["total_cost_usd"],
			"num_turns":      m["num_turns"],
			"usage":          m["usage"],
			"result":         StrOr(m["result"], ""),
		}, true

	case "system":
		if sub, _ := m["subtype"].(string); sub == "init" {
			return map[string]any{
				"type":       "ready",
				"cwd":        StrOr(m["cwd"], ""),
				"session_id": StrOr(m["session_id"], ""),
			}, true
		}
		return nil, false
	}
	return nil, false
}

func ExtractAssistantBlocks(m map[string]any) []map[string]any {
	msg, _ := m["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var blocks []map[string]any
	for _, c := range content {
		block, _ := c.(map[string]any)
		switch block["type"] {
		case "text":
			if txt := StrOr(block["text"], ""); txt != "" {
				blocks = append(blocks, map[string]any{"kind": "text", "text": txt})
			}
		case "tool_use":
			blocks = append(blocks, map[string]any{
				"kind":  "tool_use",
				"id":    StrOr(block["id"], ""),
				"name":  StrOr(block["name"], ""),
				"input": block["input"],
			})
		}
	}
	return blocks
}

func ExtractToolResults(m map[string]any) []map[string]any {
	msg, _ := m["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var results []map[string]any
	for _, c := range content {
		block, _ := c.(map[string]any)
		if block["type"] == "tool_result" {
			results = append(results, map[string]any{
				"content":  StringifyContent(block["content"]),
				"is_error": BoolOf(block["is_error"]),
			})
		}
	}
	return results
}

// StringifyContent 把 tool_result 的 content（可能是 string / []block / map）统一成字符串。
func StringifyContent(v any) string {
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
				if txt := StrOr(blk["text"], ""); txt != "" {
					s += txt
					continue
				}
			}
			b, _ := json.Marshal(item)
			s += string(b)
		}
		return s
	case map[string]any:
		if txt := StrOr(c["text"], ""); txt != "" {
			return txt
		}
		b, _ := json.Marshal(c)
		return string(b)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func StrOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func BoolOf(v any) bool {
	b, _ := v.(bool)
	return b
}
