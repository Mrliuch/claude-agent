package main

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
)

// Bridge 驱动本机 claude code CLI 的 stream-json 双向控制协议。
// 一个 WebSocket 连接对应一个 Bridge（= 一个 claude 子进程 = 一轮对话）。
type Bridge struct {
	cfg    Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan map[string]any

	writeMu    sync.Mutex
	closeOnce  sync.Once
	reqCounter int
}

func NewBridge(cfg Config) *Bridge {
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
	b.cmd.Dir = b.cfg.resolvedWorkDir()
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
		// 关键：让 CLI 把权限请求经 stdio 控制协议发出（can_use_tool control_request），
		// 否则需授权的操作会被直接当"未授权"拒绝，不会触发我们的权限弹窗。
		"--permission-prompt-tool", "stdio",
	}
	if b.cfg.Model != "" {
		args = append(args, "--model", b.cfg.Model)
	}
	if b.cfg.SessionID != "" {
		// 续接历史会话，恢复 claude 上下文
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
				ev, ok := translate([]byte(trimmed))
				if debug {
					head := trimmed
					if len(head) > 120 {
						head = head[:120]
					}
					log.Printf("[bridge] read(forward=%v): %s", ok, head)
				}
				if ok {
					b.events <- ev
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
	// 回收子进程，避免被 Kill 后残留僵尸进程(defunct)占用 PID 表
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
