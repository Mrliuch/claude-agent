package server

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveWorkSubdir：合法子目录返回其绝对路径，空/越界/非目录返回 ""。
func TestResolveWorkSubdir(t *testing.T) {
	s, root := newFsServer(t)
	sub := filepath.Join(root, "proj", "svc")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建子目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("建文件失败: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串回退根", "", ""},
		{"合法子目录", "proj/svc", sub},
		{"越界被忽略", "../../etc", ""},
		{"不存在目录", "nope/x", ""},
		{"指向文件非目录", "a.txt", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.resolveWorkSubdir(c.in); got != c.want {
				t.Fatalf("resolveWorkSubdir(%q)=%q, want %q", c.in, got, c.want)
			}
		})
	}
}
