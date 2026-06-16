package wechat

import (
	"os"
	"path/filepath"
	"strings"
)

// loadToken 从文件读取已保存的 bot_token;不存在或为空返回 ""。
func loadToken(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveToken 持久化 bot_token,文件权限 0600;父目录自动创建。
func saveToken(path, token string) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(token), 0o600)
}

// defaultTokenPath 返回默认 token 路径 ~/.config/claude-agent/wechat_token。
func defaultTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "claude-agent", "wechat_token")
}
