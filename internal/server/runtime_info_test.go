package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"claude-agent/internal/config"
)

func TestClaudeRuntimeInfoDetectsProjectSkillSupport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("测试脚本使用 POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 2.1.200; else echo '  --setting-sources'; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewServer(config.Config{ClaudeBin: path})
	version, supported := s.claudeRuntimeInfo()
	if version != "2.1.200" || !supported {
		t.Fatalf("运行能力探测错误: version=%q supported=%v", version, supported)
	}
}
