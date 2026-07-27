package server

import (
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

func TestGitEnvironmentUsesTemporaryBasicHeader(t *testing.T) {
	joined := strings.Join(gitEnvironment("token-value"), "\n")
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=http.extraheader") || !strings.Contains(joined, "Authorization: Basic ") {
		t.Fatal("Git 认证应通过临时 HTTP Basic Header 注入")
	}
}
