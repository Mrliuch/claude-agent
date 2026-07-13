package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// handleAICatalog 返回 agent 本机发现的系统级 Skills 与 MCP 名称；只读且不返回密钥。
func (s *Server) handleAICatalog(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	root := s.cfg.ResolvedWorkDir()
	if sub := s.resolveWorkSubdir(r.URL.Query().Get("work_subdir")); sub != "" {
		root = sub
	}
	home, _ := os.UserHomeDir()
	// 系统目录与用户工作目录必须分开返回：前端只将 system_* 作为只读配置展示，
	// user_skills 则是当前用户可编辑的工作区内容。
	systemSkills := scanSkills(filepath.Join(home, ".claude", "skills"), "系统")
	userSkills := scanSkills(filepath.Join(root, ".claude", "skills"), "工作目录")
	// 系统 MCP 注册表不放进 ~/.claude.json：后者会被 Claude CLI 当作实际运行配置读取，
	// 若写入共享 Authorization 会让所有平台用户越权。system-mcp.json 仅供目录发现，
	// 每个用户的凭据由平台在建立连接时单独注入临时 MCP 配置。
	systemMCP := mergeMCP(
		scanMCP(filepath.Join(home, ".claude.json"), "系统"),
		scanMCP(filepath.Join(home, ".claude", "system-mcp.json"), "系统"),
	)
	claudeVersion, skillsOkay := s.claudeRuntimeInfo()
	writeJSON(w, 0, "ok", map[string]any{
		"system_skills": systemSkills, "user_skills": userSkills, "system_mcp": systemMCP,
		"claude_version": claudeVersion, "project_skills_supported": skillsOkay,
	})
}

func mergeMCP(groups ...[]map[string]string) []map[string]string {
	seen := map[string]bool{}
	out := []map[string]string{}
	for _, group := range groups {
		for _, item := range group {
			name := strings.TrimSpace(item["name"])
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

func scanSkills(dir, source string) []map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			if info, err := os.Stat(path); err == nil {
				out = append(out, map[string]string{"name": e.Name(), "source": source,
					"description": skillDescription(path), "updated_at": info.ModTime().Format(time.DateTime)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

// skillDescription 优先读取 SKILL.md frontmatter 的 description；没有则取正文首段。
func skillDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	inFront, first := false, ""
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "---" {
			inFront = !inFront
			continue
		}
		if inFront && strings.HasPrefix(strings.ToLower(line), "description:") {
			return strings.Trim(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), "\"'")
		}
		if !inFront && line != "" && !strings.HasPrefix(line, "#") && !strings.Contains(line, ":") {
			first = line
			break
		}
	}
	return first
}

func scanMCP(path, source string) []map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	out := []map[string]string{}
	for name := range raw.MCPServers {
		if strings.TrimSpace(name) != "" {
			out = append(out, map[string]string{"name": name, "source": source})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}
