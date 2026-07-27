package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxGitOutputBytes = 64 << 10

type gitRunRequest struct {
	Action        string `json:"action"`
	Workspace     string `json:"workspace"`
	RepositoryURL string `json:"repository_url"`
	Message       string `json:"message"`
	AuthorName    string `json:"author_name"`
	AuthorEmail   string `json:"author_email"`
}

// handleGitRun is intentionally narrow: it is an authenticated platform-only
// executor, not a shell endpoint. Workspace remains inside the agent work-dir.
func (s *Server) handleGitRun(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	var body gitRunRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, "请求体格式错误", nil)
		return
	}
	body.Action = strings.TrimSpace(body.Action)
	if !validGitAction(body.Action) {
		writeJSON(w, http.StatusBadRequest, "不支持的 Git 操作", nil)
		return
	}
	workspace, err := s.safeResolve(strings.TrimSpace(body.Workspace))
	if err != nil || workspace == "" {
		writeJSON(w, http.StatusBadRequest, "工作区路径无效或越界", nil)
		return
	}
	if body.Action == "clone" {
		if !validGitHTTPSURL(body.RepositoryURL) {
			writeJSON(w, http.StatusBadRequest, "仓库地址必须为不含凭据的 HTTPS 地址", nil)
			return
		}
		if info, statErr := os.Stat(workspace); statErr == nil && info.IsDir() {
			entries, readErr := os.ReadDir(workspace)
			if readErr != nil || len(entries) > 0 {
				writeJSON(w, http.StatusBadRequest, "工作区已存在且非空", nil)
				return
			}
		}
		if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
			writeJSON(w, http.StatusBadRequest, "无法创建工作区父目录", nil)
			return
		}
	} else if info, statErr := os.Stat(filepath.Join(workspace, ".git")); statErr != nil || !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, "项目尚未克隆", nil)
		return
	}

	args, err := gitArgs(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	askpass, cleanupAskpass, askpassErr := createGitAskpass(r.Header.Get("X-Dev-Git-Token"))
	if askpassErr != nil {
		writeJSON(w, http.StatusInternalServerError, "无法准备 Git 凭据助手", nil)
		return
	}
	defer cleanupAskpass()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = filepath.Dir(workspace)
	if body.Action != "clone" {
		cmd.Dir = workspace
	}
	cmd.Env = gitEnvironment(r.Header.Get("X-Dev-Git-Token"), askpass)
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		writeJSON(w, http.StatusGatewayTimeout, "Git 操作超时", map[string]any{"output": trimGitOutput(string(output))})
		return
	}
	if runErr != nil {
		writeJSON(w, http.StatusBadRequest, "Git 操作失败", map[string]any{"output": trimGitOutput(string(output))})
		return
	}
	writeJSON(w, 0, "ok", map[string]any{"output": trimGitOutput(string(output))})
}

func validGitAction(action string) bool {
	return action == "clone" || action == "pull" || action == "add" || action == "commit" || action == "push" || action == "status"
}

func validGitHTTPSURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func gitArgs(body gitRunRequest) ([]string, error) {
	switch body.Action {
	case "clone":
		destination := filepath.Base(filepath.Clean(body.Workspace))
		if destination == "." || destination == string(filepath.Separator) {
			return nil, errors.New("Clone 目标目录无效")
		}
		return []string{"clone", "--progress", "--origin", "origin", body.RepositoryURL, destination}, nil
	case "pull":
		return []string{"pull", "--ff-only", "--progress"}, nil
	case "add":
		return []string{"add", "--all"}, nil
	case "commit":
		if strings.TrimSpace(body.Message) == "" {
			return nil, errGitMessageRequired{}
		}
		name := strings.TrimSpace(body.AuthorName)
		if name == "" {
			name = "CloudScope"
		}
		email := strings.TrimSpace(body.AuthorEmail)
		if email == "" {
			email = "cloudscope@localhost"
		}
		return []string{"-c", "user.name=" + name, "-c", "user.email=" + email, "commit", "-m", body.Message}, nil
	case "status":
		return []string{"status", "--short", "--branch"}, nil
	default:
		return []string{"push", "--progress", "origin", "HEAD"}, nil
	}
}

type errGitMessageRequired struct{}

func (errGitMessageRequired) Error() string { return "提交说明不能为空" }

func createGitAskpass(token string) (string, func(), error) {
	if strings.TrimSpace(token) == "" {
		return "", func() {}, nil
	}
	file, err := os.CreateTemp("", "claude-agent-git-askpass-*")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	if _, err := file.WriteString("#!/bin/sh\ncase \"$1\" in\n  *Username*) printf '%s\\n' \"${GIT_ASKPASS_USERNAME}\" ;;\n  *) printf '%s\\n' \"${GIT_ASKPASS_TOKEN}\" ;;\nesac\n"); err != nil {
		file.Close()
		os.Remove(path)
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", func() {}, err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func gitEnvironment(token, askpass string) []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if strings.TrimSpace(token) != "" && askpass != "" {
		env = append(env, "GIT_ASKPASS="+askpass, "GIT_ASKPASS_USERNAME=oauth2", "GIT_ASKPASS_TOKEN="+token)
	}
	return env
}

func trimGitOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxGitOutputBytes {
		return value[len(value)-maxGitOutputBytes:]
	}
	return value
}
