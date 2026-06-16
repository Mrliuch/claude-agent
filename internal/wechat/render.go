package wechat

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"claude-agent/internal/protocol"
)

// clip 截断长字符串(按 rune),超出加省略号。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// renderEvent 把一个 claude 事件翻译成发往微信的文本。
// 返回空串表示该事件无需推送(如内部 ready/result/正常 tool_result)。
func renderEvent(ev map[string]any) string {
	switch ev["type"] {
	case "assistant":
		return renderAssistant(ev)
	case "tool_result":
		return renderToolResults(ev)
	case "closed":
		if stderr := strings.TrimSpace(protocol.StrOr(ev["stderr"], "")); stderr != "" {
			return "⚠️ 会话结束: " + clip(stderr, 500)
		}
		return ""
	case "error":
		if msg := protocol.StrOr(ev["msg"], ""); msg != "" {
			return "⚠️ " + msg
		}
		return ""
	default:
		// ready / result / user_question / permission_request 不走这里。
		return ""
	}
}

func renderAssistant(ev map[string]any) string {
	blocks, _ := ev["blocks"].([]map[string]any)
	var parts []string
	for _, b := range blocks {
		switch b["kind"] {
		case "text":
			if t := strings.TrimSpace(protocol.StrOr(b["text"], "")); t != "" {
				parts = append(parts, t)
			}
		case "tool_use":
			parts = append(parts, "🔧 "+toolSummary(protocol.StrOr(b["name"], ""), b["input"]))
		}
	}
	return strings.Join(parts, "\n\n")
}

// renderToolResults 默认安静;仅在出错时回报,避免刷屏。
func renderToolResults(ev map[string]any) string {
	results, _ := ev["results"].([]map[string]any)
	var parts []string
	for _, r := range results {
		if protocol.BoolOf(r["is_error"]) {
			parts = append(parts, "❌ 工具出错: "+clip(protocol.StrOr(r["content"], ""), 300))
		}
	}
	return strings.Join(parts, "\n")
}

// toolSummary 给工具调用一个一行人话摘要。
func toolSummary(name string, input any) string {
	m, _ := input.(map[string]any)
	switch name {
	case "Bash":
		if cmd := protocol.StrOr(m["command"], ""); cmd != "" {
			return "Bash: " + clip(cmd, 200)
		}
	case "Write", "Edit", "Read":
		if fp := protocol.StrOr(m["file_path"], ""); fp != "" {
			return name + ": " + fp
		}
	}
	if m == nil {
		return name
	}
	data, _ := json.Marshal(m)
	return name + ": " + clip(string(data), 200)
}

// renderPermissionPrompt 渲染权限确认卡片。
func renderPermissionPrompt(ev map[string]any) string {
	name := protocol.StrOr(ev["tool_name"], "操作")
	var b strings.Builder
	b.WriteString("⚠️ Claude 请求执行 ")
	b.WriteString(toolSummary(name, ev["tool_input"]))
	if desc := strings.TrimSpace(protocol.StrOr(ev["description"], "")); desc != "" {
		b.WriteString("\n")
		b.WriteString(clip(desc, 300))
	}
	b.WriteString("\n\n回复 y / 允许 放行，n / 拒绝 拒绝")
	return b.String()
}

// renderQuestionPrompt 渲染 AskUserQuestion 选项卡;每题选项从 1 开始编号。
func renderQuestionPrompt(questions []any) string {
	var b strings.Builder
	b.WriteString("❓ Claude 有问题需要你选择：")
	for qi, q := range questions {
		qm, _ := q.(map[string]any)
		b.WriteString("\n\n")
		if len(questions) > 1 {
			b.WriteString(fmt.Sprintf("[%d] ", qi+1))
		}
		b.WriteString(protocol.StrOr(qm["question"], ""))
		for oi, label := range questionOptions(qm) {
			b.WriteString(fmt.Sprintf("\n  %d. %s", oi+1, label))
		}
	}
	if len(questions) == 1 {
		b.WriteString("\n\n回复选项编号，如 1")
	} else {
		b.WriteString("\n\n按题序回复各题编号，用空格或逗号分隔，如 1,2")
	}
	return b.String()
}

// questionOptions 取一道题的选项 label 列表。
func questionOptions(qm map[string]any) []string {
	opts, _ := qm["options"].([]any)
	var labels []string
	for _, o := range opts {
		om, _ := o.(map[string]any)
		labels = append(labels, protocol.StrOr(om["label"], ""))
	}
	return labels
}

// parsePermissionReply 解析用户对权限确认的回复。
// 返回 (allow, ok):ok=false 表示无法识别,调用方应提示重答。
func parsePermissionReply(text string) (allow bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "y", "yes", "允许", "同意", "好", "ok", "可以":
		return true, true
	case "n", "no", "拒绝", "不", "否", "不行":
		return false, true
	default:
		return false, false
	}
}

// parseQuestionReply 解析用户对 AskUserQuestion 的回复,产出 answers{问题文本: 选项label}。
// 每题需恰好一个 1 基编号;数量不匹配或越界则 ok=false。
func parseQuestionReply(text string, questions []any) (map[string]any, bool) {
	tokens := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(tokens) != len(questions) {
		return nil, false
	}
	answers := make(map[string]any, len(questions))
	for i, q := range questions {
		qm, _ := q.(map[string]any)
		idx, err := strconv.Atoi(strings.TrimSpace(tokens[i]))
		if err != nil {
			return nil, false
		}
		labels := questionOptions(qm)
		if idx < 1 || idx > len(labels) {
			return nil, false
		}
		answers[protocol.StrOr(qm["question"], "")] = labels[idx-1]
	}
	return answers, true
}
