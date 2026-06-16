package config

import (
	"os"
	"strconv"
)

// Config 来自环境变量的 agent 配置。
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

	// 微信 ClawBot 接入通道（默认关闭，不影响原有功能）。
	WeChatEnabled     bool   // AGENT_WECHAT=on 时启用微信通道
	WeChatTokenPath   string // bot_token 持久化路径，留空用 ~/.config/claude-agent/wechat_token
	WeChatBaseURL     string // iLink 接入域名，留空用官方默认；用于测试 mock
	WeChatMaxSessions int    // 并发微信会话上限（每会话=1 个 claude 子进程），默认 20
}

// LoadConfig 从环境变量读取配置并填充默认值。
func LoadConfig() Config {
	return Config{
		ListenAddr:     envOr("AGENT_LISTEN_ADDR", ":8765"),
		Token:          os.Getenv("AGENT_TOKEN"),
		ClaudeBin:      envOr("CLAUDE_BIN", "claude"),
		Model:          os.Getenv("CLAUDE_MODEL"),
		WorkDir:        os.Getenv("CLAUDE_WORK_DIR"),
		PermissionMode: envOr("CLAUDE_PERMISSION_MODE", "default"),
		IdleTimeoutSec: envInt("CLAUDE_IDLE_TIMEOUT", 1800),
		UIEnabled:      envOr("AGENT_UI", "on") != "off",

		WeChatEnabled:     envOr("AGENT_WECHAT", "off") == "on",
		WeChatTokenPath:   os.Getenv("AGENT_WECHAT_TOKEN_PATH"),
		WeChatBaseURL:     os.Getenv("AGENT_WECHAT_BASEURL"),
		WeChatMaxSessions: envInt("AGENT_WECHAT_MAX_SESSIONS", 20),
	}
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
