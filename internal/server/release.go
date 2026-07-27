package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxReleaseOutputBytes = 128 << 10

var releaseImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{1,500}$`)
var releaseName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,100}$`)
var releaseBuildTarget = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}$`)
var releaseBuildArg = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\r\n]{0,1000}$`)
var releaseTaskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}$`)

type releaseRunRequest struct {
	Action      string   `json:"action"`
	Workspace   string   `json:"workspace"`
	Dockerfile  string   `json:"dockerfile"`
	Image       string   `json:"image"`
	Container   string   `json:"container"`
	Ports       []string `json:"ports"`
	Env         []string `json:"env"`
	Volumes     []string `json:"volumes"`
	TaskID      string   `json:"task_id"`
	BuildTarget string   `json:"build_target"`
	BuildArgs   []string `json:"build_args"`
	Pull        *bool    `json:"pull"`
	NoCache     bool     `json:"no_cache"`
	ReleaseID   string   `json:"release_id"`
}

type releaseOpsRequest struct {
	Action    string `json:"action"`
	Container string `json:"container"`
	Tail      int    `json:"tail"`
}

type releaseCredentials struct {
	endpoint string
	username string
	password string
}

// releaseTask keeps only one bounded, redacted output buffer.  The platform
// reads it using a byte offset so Dockerfile output can be shown while Docker
// is still running without opening a generic command-stream API.
type releaseTask struct {
	mu       sync.Mutex
	status   string
	stage    string
	output   string
	logStart int64
	errorMsg string
	finished time.Time
	redacts  []string
}

func (t *releaseTask) appendOutput(value string) {
	if value == "" {
		return
	}
	for _, secret := range t.redacts {
		if len(secret) >= 3 {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	value = regexp.MustCompile(`(?i)((?:password|passwd|token|secret|api[_-]?key)\s*[:=]\s*)\S+`).ReplaceAllString(value, "$1***")
	t.mu.Lock()
	defer t.mu.Unlock()
	combined := append([]byte(t.output), []byte(value)...)
	if len(combined) > maxReleaseOutputBytes {
		drop := len(combined) - maxReleaseOutputBytes
		t.logStart += int64(drop)
		combined = combined[drop:]
	}
	t.output = string(combined)
}

func (t *releaseTask) snapshot(offset int64) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	if offset < t.logStart {
		offset = t.logStart
	}
	end := t.logStart + int64(len(t.output))
	if offset > end {
		offset = end
	}
	part := []byte(t.output)[offset-t.logStart:]
	return map[string]any{"status": t.status, "stage": t.stage, "output": string(part),
		"next_offset": end, "reset": offset == t.logStart && t.logStart > 0,
		"error": t.errorMsg}
}

func (t *releaseTask) setStage(stage string) {
	t.mu.Lock()
	t.stage = stage
	t.mu.Unlock()
}

func (t *releaseTask) finish(status, message string) {
	t.mu.Lock()
	t.status = status
	t.errorMsg = message
	t.finished = time.Now()
	t.mu.Unlock()
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
	body.TaskID = strings.TrimSpace(body.TaskID)
	credentials, err := releaseCredentialsFromRequest(r)
	if err != nil {
		writeJSON(w, 400, "镜像仓库凭据无效", nil)
		return
	}
	if body.Action == "build" {
		if !releaseTaskID.MatchString(body.TaskID) {
			writeJSON(w, 400, "构建任务标识无效", nil)
			return
		}
		if err := s.startReleaseBuild(body, credentials); err != nil {
			writeJSON(w, 409, err.Error(), nil)
			return
		}
		writeJSON(w, 0, "ok", map[string]any{"task_id": body.TaskID, "status": "running"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	dockerEnv, cleanup, err := releaseDockerEnv()
	if err != nil {
		writeJSON(w, 500, "无法创建临时 Docker 凭据目录", nil)
		return
	}
	defer cleanup()
	if err := releaseLogin(ctx, credentials, dockerEnv); err != nil {
		writeJSON(w, 400, "镜像仓库登录失败", nil)
		return
	}
	var output string
	output, err = s.releaseDockerDeploy(ctx, body, dockerEnv)
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

func (s *Server) startReleaseBuild(body releaseRunRequest, credentials releaseCredentials) error {
	s.releaseMu.Lock()
	defer s.releaseMu.Unlock()
	s.cleanupReleaseTasksLocked()
	if _, exists := s.releaseCancels[body.TaskID]; exists {
		return errorsNew("发布任务已在执行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	task := &releaseTask{status: "running", stage: "正在准备 Docker 构建任务", redacts: releaseRedacts(body, credentials)}
	s.releaseCancels[body.TaskID] = cancel
	s.releaseTasks[body.TaskID] = task
	go s.runReleaseBuild(ctx, cancel, body.TaskID, body, credentials, task)
	return nil
}

// errorsNew is kept local to avoid exposing a task implementation detail in
// callers and keeps this file's user-facing errors in Chinese.
func errorsNew(message string) error { return &releaseError{message: message} }

type releaseError struct{ message string }

func (e *releaseError) Error() string { return e.message }

func (s *Server) runReleaseBuild(ctx context.Context, cancel context.CancelFunc, taskID string, body releaseRunRequest, credentials releaseCredentials, task *releaseTask) {
	defer cancel()
	defer func() {
		s.releaseMu.Lock()
		delete(s.releaseCancels, taskID)
		s.releaseMu.Unlock()
	}()
	dockerEnv, cleanup, err := releaseDockerEnv()
	if err != nil {
		task.appendOutput("[构建失败] 无法创建临时 Docker 凭据目录\n")
		task.finish("failed", "无法创建临时 Docker 凭据目录")
		return
	}
	defer cleanup()
	task.setStage("正在登录镜像仓库")
	task.appendOutput("[正在登录镜像仓库]\n")
	if err := releaseLogin(ctx, credentials, dockerEnv); err != nil {
		task.appendOutput("[构建失败] 镜像仓库登录失败\n")
		task.finish(releaseTaskStatus(ctx), "镜像仓库登录失败")
		return
	}
	task.setStage("Dockerfile 构建中")
	task.appendOutput("[Docker build]\n")
	if err := s.releaseBuildStream(ctx, body, dockerEnv, task); err != nil {
		status := releaseTaskStatus(ctx)
		message := "Docker 构建或推送失败"
		if status == "cancelled" {
			message = "构建已中断"
		}
		task.appendOutput("\n[" + message + "]\n")
		task.finish(status, message)
		return
	}
	task.appendOutput("\n[镜像构建并推送完成]\n")
	task.finish("success", "")
}

func releaseTaskStatus(ctx context.Context) string {
	if ctx.Err() == context.Canceled {
		return "cancelled"
	}
	if ctx.Err() != nil {
		return "failed"
	}
	return "failed"
}

func (s *Server) cleanupReleaseTasksLocked() {
	for id, task := range s.releaseTasks {
		task.mu.Lock()
		finished := task.finished
		task.mu.Unlock()
		if !finished.IsZero() && time.Since(finished) > time.Hour {
			delete(s.releaseTasks, id)
		}
	}
}

func (s *Server) handleReleaseTask(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if !releaseTaskID.MatchString(taskID) {
		writeJSON(w, 400, "构建任务标识无效", nil)
		return
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("offset")), 10, 64)
	if err != nil || offset < 0 {
		offset = 0
	}
	s.releaseMu.Lock()
	task := s.releaseTasks[taskID]
	s.releaseMu.Unlock()
	if task == nil {
		writeJSON(w, 404, "构建任务不存在或日志已过期", nil)
		return
	}
	writeJSON(w, 0, "ok", task.snapshot(offset))
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

// handleReleaseOps exposes only the small Docker operation set needed by the
// application operations page. It deliberately does not accept arbitrary
// Docker arguments, inspect templates, or container filters.
func (s *Server) handleReleaseOps(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	var body releaseOpsRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, "请求体格式错误", nil)
		return
	}
	body.Action = strings.TrimSpace(body.Action)
	body.Container = strings.TrimSpace(body.Container)
	if !validReleaseOps(body.Action, body.Container) {
		writeJSON(w, http.StatusBadRequest, "Docker 运维操作或容器名称无效", nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data, err := releaseDockerOps(ctx, body)
	if ctx.Err() != nil {
		writeJSON(w, http.StatusGatewayTimeout, "Docker 运维操作超时", nil)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, "Docker 运维操作失败", map[string]any{"output": trimReleaseOutput(data["output"])})
		return
	}
	writeJSON(w, 0, "ok", data)
}

func validReleaseOps(action, container string) bool {
	if !releaseName.MatchString(container) {
		return false
	}
	switch action {
	case "status", "logs", "start", "stop", "restart":
		return true
	default:
		return false
	}
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

func releaseCredentialsFromRequest(r *http.Request) (releaseCredentials, error) {
	credentials := releaseCredentials{endpoint: strings.TrimSpace(r.Header.Get("X-Release-Registry")), username: strings.TrimSpace(r.Header.Get("X-Release-Username")), password: r.Header.Get("X-Release-Password")}
	if credentials.endpoint == "" && credentials.username == "" && credentials.password == "" {
		return credentials, nil
	}
	if credentials.endpoint == "" || credentials.username == "" || credentials.password == "" || strings.ContainsAny(credentials.endpoint, " \t\n") {
		return releaseCredentials{}, errOutsideRoot
	}
	return credentials, nil
}

func releaseLogin(ctx context.Context, credentials releaseCredentials, dockerEnv []string) error {
	if credentials.endpoint == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "login", credentials.endpoint, "--username", credentials.username, "--password-stdin")
	cmd.Stdin = strings.NewReader(credentials.password)
	cmd.Env = dockerEnv
	return cmd.Run()
}

func (s *Server) releaseBuildStream(ctx context.Context, body releaseRunRequest, dockerEnv []string, task *releaseTask) error {
	workspace, err := s.safeResolve(body.Workspace)
	if err != nil {
		return err
	}
	dockerfile := strings.TrimSpace(body.Dockerfile)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if filepath.IsAbs(dockerfile) || isProtectedFsPath(dockerfile) {
		return errOutsideRoot
	}
	if _, err = s.safeResolve(filepath.Join(body.Workspace, dockerfile)); err != nil {
		return err
	}
	args, err := releaseBuildArgs(body, dockerfile)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = workspace
	cmd.Env = dockerEnv
	if err := runReleaseCommand(cmd, task); err != nil {
		return err
	}
	task.setStage("正在推送镜像")
	task.appendOutput("\n[Docker push]\n")
	cmd = exec.CommandContext(ctx, "docker", "push", body.Image)
	cmd.Env = dockerEnv
	return runReleaseCommand(cmd, task)
}

type releaseLogWriter struct{ task *releaseTask }

func (w releaseLogWriter) Write(p []byte) (int, error) {
	w.task.appendOutput(string(p))
	return len(p), nil
}

func runReleaseCommand(cmd *exec.Cmd, task *releaseTask) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, stream := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(reader io.Reader) { defer wg.Done(); _, _ = io.Copy(releaseLogWriter{task}, reader) }(stream)
	}
	err = cmd.Wait()
	wg.Wait()
	return err
}

func releaseRedacts(body releaseRunRequest, credentials releaseCredentials) []string {
	values := []string{credentials.password}
	for _, item := range body.BuildArgs {
		if _, value, ok := strings.Cut(item, "="); ok {
			values = append(values, value)
		}
	}
	return values
}

func releaseBuildArgs(body releaseRunRequest, dockerfile string) ([]string, error) {
	if len(body.BuildArgs) > 30 {
		return nil, errOutsideRoot
	}
	args := []string{"build"}
	if body.Pull == nil || *body.Pull {
		args = append(args, "--pull")
	}
	if body.NoCache {
		args = append(args, "--no-cache")
	}
	if target := strings.TrimSpace(body.BuildTarget); target != "" {
		if !releaseBuildTarget.MatchString(target) {
			return nil, errOutsideRoot
		}
		args = append(args, "--target", target)
	}
	for _, value := range body.BuildArgs {
		if !releaseBuildArg.MatchString(value) {
			return nil, errOutsideRoot
		}
		args = append(args, "--build-arg", value)
	}
	return append(args, "-f", dockerfile, "-t", body.Image, "."), nil
}

func (s *Server) releaseDockerDeploy(ctx context.Context, body releaseRunRequest, dockerEnv []string) (string, error) {
	if !releaseName.MatchString(strings.TrimSpace(body.Container)) {
		return "", errOutsideRoot
	}
	args := []string{"run", "-d", "--restart", "unless-stopped", "--name", body.Container, "--label", "cloudscope.managed=true"}
	if releaseID := strings.TrimSpace(body.ReleaseID); releaseID != "" {
		if !releaseName.MatchString(releaseID) {
			return "", errOutsideRoot
		}
		args = append(args, "--label", "cloudscope.release_id="+releaseID)
	}
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

func releaseDockerOps(ctx context.Context, body releaseOpsRequest) (map[string]string, error) {
	switch body.Action {
	case "status":
		return releaseDockerStatus(ctx, body.Container)
	case "logs":
		tail := body.Tail
		if tail <= 0 {
			tail = 500
		}
		if tail > 2000 {
			tail = 2000
		}
		out, err := exec.CommandContext(ctx, "docker", "logs", "--timestamps", "--tail", strconv.Itoa(tail), body.Container).CombinedOutput()
		return map[string]string{"output": trimReleaseOutput(string(out))}, err
	case "start", "stop", "restart":
		out, err := exec.CommandContext(ctx, "docker", body.Action, body.Container).CombinedOutput()
		return map[string]string{"output": trimReleaseOutput(string(out))}, err
	default:
		return map[string]string{}, errOutsideRoot
	}
}

func releaseDockerStatus(ctx context.Context, container string) (map[string]string, error) {
	// The template is fixed in code so inspect never returns environment values,
	// labels beyond the managed marker, mounts, or other sensitive metadata.
	const format = "{{.State.Status}}\\t{{.State.Running}}\\t{{.Config.Image}}\\t{{.State.StartedAt}}\\t{{.State.FinishedAt}}\\t{{.RestartCount}}"
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, container).CombinedOutput()
	if err != nil {
		return map[string]string{"output": trimReleaseOutput(string(out))}, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\\t")
	if len(parts) != 6 {
		return map[string]string{}, errOutsideRoot
	}
	result := map[string]string{"status": parts[0], "running": parts[1], "image": parts[2], "started_at": parts[3], "finished_at": parts[4], "restart_count": parts[5]}
	stats, statsErr := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.CPUPerc}}\\t{{.MemUsage}}", container).CombinedOutput()
	if statsErr == nil {
		values := strings.Split(strings.TrimSpace(string(stats)), "\\t")
		if len(values) == 2 {
			result["cpu"] = values[0]
			result["memory"] = values[1]
		}
	}
	return result, nil
}

func trimReleaseOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxReleaseOutputBytes {
		return value[len(value)-maxReleaseOutputBytes:]
	}
	return value
}
