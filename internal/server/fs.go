package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxReadBytes = 1 << 20

var errOutsideRoot = errors.New("路径越界：仅允许操作工作目录及其子目录")

// realRoot 返回经软链解析的工作目录绝对路径（围栏根）。
func (s *Server) realRoot() (string, error) {
	wd := s.cfg.ResolvedWorkDir()
	abs, err := filepath.Abs(wd)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// safeResolve 把相对 work_dir 的路径解析为绝对路径，并强制围栏。
func (s *Server) safeResolve(rel string) (string, error) {
	root, err := s.realRoot()
	if err != nil {
		return "", err
	}
	clean := filepath.Clean("/" + filepath.ToSlash(strings.TrimSpace(rel)))
	target := filepath.Join(root, clean)

	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", errOutsideRoot
	}
	real, err := evalRealWithin(target)
	if err != nil {
		return "", err
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", errOutsideRoot
	}
	return target, nil
}

// evalRealWithin 解析 target 路径中已存在部分的真实路径，再把尚不存在的尾部原样拼回。
func evalRealWithin(target string) (string, error) {
	p := target
	var tail []string
	for {
		if _, err := os.Lstat(p); err == nil {
			realP, err := filepath.EvalSymlinks(p)
			if err != nil {
				return "", err
			}
			full := realP
			for i := len(tail) - 1; i >= 0; i-- {
				full = filepath.Join(full, tail[i])
			}
			return full, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return target, nil
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}

type fsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mtime string `json:"mtime"`
}

func (s *Server) handleFsList(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	abs, err := s.safeResolve(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	infos, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, 400, "读取目录失败: "+err.Error(), nil)
		return
	}
	entries := make([]fsEntry, 0, len(infos))
	for _, de := range infos {
		fi, err := de.Info()
		if err != nil {
			continue
		}
		entries = append(entries, fsEntry{
			Name:  de.Name(),
			IsDir: de.IsDir(),
			Size:  fi.Size(),
			Mtime: fi.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	writeJSON(w, 0, "ok", map[string]any{"path": r.URL.Query().Get("path"), "entries": entries})
}

func (s *Server) handleFsRead(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	abs, err := s.safeResolve(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		writeJSON(w, 400, "文件不存在", nil)
		return
	}
	if fi.IsDir() {
		writeJSON(w, 400, "目标是目录，不能读取内容", nil)
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		writeJSON(w, 400, "读取失败: "+err.Error(), nil)
		return
	}
	truncated := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		truncated = true
	}
	if isBinary(data) {
		writeJSON(w, 400, "二进制文件，暂不支持在线查看", nil)
		return
	}
	writeJSON(w, 0, "ok", map[string]any{
		"content": string(data), "truncated": truncated, "size": fi.Size(),
	})
}

func (s *Server) handleFsWrite(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, "请求体格式错误", nil)
		return
	}
	abs, err := s.safeResolve(body.Path)
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		writeJSON(w, 400, "目标是目录，不能写入", nil)
		return
	}
	if err := os.WriteFile(abs, []byte(body.Content), 0o644); err != nil {
		writeJSON(w, 400, "写入失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

const maxUploadBytes = 50 << 20

func (s *Server) handleFsUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	var body struct {
		Path       string `json:"path"`
		ContentB64 string `json:"content_b64"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, "请求体格式错误", nil)
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.ContentB64)
	if err != nil {
		writeJSON(w, 400, "文件内容解码失败", nil)
		return
	}
	if len(data) > maxUploadBytes {
		writeJSON(w, 400, "文件超过 50MB 上限", nil)
		return
	}
	abs, err := s.safeResolve(body.Path)
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		writeJSON(w, 400, "目标是目录，不能写入", nil)
		return
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		writeJSON(w, 400, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

func (s *Server) handleFsMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, "请求体格式错误", nil)
		return
	}
	abs, err := s.safeResolve(body.Path)
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeJSON(w, 400, "创建目录失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

// handleFsDownload 流式下载工作目录内的文件。
func (s *Server) handleFsDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	abs, err := s.safeResolve(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		writeJSON(w, 400, "目标不存在或为目录", nil)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeJSON(w, 400, "打开失败: "+err.Error(), nil)
		return
	}
	defer f.Close()

	name := filepath.Base(abs)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Content-Disposition",
		"attachment; filename=\"download\"; filename*=UTF-8''"+url.PathEscape(name))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (s *Server) handleFsDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	rel := r.URL.Query().Get("path")
	abs, err := s.safeResolve(rel)
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	root, _ := s.realRoot()
	if abs == root {
		writeJSON(w, 400, "不能删除工作目录本身", nil)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeJSON(w, 400, "目标不存在", nil)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeJSON(w, 400, "删除失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

const maxTreeEntries = 3000

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".idea": true, "__pycache__": true,
	".venv": true, "venv": true, "dist": true, ".cache": true,
}

func (s *Server) handleFsTree(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		writeJSON(w, 401, "unauthorized", nil)
		return
	}
	root, err := s.realRoot()
	if err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	paths := make([]string, 0, 256)
	errStop := errors.New("stop")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != root && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		rel, e := filepath.Rel(root, p)
		if e != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			rel += "/"
		}
		paths = append(paths, rel)
		if len(paths) >= maxTreeEntries {
			return errStop
		}
		return nil
	})
	writeJSON(w, 0, "ok", map[string]any{"paths": paths})
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if code == 401 {
		status = http.StatusUnauthorized
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg, "data": data})
}
