package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// claude code 把每个工作目录的会话历史存为 jsonl：
//   ~/.claude/projects/<slug>/<session-uuid>.jsonl
// 其中 <slug> 是工作目录绝对路径里「每个非字母数字字符替换为 '-'」的结果。
// 本文件提供两个只读端点，让 Web 控制台回看/续接这些历史会话。

var (
	reNonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)
	reUUID     = regexp.MustCompile(`^[a-fA-F0-9-]{8,64}$`)
)

const maxSessionScanLines = 8000 // 列表统计时单文件最多扫描行数，避免超大会话拖慢

// claudeProjectsDir 返回 ~/.claude/projects。
func claudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// projectSlug 复刻 claude code 的工程目录编码：取真实（解析软链后）的绝对路径，
// 再把每个非字母数字字符替换为 '-'。解析软链很关键——macOS 上 /tmp 实为
// /private/tmp，claude 记录的是解析后的路径，不解析会导致 slug 对不上。
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

// sessionDir 返回当前工作目录对应的会话目录（可能尚不存在）。
func (s *Server) sessionDir() string {
	root := claudeProjectsDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, projectSlug(s.cfg.resolvedWorkDir()))
}

type sessionMeta struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Messages int    `json:"messages"`
	Mtime    string `json:"mtime"`
	Size     int64  `json:"size"`
}

// handleSessionsList 列出当前工作目录的历史会话（按修改时间倒序）。
func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	cwd := s.cfg.resolvedWorkDir()
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

// scanSessionMeta 扫描一个会话 jsonl，取首条用户消息为标题、统计 user+assistant 条数。
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

// firstUserText 从一条 user 记录里取可读文本（content 可能是 string 或 block 数组）。
func firstUserText(m map[string]any) string {
	msg, _ := m["message"].(map[string]any)
	switch c := msg["content"].(type) {
	case string:
		return clip(strings.TrimSpace(c), 80)
	case []any:
		for _, it := range c {
			if blk, ok := it.(map[string]any); ok {
				if blk["type"] == "text" {
					return clip(strings.TrimSpace(strOr(blk["text"], "")), 80)
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

// sessionItem 是回看用的一条精简记录。
type sessionItem struct {
	Role   string           `json:"role"`
	Blocks []map[string]any `json:"blocks"`
	Ts     string           `json:"ts"`
}

// handleSessionRead 解析指定会话 jsonl，返回精简的可回看记录。
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
	if filepath.Dir(path) != dir { // 双保险，防止任何路径逃逸
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
		ts := strOr(m["timestamp"], "")
		switch m["type"] {
		case "user":
			if blocks := userBlocks(m); len(blocks) > 0 {
				items = append(items, sessionItem{Role: "user", Blocks: blocks, Ts: ts})
			}
		case "assistant":
			if blocks := extractAssistantBlocks(m); len(blocks) > 0 {
				items = append(items, sessionItem{Role: "assistant", Blocks: blocks, Ts: ts})
			}
		}
	}
	writeJSON(w, 0, "ok", map[string]any{"id": id, "items": items})
}

// userBlocks 把一条 user 记录转成回看 blocks：纯文本 → text 块；
// 工具结果数组 → tool_result 块（与 assistant 的 block 结构保持一致）。
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
				if t := strOr(blk["text"], ""); t != "" {
					blocks = append(blocks, map[string]any{"kind": "text", "text": t})
				}
			case "tool_result":
				blocks = append(blocks, map[string]any{
					"kind":     "tool_result",
					"content":  stringifyContent(blk["content"]),
					"is_error": boolOf(blk["is_error"]),
				})
			}
		}
		return blocks
	}
	return nil
}
