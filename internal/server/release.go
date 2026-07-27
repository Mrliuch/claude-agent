package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxReleaseOutputBytes = 128 << 10

var releaseImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{1,500}$`)
var releaseName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,100}$`)

type releaseRunRequest struct {
	Action     string   `json:"action"`
	Workspace  string   `json:"workspace"`
	Dockerfile string   `json:"dockerfile"`
	Image      string   `json:"image"`
	Container  string   `json:"container"`
	Ports      []string `json:"ports"`
	Env        []string `json:"env"`
	Volumes    []string `json:"volumes"`
	TaskID     string   `json:"task_id"`
}

// handleReleaseRun is a deliberately narrow Docker executor. It never accepts
// a shell command: workspace is fenced and every Docker argument is validated.
func (s *Server) handleReleaseRun(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	var body releaseRunRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, "请求体格式错误", nil)
		return
	}
	body.Action = strings.TrimSpace(body.Action)
	if body.Action != "build" && body.Action != "docker_deploy" {
		writeJSON(w, 400, "不支持的发布操作", nil)
		return
	}
	if !releaseImage.MatchString(strings.TrimSpace(body.Image)) {
		writeJSON(w, 400, "镜像地址格式无效", nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	body.TaskID = strings.TrimSpace(body.TaskID)
	if body.TaskID != "" {
		s.releaseMu.Lock()
		if _, exists := s.releaseCancels[body.TaskID]; exists {
			s.releaseMu.Unlock()
			writeJSON(w, 409, "发布任务已在执行", nil)
			return
		}
		s.releaseCancels[body.TaskID] = cancel
		s.releaseMu.Unlock()
		defer func() { s.releaseMu.Lock(); delete(s.releaseCancels, body.TaskID); s.releaseMu.Unlock() }()
	}
	dockerEnv, cleanup, err := releaseDockerEnv()
	if err != nil {
		writeJSON(w, 500, "无法创建临时 Docker 凭据目录", nil)
		return
	}
	defer cleanup()
	if err := releaseLogin(ctx, r, dockerEnv); err != nil {
		writeJSON(w, 400, "镜像仓库登录失败", nil)
		return
	}
	var output string
	if body.Action == "build" {
		output, err = s.releaseBuild(ctx, body, dockerEnv)
	} else {
		output, err = s.releaseDockerDeploy(ctx, body, dockerEnv)
	}
	if ctx.Err() != nil {
		writeJSON(w, 504, "发布操作超时", map[string]any{"output": trimReleaseOutput(output)})
		return
	}
	if err != nil {
		writeJSON(w, 400, "发布操作失败", map[string]any{"output": trimReleaseOutput(output)})
		return
	}
	writeJSON(w, 0, "ok", map[string]any{"output": trimReleaseOutput(output)})
}

func (s *Server) handleReleaseCancel(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) || r.Method != http.MethodPost {
		writeJSON(w, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	s.releaseMu.Lock()
	cancel := s.releaseCancels[taskID]
	s.releaseMu.Unlock()
	if cancel == nil {
		writeJSON(w, 404, "构建任务不存在或已结束", nil)
		return
	}
	cancel()
	writeJSON(w, 0, "ok", nil)
}

// Registry credentials only exist in request headers and an operation-local
// Docker config directory. They are never placed in argv, output, or retained.
func releaseDockerEnv() ([]string, func(), error) {
	dir, err := os.MkdirTemp("", "claude-agent-docker-")
	if err != nil {
		return nil, nil, err
	}
	return append(os.Environ(), "DOCKER_CONFIG="+dir), func() { _ = os.RemoveAll(dir) }, nil
}

func releaseLogin(ctx context.Context, r *http.Request, dockerEnv []string) error {
	endpoint := strings.TrimSpace(r.Header.Get("X-Release-Registry"))
	username := strings.TrimSpace(r.Header.Get("X-Release-Username"))
	password := r.Header.Get("X-Release-Password")
	if endpoint == "" && username == "" && password == "" {
		return nil
	}
	if endpoint == "" || username == "" || password == "" || strings.ContainsAny(endpoint, " \t\n") {
		return errOutsideRoot
	}
	cmd := exec.CommandContext(ctx, "docker", "login", endpoint, "--username", username, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	cmd.Env = dockerEnv
	return cmd.Run()
}

func (s *Server) releaseBuild(ctx context.Context, body releaseRunRequest, dockerEnv []string) (string, error) {
	workspace, err := s.safeResolve(body.Workspace)
	if err != nil {
		return "", err
	}
	dockerfile := strings.TrimSpace(body.Dockerfile)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if filepath.IsAbs(dockerfile) || isProtectedFsPath(dockerfile) {
		return "", errOutsideRoot
	}
	if _, err = s.safeResolve(filepath.Join(body.Workspace, dockerfile)); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "docker", "build", "--pull", "-f", dockerfile, "-t", body.Image, ".")
	cmd.Dir = workspace
	cmd.Env = dockerEnv
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return output, err
	}
	cmd = exec.CommandContext(ctx, "docker", "push", body.Image)
	cmd.Env = dockerEnv
	out, err = cmd.CombinedOutput()
	return output + "\n" + string(out), err
}

func (s *Server) releaseDockerDeploy(ctx context.Context, body releaseRunRequest, dockerEnv []string) (string, error) {
	if !releaseName.MatchString(strings.TrimSpace(body.Container)) {
		return "", errOutsideRoot
	}
	args := []string{"run", "-d", "--restart", "unless-stopped", "--name", body.Container}
	for _, value := range body.Ports {
		if !regexp.MustCompile(`^[0-9]{1,5}:[0-9]{1,5}(/(tcp|udp))?$`).MatchString(value) {
			return "", errOutsideRoot
		}
		args = append(args, "-p", value)
	}
	for _, value := range body.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=.{0,4000}$`).MatchString(value) {
			return "", errOutsideRoot
		}
		args = append(args, "-e", value)
	}
	if len(body.Volumes) > 0 {
		return "", errOutsideRoot
	}
	cmd := exec.CommandContext(ctx, "docker", "pull", body.Image)
	cmd.Env = dockerEnv
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return output, err
	}
	remove := exec.CommandContext(ctx, "docker", "rm", "-f", body.Container)
	remove.Env = dockerEnv
	_ = remove.Run()
	args = append(args, body.Image)
	cmd = exec.CommandContext(ctx, "docker", args...)
	cmd.Env = dockerEnv
	out, err = cmd.CombinedOutput()
	return output + "\n" + string(out), err
}

func trimReleaseOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxReleaseOutputBytes {
		return value[len(value)-maxReleaseOutputBytes:]
	}
	return value
}
