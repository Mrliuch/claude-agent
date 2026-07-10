package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	skills := append(scanSkills(filepath.Join(home, ".claude", "skills"), "系统"), scanSkills(filepath.Join(root, ".claude", "skills"), "工作目录")...)
	mcp := append(scanMCP(filepath.Join(home, ".claude.json"), "系统"), scanMCP(filepath.Join(root, ".mcp.json"), "工作目录")...)
	writeJSON(w, 0, "ok", map[string]any{"system_skills": skills, "system_mcp": mcp})
}

func scanSkills(dir, source string) []map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err == nil {
				out = append(out, map[string]string{"name": e.Name(), "source": source})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
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
