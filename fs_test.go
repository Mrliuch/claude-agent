package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFsServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	real, _ := filepath.EvalSymlinks(dir)
	return NewServer(Config{Token: "t", WorkDir: real}), real
}

// ── 路径围栏（安全核心）────────────────────────────────────────────────────

func TestSafeResolveWithinRoot(t *testing.T) {
	s, root := newFsServer(t)
	got, err := s.safeResolve("a/b.txt")
	if err != nil {
		t.Fatalf("正常路径不应报错: %v", err)
	}
	if got != filepath.Join(root, "a/b.txt") {
		t.Fatalf("解析错误: %s", got)
	}
}

func TestSafeResolveNeutralizesDotDot(t *testing.T) {
	s, root := newFsServer(t)
	// ../ 越界应被中和为根相对，绝不逃出根
	for _, rel := range []string{"../etc/passwd", "../../../../etc/passwd", "/etc/passwd", "a/../../b"} {
		got, err := s.safeResolve(rel)
		if err != nil {
			continue // 报错也算安全
		}
		if got != root && !strings.HasPrefix(got, root+string(os.PathSeparator)) {
			t.Fatalf("%q 逃出了围栏: %s (root=%s)", rel, got, root)
		}
	}
}

func TestSafeResolveRejectsSymlinkEscape(t *testing.T) {
	s, root := newFsServer(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}
	if _, err := s.safeResolve("escape/secret.txt"); err == nil {
		t.Fatal("软链逃逸未被拦截")
	}
}

// ── 增删改查 + 鉴权（HTTP）──────────────────────────────────────────────────

func doFs(t *testing.T, srv *httptest.Server, method, path string, body string) map[string]any {
	t.Helper()
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	} else {
		req, _ = http.NewRequest(method, srv.URL+path, nil)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func TestFsCrudRoundtrip(t *testing.T) {
	s, _ := newFsServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	tk := "?token=t"

	// mkdir
	if r := doFs(t, ts, "POST", "/agent/fs/mkdir"+tk, `{"path":"sub"}`); r["code"].(float64) != 0 {
		t.Fatalf("mkdir 失败: %v", r)
	}
	// write
	if r := doFs(t, ts, "POST", "/agent/fs/write"+tk, `{"path":"sub/a.txt","content":"hello"}`); r["code"].(float64) != 0 {
		t.Fatalf("write 失败: %v", r)
	}
	// list
	r := doFs(t, ts, "GET", "/agent/fs/list"+tk+"&path=sub", "")
	if r["code"].(float64) != 0 {
		t.Fatalf("list 失败: %v", r)
	}
	entries := r["data"].(map[string]any)["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "a.txt" {
		t.Fatalf("list 结果错误: %v", entries)
	}
	// read
	r = doFs(t, ts, "GET", "/agent/fs/read"+tk+"&path=sub/a.txt", "")
	if r["code"].(float64) != 0 || r["data"].(map[string]any)["content"] != "hello" {
		t.Fatalf("read 错误: %v", r)
	}
	// delete
	if r := doFs(t, ts, "DELETE", "/agent/fs/delete"+tk+"&path=sub", ""); r["code"].(float64) != 0 {
		t.Fatalf("delete 失败: %v", r)
	}
	if r := doFs(t, ts, "GET", "/agent/fs/read"+tk+"&path=sub/a.txt", ""); r["code"].(float64) == 0 {
		t.Fatal("删除后仍能读取")
	}
}

func TestFsRejectsBadToken(t *testing.T) {
	s, _ := newFsServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	r := doFs(t, ts, "GET", "/agent/fs/list?token=wrong&path=", "")
	if r["code"].(float64) != 401 {
		t.Fatalf("错误 token 应 401: %v", r)
	}
}

func TestFsTree(t *testing.T) {
	s, root := newFsServer(t)
	os.MkdirAll(filepath.Join(root, "sub/deep"), 0o755)
	os.WriteFile(filepath.Join(root, "sub/deep/x.txt"), []byte("hi"), 0o644)
	os.MkdirAll(filepath.Join(root, "node_modules/pkg"), 0o755) // 应被跳过
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	r := doFs(t, ts, "GET", "/agent/fs/tree?token=t", "")
	if r["code"].(float64) != 0 {
		t.Fatalf("tree 失败: %v", r)
	}
	paths := r["data"].(map[string]any)["paths"].([]any)
	var has, hasNM bool
	for _, p := range paths {
		if p.(string) == "sub/deep/x.txt" {
			has = true
		}
		if strings.HasPrefix(p.(string), "node_modules") {
			hasNM = true
		}
	}
	if !has {
		t.Fatalf("tree 未含 sub/deep/x.txt: %v", paths)
	}
	if hasNM {
		t.Fatal("node_modules 未被跳过")
	}
}

func TestFsDownload(t *testing.T) {
	s, root := newFsServer(t)
	os.WriteFile(filepath.Join(root, "d.bin"), []byte("\x00\x01binary\xff"), 0o644)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// 正常下载（含二进制字节，不应被拒）
	resp, err := http.Get(ts.URL + "/agent/fs/download?token=t&path=d.bin")
	if err != nil {
		t.Fatalf("下载请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("缺少 attachment 头: %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "\x00\x01binary\xff" {
		t.Fatalf("内容不符: %q", body)
	}

	// 越权下载应被拦
	r := doFs(t, ts, "GET", "/agent/fs/download?token=t&path=../../../etc/passwd", "")
	if r == nil || r["code"] == nil || r["code"].(float64) == 0 {
		t.Fatal("越权/不存在下载应失败")
	}
}

func TestFsUpload(t *testing.T) {
	s, root := newFsServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	b64 := base64.StdEncoding.EncodeToString([]byte("uploaded\x00bytes"))
	// 正常上传
	r := doFs(t, ts, "POST", "/agent/fs/upload?token=t",
		`{"path":"up.bin","content_b64":"`+b64+`"}`)
	if r["code"].(float64) != 0 {
		t.Fatalf("上传失败: %v", r)
	}
	got, _ := os.ReadFile(filepath.Join(root, "up.bin"))
	if string(got) != "uploaded\x00bytes" {
		t.Fatalf("落地内容不符: %q", got)
	}
	// 越权上传应被拦
	r = doFs(t, ts, "POST", "/agent/fs/upload?token=t",
		`{"path":"../../../../tmp/evil","content_b64":"`+b64+`"}`)
	abs := "/tmp/evil"
	if _, err := os.Stat(abs); err == nil {
		os.Remove(abs)
		t.Fatal("越权上传逃出了围栏，写到了 /tmp/evil")
	}
}

func TestFsDeleteRootForbidden(t *testing.T) {
	s, _ := newFsServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	r := doFs(t, ts, "DELETE", "/agent/fs/delete?token=t&path=", "")
	if r["code"].(float64) == 0 {
		t.Fatal("不应允许删除工作目录本身")
	}
}
