package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"claude-agent/internal/protocol"
)

var (
	reNonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)
	reUUID     = regexp.MustCompile(`^[a-fA-F0-9-]{8,64}$`)
)

const maxSessionScanLines = 8000

func claudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// projectSlug 复刻 claude code 的工程目录编码。
func projectSlug(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return reNonAlnum.ReplaceAllString(abs, "-")
}

func (s *Server) sessionDir() string {
	root := claudeProjectsDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, projectSlug(s.cfg.ResolvedWorkDir()))
}

type sessionMeta struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Messages int    `json:"messages"`
	Mtime    string `json:"mtime"`
	Size     int64  `json:"size"`
}

func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	cwd := s.cfg.ResolvedWorkDir()
	dir := s.sessionDir()
	out := make([]sessionMeta, 0, 16)
	if dir != "" {
		if infos, err := os.ReadDir(dir); err == nil {
			for _, de := range infos {
				name := de.Name()
				if de.IsDir() || !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				fi, err := de.Info()
				if err != nil {
					continue
				}
				title, count := scanSessionMeta(filepath.Join(dir, name))
				out = append(out, sessionMeta{
					ID:       strings.TrimSuffix(name, ".jsonl"),
					Title:    title,
					Messages: count,
					Mtime:    fi.ModTime().Format("2006-01-02 15:04:05"),
					Size:     fi.Size(),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
	writeJSON(w, 0, "ok", map[string]any{"cwd": cwd, "sessions": out})
}

func scanSessionMeta(path string) (title string, count int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lines := 0
	for sc.Scan() {
		if lines++; lines > maxSessionScanLines {
			break
		}
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "user":
			count++
			if title == "" {
				if t := firstUserText(m); t != "" {
					title = t
				}
			}
		case "assistant":
			count++
		}
	}
	return title, count
}

func firstUserText(m map[string]any) string {
	msg, _ := m["message"].(map[string]any)
	switch c := msg["content"].(type) {
	case string:
		return clip(strings.TrimSpace(c), 80)
	case []any:
		for _, it := range c {
			if blk, ok := it.(map[string]any); ok {
				if blk["type"] == "text" {
					return clip(strings.TrimSpace(protocol.StrOr(blk["text"], "")), 80)
				}
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

type sessionItem struct {
	Role   string           `json:"role"`
	Blocks []map[string]any `json:"blocks"`
	Ts     string           `json:"ts"`
}

func (s *Server) handleSessionRead(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	id := r.URL.Query().Get("id")
	if !reUUID.MatchString(id) {
		writeJSON(w, 400, "非法会话 id", nil)
		return
	}
	dir := s.sessionDir()
	if dir == "" {
		writeJSON(w, 400, "无法定位会话目录", nil)
		return
	}
	path := filepath.Join(dir, id+".jsonl")
	if filepath.Dir(path) != dir {
		writeJSON(w, 400, "非法会话 id", nil)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, 400, "会话不存在", nil)
		return
	}
	defer f.Close()

	items := make([]sessionItem, 0, 256)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		ts := protocol.StrOr(m["timestamp"], "")
		switch m["type"] {
		case "user":
			if blocks := userBlocks(m); len(blocks) > 0 {
				items = append(items, sessionItem{Role: "user", Blocks: blocks, Ts: ts})
			}
		case "assistant":
			if blocks := protocol.ExtractAssistantBlocks(m); len(blocks) > 0 {
				items = append(items, sessionItem{Role: "assistant", Blocks: blocks, Ts: ts})
			}
		}
	}
	writeJSON(w, 0, "ok", map[string]any{"id": id, "items": items})
}

func userBlocks(m map[string]any) []map[string]any {
	msg, _ := m["message"].(map[string]any)
	switch c := msg["content"].(type) {
	case string:
		if t := strings.TrimSpace(c); t != "" {
			return []map[string]any{{"kind": "text", "text": t}}
		}
	case []any:
		var blocks []map[string]any
		for _, it := range c {
			blk, _ := it.(map[string]any)
			switch blk["type"] {
			case "text":
				if t := protocol.StrOr(blk["text"], ""); t != "" {
					blocks = append(blocks, map[string]any{"kind": "text", "text": t})
				}
			case "tool_result":
				blocks = append(blocks, map[string]any{
					"kind":     "tool_result",
					"content":  protocol.StringifyContent(blk["content"]),
					"is_error": protocol.BoolOf(blk["is_error"]),
				})
			}
		}
		return blocks
	}
	return nil
}
