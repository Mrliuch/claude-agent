package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"claude-agent/internal/config"
)

// 公司/私有网关专用的模型映射键：当用户改用自己的 base_url 时应丢弃，
// 避免把网关私有模型名强加到一个陌生端点上。
var gatewayModelKeys = []string{
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
}

// writeUserMCPConfig 将平台已鉴权的用户 MCP 生成 Claude CLI 可读取的临时配置。
// 结构兼容 Claude Code 的 .mcp.json；文件 0600 且连接结束立即删除。
func writeUserMCPConfig(raw string) (string, error) {
	var rows []struct {
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		Endpoint  string            `json:"endpoint_or_command"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
		Headers   map[string]string `json:"headers"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return "", err
	}
	servers := map[string]any{}
	for _, row := range rows {
		name := strings.TrimSpace(row.Name)
		endpoint := strings.TrimSpace(row.Endpoint)
		if name == "" || endpoint == "" {
			continue
		}
		if row.Transport == "stdio" {
			servers[name] = map[string]any{"command": endpoint, "args": row.Args, "env": row.Env}
		} else {
			servers[name] = map[string]any{"type": "http", "url": endpoint, "headers": row.Headers}
		}
	}
	payload, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "claude-agent-mcp-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_ = f.Chmod(0o600)
	if _, err = f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// hostSettingsEnv 读取宿主 claude 配置（CLAUDE_CONFIG_DIR 或 ~/.claude）下
// settings.json 的 env 块，作为用户凭据的基底（保住网关地址、模型映射、各类
// CLAUDE_CODE_* 开关）。读不到则返回空 map（退化为仅用用户覆盖项）。
func hostSettingsEnv() map[string]string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return map[string]string{}
		}
		dir = filepath.Join(home, ".claude")
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return map[string]string{}
	}
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Env == nil {
		return map[string]string{}
	}
	return parsed.Env
}

// buildUserSettingsEnv 在宿主 env 基底上叠加当前连接的用户私有凭据：
//   - 始终覆盖 ANTHROPIC_AUTH_TOKEN 为用户 token
//   - 用户给了 base_url：覆盖 ANTHROPIC_BASE_URL，并丢弃网关专用模型映射
//   - 始终清理宿主全局 CloudScope 任务归档凭据；仅同时收到本次握手的 URL/Token 时注入
//
// 返回最终的 env map，不修改入参基底。
func buildUserSettingsEnv(cfg config.Config, base map[string]string) map[string]string {
	env := make(map[string]string, len(base)+2)
	for k, v := range base {
		env[k] = v
	}
	delete(env, "CLOUDSCOPE_TASK_AUDIT_URL")
	delete(env, "CLOUDSCOPE_TASK_AUDIT_TOKEN")
	if cfg.ClaudeAuthToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = cfg.ClaudeAuthToken
	}
	if cfg.ClaudeBaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = cfg.ClaudeBaseURL
		for _, k := range gatewayModelKeys {
			delete(env, k)
		}
	}
	if cfg.TaskAuditURL != "" && cfg.TaskAuditToken != "" {
		env["CLOUDSCOPE_TASK_AUDIT_URL"] = cfg.TaskAuditURL
		env["CLOUDSCOPE_TASK_AUDIT_TOKEN"] = cfg.TaskAuditToken
	}
	return env
}

// writeUserSettings 把当前连接的用户凭据和归档凭据写成临时 --settings 文件（0600），返回路径。
// 调用方（Bridge.Close）负责删除。
//
// 为什么不用进程 env：claude 的 settings.json env 块会完全压制进程环境变量（真机核实），
// 只有 --settings（优先级高于用户 settings.json）才能让用户 token 生效。
func writeUserSettings(cfg config.Config) (string, error) {
	env := buildUserSettingsEnv(cfg, hostSettingsEnv())
	payload, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "claude-agent-settings-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_ = f.Chmod(0o600)
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
