package server

import (
	"net/http"
	"testing"

	"claude-agent/internal/config"
)

func TestApplyTaskAuditHeaders_AcceptsPlatformActorProof(t *testing.T) {
	cfg := config.Config{}
	headers := make(http.Header)
	headers.Set("X-Claude-Task-Audit-Url", "https://cloudscope.example/api/audit/ai-tasks/ingest")
	headers.Set("X-Claude-Task-Audit-Token", "csactor_platform_user")

	applyTaskAuditHeaders(&cfg, headers)

	if cfg.TaskAuditURL != "https://cloudscope.example/api/audit/ai-tasks/ingest" {
		t.Fatalf("任务归档地址未注入: %q", cfg.TaskAuditURL)
	}
	if cfg.TaskAuditToken != "csactor_platform_user" {
		t.Fatalf("csactor_ 操作者证明未注入: %q", cfg.TaskAuditToken)
	}
}

func TestApplyTaskAuditHeaders_RejectsServiceTokenAndIncompleteContext(t *testing.T) {
	cfg := config.Config{TaskAuditURL: "https://stale.example", TaskAuditToken: "csactor_stale"}
	headers := make(http.Header)
	headers.Set("X-Claude-Task-Audit-Url", "https://cloudscope.example/api/audit/ai-tasks/ingest")
	headers.Set("X-Claude-Task-Audit-Token", "cstask-local-service")

	applyTaskAuditHeaders(&cfg, headers)

	if cfg.TaskAuditURL != "" || cfg.TaskAuditToken != "" {
		t.Fatalf("不得接受 cstask_ 服务令牌或保留旧会话上下文: %+v", cfg)
	}
}
