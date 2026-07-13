package bridge

import (
	"testing"

	"claude-agent/internal/config"
)

func TestBuildArgsLoadsProjectSettingSources(t *testing.T) {
	b := NewBridge(config.Config{ClaudeBin: "claude"})
	args := b.buildArgs()
	for i := range args {
		if args[i] == "--setting-sources" && i+1 < len(args) && args[i+1] == "user,project,local" {
			return
		}
	}
	t.Fatalf("缺少项目 Skills 所需的 --setting-sources 参数: %#v", args)
}
