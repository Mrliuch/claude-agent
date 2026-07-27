package server

import (
	"os"
	"strings"
	"testing"
)

func TestValidGitHTTPSURL(t *testing.T) {
	if !validGitHTTPSURL("https://gitlab.example.com/group/project.git") {
		t.Fatal("有效 HTTPS Git URL 被拒绝")
	}
	for _, value := range []string{"http://gitlab.example.com/a.git", "https://token@gitlab.example.com/a.git", "file:///tmp/a"} {
		if validGitHTTPSURL(value) {
			t.Fatalf("不安全 URL 被接受: %s", value)
		}
	}
}

func TestGitArgsRejectsEmptyCommitMessage(t *testing.T) {
	if _, err := gitArgs(gitRunRequest{Action: "commit"}); err == nil {
		t.Fatal("空提交说明应被拒绝")
	}
}

func TestGitCloneUsesWorkspaceLeafInParentDirectory(t *testing.T) {
	args, err := gitArgs(gitRunRequest{Action: "clone", Workspace: "team/project", RepositoryURL: "https://gitlab.example.com/team/project.git"})
	if err != nil {
		t.Fatalf("构造 clone 参数失败: %v", err)
	}
	if got := args[len(args)-1]; got != "project" {
		t.Fatalf("clone 目标应为工作区目录名，实际为 %q", got)
	}
}

func TestGitAskpassUsesTransientEnvironment(t *testing.T) {
	path, cleanup, err := createGitAskpass("token-value")
	if err != nil {
		t.Fatalf("创建 askpass 失败: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("askpass 文件不存在: %v", err)
	}
	joined := strings.Join(gitEnvironment("token-value", path), "\n")
	if !strings.Contains(joined, "GIT_ASKPASS="+path) || !strings.Contains(joined, "GIT_ASKPASS_USERNAME=oauth2") || !strings.Contains(joined, "GIT_ASKPASS_TOKEN=token-value") {
		t.Fatal("Git 凭据应通过临时 AskPass 环境注入")
	}
}
