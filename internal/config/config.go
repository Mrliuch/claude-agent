package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

// Config 来自命令行参数/环境变量的 agent 配置。
// 优先级：--flag > 环境变量 > 内置默认值。
type Config struct {
	ListenAddr     string // 监听地址，如 :8765
	Token          string // 共享鉴权 token，客户端（Web 控制台/上游中继）需携带
	ClaudeBin      string // 本机 claude code CLI 路径/命令
	Model          string // 模型，留空用 claude 默认
	WorkDir        string // claude 工作目录，留空回退到运行用户 HOME
	PermissionMode string // default=危险操作走权限确认
	SessionID      string // 非空则以 --resume 续接该 claude 会话（按连接设置，不来自 env）
	IdleTimeoutSec int    // 空闲多少秒无用户消息则结束会话(回收claude)；0=禁用，默认1800
	UIEnabled      bool   // 是否在 / 提供内置 Web 控制台（AGENT_UI=off 关闭）
	Version        string // 版本号，由 main.go 注入，不来自 env/flag

	// 微信 ClawBot 接入通道（默认关闭，不影响原有功能）。
	WeChatEnabled     bool   // AGENT_WECHAT=on 时启用微信通道
	WeChatTokenPath   string // bot_token 持久化路径，留空用 ~/.config/claude-agent/wechat_token
	WeChatBaseURL     string // iLink 接入域名，留空用官方默认；用于测试 mock
	WeChatMaxSessions int    // 并发微信会话上限（每会话=1 个 claude 子进程），默认 20

	// 以下为「按连接」注入的字段，不来自 env，由调用方按请求写入。
	ClaudeAuthToken        string // 用户私有 ANTHROPIC_AUTH_TOKEN，非空时经 --settings 注入
	ClaudeBaseURL          string // 用户私有 ANTHROPIC_BASE_URL，留空走宿主配置或官方端点
	DisableBackgroundTasks bool   // 禁用 Bash run_in_background（CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1）
}

// paramDoc 记录每个可配置参数，用于 --help 格式化输出。
type paramDoc struct {
	flagName string
	envVar   string
	defVal   string
	desc     string
}

var paramDocs = []paramDoc{
	{"addr", "AGENT_LISTEN_ADDR", ":8765", "监听地址（host:port）"},
	{"token", "AGENT_TOKEN", "", "共享鉴权 token，客户端需携带（必填）"},
	{"claude-bin", "CLAUDE_BIN", "claude", "本机 claude CLI 路径或命令名"},
	{"model", "CLAUDE_MODEL", "", "claude 模型，留空使用 claude 默认"},
	{"work-dir", "CLAUDE_WORK_DIR", "", "claude 工作目录，留空回退到运行用户 HOME"},
	{"permission-mode", "CLAUDE_PERMISSION_MODE", "default", "权限模式：default=危险操作弹窗确认"},
	{"idle-timeout", "CLAUDE_IDLE_TIMEOUT", "1800", "空闲超时（秒），超时后回收 claude 进程；0=禁用"},
	{"ui", "AGENT_UI", "on", "内置 Web 控制台开关（on/off）"},
	{"disable-bg-tasks", "CLAUDE_DISABLE_BACKGROUND_TASKS", "off", "禁用 Bash 后台任务 run_in_background（on/off）"},
	{"wechat", "AGENT_WECHAT", "off", "微信 ClawBot 通道开关（on/off）"},
	{"wechat-token-path", "AGENT_WECHAT_TOKEN_PATH", "", "微信 bot_token 持久化路径，留空用默认"},
	{"wechat-baseurl", "AGENT_WECHAT_BASEURL", "", "微信 iLink 域名，留空用官方默认"},
	{"wechat-max-sessions", "AGENT_WECHAT_MAX_SESSIONS", "20", "微信并发会话上限（每会话一个 claude 进程）"},
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "用法: claude-agent [选项]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "命令行参数（--flag）优先于同名环境变量。")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "选项:")
	fmt.Fprintf(w, "  %-34s  %s\n", "--version, -v", "显示版本号并退出")
	fmt.Fprintf(w, "  %-34s  %s\n", "--help, -h", "显示此帮助并退出")
	fmt.Fprintln(w, "")
	for _, p := range paramDocs {
		col := fmt.Sprintf("--%s <值>", p.flagName)
		envPart := fmt.Sprintf("  [环境变量: %s]", p.envVar)
		defPart := ""
		if p.defVal != "" {
			defPart = fmt.Sprintf("  默认: %s", p.defVal)
		}
		fmt.Fprintf(w, "  %-34s  %s%s%s\n", col, p.desc, defPart, envPart)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "示例:")
	fmt.Fprintln(w, "  claude-agent --addr :9000 --token mytoken --work-dir /home/user/project")
	fmt.Fprintln(w, "  AGENT_TOKEN=mytoken claude-agent --idle-timeout 3600 --ui off")
}

// ParseArgs 解析命令行参数，命令行优先于环境变量。
// version 由 main.go 通过 -ldflags 注入。
// 返回 (Config, true) 表示正常启动；返回 (_, false) 表示已处理 --help/--version，调用方应 os.Exit(0)。
func ParseArgs(args []string, version string) (Config, bool) {
	for _, a := range args {
		switch a {
		case "--version", "-v", "-version":
			fmt.Fprintf(os.Stderr, "claude-agent %s\n", version)
			return Config{}, false
		case "--help", "-h", "-help":
			printHelp(os.Stderr)
			return Config{}, false
		}
	}

	fs := flag.NewFlagSet("claude-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() { printHelp(os.Stderr) }

	// 所有 flag 默认值为 ""，解析后与 env 合并：flag > env > 内置默认
	fAddr := fs.String("addr", "", "")
	fToken := fs.String("token", "", "")
	fClaudeBin := fs.String("claude-bin", "", "")
	fModel := fs.String("model", "", "")
	fWorkDir := fs.String("work-dir", "", "")
	fPermMode := fs.String("permission-mode", "", "")
	fIdleTimeout := fs.String("idle-timeout", "", "")
	fUI := fs.String("ui", "", "")
	fDisableBg := fs.String("disable-bg-tasks", "", "")
	fWechat := fs.String("wechat", "", "")
	fWechatPath := fs.String("wechat-token-path", "", "")
	fWechatBaseURL := fs.String("wechat-baseurl", "", "")
	fWechatMaxSess := fs.String("wechat-max-sessions", "", "")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n\n", err)
		printHelp(os.Stderr)
		return Config{}, false
	}

	return Config{
		ListenAddr:     firstNonEmpty(*fAddr, envOr("AGENT_LISTEN_ADDR", ":8765")),
		Token:          firstNonEmpty(*fToken, os.Getenv("AGENT_TOKEN")),
		ClaudeBin:      firstNonEmpty(*fClaudeBin, envOr("CLAUDE_BIN", "claude")),
		Model:          firstNonEmpty(*fModel, os.Getenv("CLAUDE_MODEL")),
		WorkDir:        firstNonEmpty(*fWorkDir, os.Getenv("CLAUDE_WORK_DIR")),
		PermissionMode: firstNonEmpty(*fPermMode, envOr("CLAUDE_PERMISSION_MODE", "default")),
		IdleTimeoutSec: parseIntFlag(*fIdleTimeout, envInt("CLAUDE_IDLE_TIMEOUT", 1800)),
		UIEnabled:      firstNonEmpty(*fUI, envOr("AGENT_UI", "on")) != "off",
		Version:        version,

		WeChatEnabled:     firstNonEmpty(*fWechat, envOr("AGENT_WECHAT", "off")) == "on",
		WeChatTokenPath:   firstNonEmpty(*fWechatPath, os.Getenv("AGENT_WECHAT_TOKEN_PATH")),
		WeChatBaseURL:     firstNonEmpty(*fWechatBaseURL, os.Getenv("AGENT_WECHAT_BASEURL")),
		WeChatMaxSessions: parseIntFlag(*fWechatMaxSess, envInt("AGENT_WECHAT_MAX_SESSIONS", 20)),

		DisableBackgroundTasks: firstNonEmpty(*fDisableBg, envOr("CLAUDE_DISABLE_BACKGROUND_TASKS", "off")) == "on",
	}, true
}

// LoadConfig 仅从环境变量读取配置，不解析命令行参数（供测试和嵌入场景使用）。
func LoadConfig() Config {
	cfg, _ := ParseArgs(nil, "")
	return cfg
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseIntFlag(flagVal string, envDefault int) int {
	if flagVal != "" {
		if n, err := strconv.Atoi(flagVal); err == nil {
			return n
		}
	}
	return envDefault
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ResolvedWorkDir 空则回退到运行用户 HOME，保证 cwd 一定存在。
func (c Config) ResolvedWorkDir() string {
	if c.WorkDir != "" {
		if info, err := os.Stat(c.WorkDir); err == nil && info.IsDir() {
			return c.WorkDir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/"
	}
	return home
}
