package bridge

import (
	"encoding/json"
	"os"
	"testing"

	"claude-agent/internal/config"
)

func TestBuildUserSettingsEnv_TokenOnly_InheritsGatewayAndModels(t *testing.T) {
	base := map[string]string{
		"ANTHROPIC_AUTH_TOKEN":           "shared-token",
		"ANTHROPIC_BASE_URL":             "https://example.gateway.com",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "my-opus-model",
		"CLAUDE_CODE_ATTRIBUTION_HEADER": "0",
	}
	env := buildUserSettingsEnv(config.Config{ClaudeAuthToken: "user-token"}, base)

	if env["ANTHROPIC_AUTH_TOKEN"] != "user-token" {
		t.Fatalf("应覆盖为用户 token，got %q", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://example.gateway.com" {
		t.Fatalf("未给 base_url 时应保留原网关，got %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "my-opus-model" {
		t.Fatalf("应保留网关模型映射，got %q", env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
	if env["CLAUDE_CODE_ATTRIBUTION_HEADER"] != "0" {
		t.Fatalf("应保留其它 CLAUDE_CODE_* 开关")
	}
}

func TestBuildUserSettingsEnv_WithBaseURL_DropsGatewayModels(t *testing.T) {
	base := map[string]string{
		"ANTHROPIC_AUTH_TOKEN":           "shared-token",
		"ANTHROPIC_BASE_URL":             "https://example.gateway.com",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "gw-opus",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "gw-sonnet",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "gw-haiku",
	}
	env := buildUserSettingsEnv(
		config.Config{ClaudeAuthToken: "user-token", ClaudeBaseURL: "https://my.endpoint.com"}, base)

	if env["ANTHROPIC_BASE_URL"] != "https://my.endpoint.com" {
		t.Fatalf("应覆盖为用户 base_url，got %q", env["ANTHROPIC_BASE_URL"])
	}
	for _, k := range gatewayModelKeys {
		if _, ok := env[k]; ok {
			t.Fatalf("给了自定义 base_url 时应丢弃网关模型映射 %s", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "user-token" {
		t.Fatalf("应覆盖为用户 token")
	}
}

func TestBuildUserSettingsEnv_EmptyBase(t *testing.T) {
	env := buildUserSettingsEnv(config.Config{ClaudeAuthToken: "tok"}, map[string]string{})
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Fatalf("空基底也应注入用户 token")
	}
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("未给 base_url 且无基底时不应有 ANTHROPIC_BASE_URL")
	}
}

func TestBuildUserSettingsEnv_DoesNotMutateBase(t *testing.T) {
	base := map[string]string{"ANTHROPIC_AUTH_TOKEN": "shared"}
	_ = buildUserSettingsEnv(config.Config{ClaudeAuthToken: "user"}, base)
	if base["ANTHROPIC_AUTH_TOKEN"] != "shared" {
		t.Fatalf("不应修改入参基底，got %q", base["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestWriteUserSettings_FileContent(t *testing.T) {
	path, err := writeUserSettings(config.Config{ClaudeAuthToken: "user-token"})
	if err != nil {
		t.Fatalf("writeUserSettings 失败: %v", err)
	}
	defer os.Remove(path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings 文件应为 0600，got %v", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("内容非合法 JSON: %v", err)
	}
	if parsed.Env["ANTHROPIC_AUTH_TOKEN"] != "user-token" {
		t.Fatalf("落盘应含用户 token，got %q", parsed.Env["ANTHROPIC_AUTH_TOKEN"])
	}
}
