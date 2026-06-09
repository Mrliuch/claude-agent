package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"claude-agent/internal/config"
	"claude-agent/internal/protocol"
)

// Bridge 驱动本机 claude code CLI 的 stream-json 双向控制协议。
// 一个 WebSocket 连接对应一个 Bridge（= 一个 claude 子进程 = 一轮对话）。
type Bridge struct {
	cfg    config.Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan map[string]any

	writeMu    sync.Mutex
	closeOnce  sync.Once
	reqCounter int

	// AskUserQuestion：缓存原始 questions（control_request 阶段）与用户答案（question_response 阶段）
	askMu        sync.Mutex
	askQuestions []any
	askAnswers   map[string]any
}

func NewBridge(cfg config.Config) *Bridge {
	return &Bridge{cfg: cfg, events: make(chan map[string]any, 64)}
}

// Events 返回事件通道，调用方 range 消费，通道关闭表示会话结束。
func (b *Bridge) Events() <-chan map[string]any {
	return b.events
}

// Start 拉起 claude 子进程并发送 initialize 握手。
func (b *Bridge) Start() error {
	args := b.buildArgs()
	b.cmd = exec.Command(args[0], args[1:]...)
	b.cmd.Dir = b.cfg.ResolvedWorkDir()
	b.cmd.Env = filteredEnv()

	stdin, err := b.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := b.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := b.cmd.StderrPipe()
	if err != nil {
		return err
	}
	b.stdin = stdin

	if err := b.cmd.Start(); err != nil {
		return fmt.Errorf("启动 claude 失败: %w", err)
	}

	go b.readLoop(stdout, stderr)
	b.sendInitialize()
	return nil
}

func (b *Bridge) buildArgs() []string {
	args := []string{
		b.cfg.ClaudeBin,
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", orDefault(b.cfg.PermissionMode, "default"),
		"--permission-prompt-tool", "stdio",
	}
	if b.cfg.Model != "" {
		args = append(args, "--model", b.cfg.Model)
	}
	if b.cfg.SessionID != "" {
		args = append(args, "--resume", b.cfg.SessionID)
	}
	return args
}

// SendUserMessage 向 claude 发送一条用户消息。
func (b *Bridge) SendUserMessage(text string) error {
	return b.write(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
}

// RespondPermission 回应一次权限请求。
func (b *Bridge) RespondPermission(requestID string, allow bool, updatedInput any) error {
	var inner map[string]any
	if allow {
		if updatedInput == nil {
			updatedInput = map[string]any{}
		}
		inner = map[string]any{"behavior": "allow", "updatedInput": updatedInput}
	} else {
		inner = map[string]any{"behavior": "deny", "message": "用户拒绝执行该操作"}
	}
	return b.write(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   inner,
		},
	})
}

// SendToolResult 向 claude 注入一条 tool_result，用于响应需要 tool_result 回路的工具。
func (b *Bridge) SendToolResult(toolUseID string, content string) error {
	return b.write(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
				},
			},
		},
	})
}

// RespondAskUserQuestion 批准 AskUserQuestion 并把答案注入 updatedInput。
// updatedInput 必须同时包含：
//   - questions: 原始问题数组（Claude Code 内部调用 questions.map()，缺失会 crash）
//   - answers: {问题文本: 答案} 对象（Claude Code 从这里读取每题的回答）
//   - annotations: {} 附加注释（可为空对象，不可缺失）
func (b *Bridge) RespondAskUserQuestion(requestID string, answers map[string]any) error {
	b.askMu.Lock()
	questions := b.askQuestions
	b.askAnswers = answers
	b.askMu.Unlock()

	annotations := make(map[string]any, len(answers))
	for k := range answers {
		annotations[k] = map[string]any{"notes": ""}
	}

	return b.write(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response": map[string]any{
				"behavior": "allow",
				"updatedInput": map[string]any{
					"questions":   questions,
					"answers":     answers,
					"annotations": annotations,
				},
			},
		},
	})
}

// Interrupt 中断 claude 当前轮次，但保留会话上下文（不杀进程，可继续对话）。
// 发送标准 stream-json 控制帧 control_request{subtype:interrupt}。
func (b *Bridge) Interrupt() error {
	b.reqCounter++
	return b.write(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("interrupt_%d", b.reqCounter),
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

// processEvents 下发前处理：
// - user_question：缓存原始 questions 供 RespondAskUserQuestion 使用
// - assistant 块中 AskUserQuestion tool_use：跳过展示（用户已看到问卷卡片）
func (b *Bridge) processEvents(ev map[string]any) []map[string]any {
	if ev["type"] == "user_question" {
		if qs, ok := ev["questions"].([]any); ok && len(qs) > 0 {
			b.askMu.Lock()
			b.askQuestions = qs
			b.askMu.Unlock()
		}
		return []map[string]any{ev}
	}

	if ev["type"] != "assistant" {
		return []map[string]any{ev}
	}
	blocks, _ := ev["blocks"].([]map[string]any)
	var regular []map[string]any
	for _, block := range blocks {
		if block["kind"] == "tool_use" && block["name"] == "AskUserQuestion" {
			continue // 不推送到前端，用户已看到问卷卡片
		}
		regular = append(regular, block)
	}
	if len(regular) == 0 {
		return nil
	}
	return []map[string]any{{"type": "assistant", "blocks": regular}}
}

func (b *Bridge) sendInitialize() {
	b.reqCounter++
	_ = b.write(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("init_%d", b.reqCounter),
		"request":    map[string]any{"subtype": "initialize", "hooks": nil},
	})
}

func (b *Bridge) write(obj map[string]any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.stdin == nil {
		return fmt.Errorf("claude 进程未启动")
	}
	_, err = b.stdin.Write(append(data, '\n'))
	return err
}

// readLoop 持续读取 claude stdout，翻译后投递事件；进程退出时投递 closed 并关闭通道。
func (b *Bridge) readLoop(stdout, stderr io.Reader) {
	reader := bufio.NewReaderSize(stdout, 1024*1024)
	debug := os.Getenv("AGENT_DEBUG") != ""
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				ev, ok := protocol.Translate([]byte(trimmed))
				if debug {
					head := trimmed
					if len(head) > 120 {
						head = head[:120]
					}
					log.Printf("[bridge] read(forward=%v): %s", ok, head)
				}
				if ok {
					for _, e := range b.processEvents(ev) {
						b.events <- e
					}
				}
			}
		}
		if err != nil {
			if debug {
				log.Printf("[bridge] stdout 读取结束: %v", err)
			}
			break
		}
	}
	errMsg := readAllString(stderr)
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000]
	}
	if b.cmd != nil {
		_ = b.cmd.Wait()
	}
	b.events <- map[string]any{"type": "closed", "stderr": strings.TrimSpace(errMsg)}
	close(b.events)
}

// Close 终止子进程并关闭 stdin；readLoop 会因 EOF 自然结束。
func (b *Bridge) Close() {
	b.closeOnce.Do(func() {
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
		if b.stdin != nil {
			_ = b.stdin.Close()
		}
	})
}

func filteredEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDECODE=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "CLAUDE_CODE_ENTRYPOINT=claude-agent")
	return out
}

func readAllString(r io.Reader) string {
	if r == nil {
		return ""
	}
	data, _ := io.ReadAll(r)
	return string(data)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
