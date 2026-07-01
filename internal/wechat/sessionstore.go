package wechat

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// session_id 持久化:每个微信用户一条 claude 会话的 session_id 落盘,
// 使 agent 重启 / claude 进程被回收后仍能用 --resume 续接同一上下文。
//
// 文件名用 userID 的 sha256(userID 含 @ 等特殊字符,不能直接做文件名);
// 存放在 token 同级的 sessions/ 子目录,权限 0600。

// sessionFileName 把 userID 映射为稳定、安全的文件名。
func sessionFileName(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(sum[:]) + ".sid"
}

// loadSessionID 读取某用户已保存的 claude session_id;dir 为空、文件不存在或为空返回 ""。
func loadSessionID(dir, userID string) string {
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, sessionFileName(userID)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveSessionID 持久化某用户的 claude session_id;dir 或 sid 为空则不落盘(no-op)。
func saveSessionID(dir, userID, sid string) error {
	if dir == "" || sid == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sessionFileName(userID)), []byte(sid), 0o600)
}

// clearSessionID 删除某用户已保存的 session_id(resume 失败降级为新会话时调用)。
func clearSessionID(dir, userID string) {
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, sessionFileName(userID)))
}
