package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(value bool) *bool { return &value }

func TestReleaseBuildArgsAllowOnlyWhitelistedOptions(t *testing.T) {
	args, err := releaseBuildArgs(releaseRunRequest{
		Image:       "registry.example.com/team/api:latest",
		BuildTarget: "runtime",
		BuildArgs:   []string{"VERSION=1.2.3", "FEATURE_FLAG=true"},
		Pull:        boolPtr(false),
		NoCache:     true,
	}, "Dockerfile")
	if err != nil {
		t.Fatalf("releaseBuildArgs error: %v", err)
	}
	want := []string{"build", "--no-cache", "--target", "runtime", "--build-arg", "VERSION=1.2.3", "--build-arg", "FEATURE_FLAG=true", "-f", "Dockerfile", "-t", "registry.example.com/team/api:latest", "."}
	if len(args) != len(want) {
		t.Fatalf("unexpected args: %#v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, args[i], want[i])
		}
	}
}

func TestReleaseBuildArgsRejectInvalidBuildArg(t *testing.T) {
	_, err := releaseBuildArgs(releaseRunRequest{Image: "registry.example.com/team/api:latest", BuildArgs: []string{"--network=host"}}, "Dockerfile")
	if err == nil {
		t.Fatal("expected invalid build argument to be rejected")
	}
}

func TestReleaseOpsAllowOnlyFixedActionsAndNames(t *testing.T) {
	for _, action := range []string{"status", "logs", "start", "stop", "restart"} {
		if !validReleaseOps(action, "cloudscope-api") {
			t.Fatalf("expected action %q to be allowed", action)
		}
	}
	if validReleaseOps("exec", "cloudscope-api") || validReleaseOps("status", "api;id") {
		t.Fatal("arbitrary action or container name must be rejected")
	}
}

func TestReleaseTaskReturnsRedactedIncrementalOutput(t *testing.T) {
	task := &releaseTask{status: "running", stage: "Dockerfile 构建中", redacts: []string{"top-secret"}}
	task.appendOutput("#3 token=top-secret\n")
	first := task.snapshot(0)
	if first["output"] != "#3 token=***\n" {
		t.Fatalf("unexpected redacted output: %#v", first["output"])
	}
	offset, ok := first["next_offset"].(int64)
	if !ok {
		t.Fatalf("unexpected offset type: %T", first["next_offset"])
	}
	task.appendOutput("#4 DONE\n")
	second := task.snapshot(offset)
	if second["output"] != "#4 DONE\n" {
		t.Fatalf("unexpected incremental output: %#v", second["output"])
	}
}

func TestPrepareGoProxyDockerfileInjectsDefaultsWithoutChangingSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "Dockerfile")
	original := "FROM golang:1.25-alpine\nWORKDIR /build\nRUN go mod download\n"
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	generated, cleanup, injected, err := prepareGoProxyDockerfile(source, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !injected || generated == "Dockerfile" {
		t.Fatalf("expected injected temporary Dockerfile: %q, %v", generated, injected)
	}
	data, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ARG GOPROXY=https://goproxy.cn,direct") ||
		!strings.Contains(string(data), "ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB}") {
		t.Fatalf("missing proxy defaults: %s", data)
	}
	unchanged, err := os.ReadFile(source)
	if err != nil || string(unchanged) != original {
		t.Fatalf("source Dockerfile was modified: %q, %v", unchanged, err)
	}
}

func TestPrepareGoProxyDockerfileKeepsExplicitProxy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "Dockerfile")
	content := "FROM golang:1.25-alpine\nARG GOPROXY=https://internal.example\nRUN go mod download\n"
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	generated, cleanup, injected, err := prepareGoProxyDockerfile(source, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if injected || generated != "Dockerfile" {
		t.Fatalf("explicit proxy should be preserved: %q, %v", generated, injected)
	}
}
