package server

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
)

//go:embed assets/cloudscope-wecom-send/SKILL.md
var cloudscopeWecomSendSkill []byte

// installBundledSystemSkills 将平台托管 Skill 安装到 Claude Code 的系统目录。
// 它不含企业微信凭据；实际 URL/短令牌仅在每个连接临时 settings 中下发。
func installBundledSystemSkills() {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	target := filepath.Join(home, ".claude", "skills", "cloudscope-wecom-send", "SKILL.md")
	if current, err := os.ReadFile(target); err == nil && string(current) == string(cloudscopeWecomSendSkill) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		log.Printf("[claude-agent] 安装企微发送 Skill 失败: %v", err)
		return
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, cloudscopeWecomSendSkill, 0644); err != nil {
		log.Printf("[claude-agent] 写入企微发送 Skill 失败: %v", err)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[claude-agent] 安装企微发送 Skill 失败: %v", err)
	}
}
