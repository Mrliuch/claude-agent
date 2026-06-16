package wechat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"claude-agent/internal/config"
)

// AccountInfo 是给 HTTP/页面用的账号摘要。
type AccountInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Manager 管理多个微信账号(每个账号 = 一个独立 bot_token 的 Channel)。
type Manager struct {
	cfg     config.Config
	dir     string
	rootCtx context.Context

	mu       sync.Mutex
	accounts map[string]*Channel
	cancels  map[string]context.CancelFunc
}

// NewManager 创建管理器;账号 token 存放目录默认 ~/.config/claude-agent/wechat/。
func NewManager(ctx context.Context, cfg config.Config) *Manager {
	dir := cfg.WeChatTokenPath
	if dir == "" {
		dir = accountsDir()
	} else {
		dir = filepath.Dir(dir) // 若配置的是文件路径,用其所在目录放多账号
	}
	return &Manager{
		cfg:      cfg,
		dir:      dir,
		rootCtx:  ctx,
		accounts: make(map[string]*Channel),
		cancels:  make(map[string]context.CancelFunc),
	}
}

// accountsDir 返回默认账号目录。
func accountsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "wechat-accounts"
	}
	return filepath.Join(home, ".config", "claude-agent", "wechat")
}

// Restore 启动时恢复目录下所有已保存账号(用已存 token 自动登录)。
func (m *Manager) Restore() {
	m.migrateLegacy()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return // 目录不存在 = 还没有账号
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".token") {
			continue
		}
		id := strings.TrimSuffix(name, ".token")
		m.startAccount(id, m.loadName(id))
	}
}

// migrateLegacy 把旧单账号 token 文件迁移成 default 账号。
func (m *Manager) migrateLegacy() {
	legacy := m.cfg.WeChatTokenPath
	if legacy == "" {
		legacy = defaultTokenPath()
	}
	if legacy == "" {
		return
	}
	if filepath.Dir(legacy) == m.dir && strings.HasSuffix(legacy, ".token") {
		return // 已是新目录结构
	}
	data, err := os.ReadFile(legacy)
	if err != nil {
		return
	}
	dst := m.tokenPath("default")
	if _, err := os.Stat(dst); err == nil {
		return // 已迁移
	}
	if err := saveToken(dst, strings.TrimSpace(string(data))); err == nil {
		m.saveName("default", "默认账号")
		log.Printf("[wechat] 已迁移旧账号 token → %s", dst)
	}
}

func (m *Manager) tokenPath(id string) string { return filepath.Join(m.dir, id+".token") }
func (m *Manager) namePath(id string) string  { return filepath.Join(m.dir, id+".name") }

func (m *Manager) loadName(id string) string {
	if data, err := os.ReadFile(m.namePath(id)); err == nil {
		if n := strings.TrimSpace(string(data)); n != "" {
			return n
		}
	}
	return "微信账号-" + shortID(id)
}

func (m *Manager) saveName(id, name string) {
	_ = os.MkdirAll(m.dir, 0o700)
	_ = os.WriteFile(m.namePath(id), []byte(name), 0o600)
}

// startAccount 启动一个账号通道(在 m.mu 外调用)。
func (m *Manager) startAccount(id, name string) {
	m.mu.Lock()
	if _, ok := m.accounts[id]; ok {
		m.mu.Unlock()
		return
	}
	ch := newAccountChannel(m.cfg, id, name, m.tokenPath(id))
	ctx, cancel := context.WithCancel(m.rootCtx)
	m.accounts[id] = ch
	m.cancels[id] = cancel
	m.mu.Unlock()

	go func() {
		if err := ch.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[wechat] 账号 %s 通道退出: %v", id, err)
		}
	}()
}

// Add 新增一个账号并触发扫码登录,返回账号 id。
func (m *Manager) Add(name string) string {
	id := newID()
	if strings.TrimSpace(name) == "" {
		name = "微信账号-" + shortID(id)
	}
	m.saveName(id, name)
	m.startAccount(id, name)
	return id
}

// List 返回所有账号摘要(按名称排序)。
func (m *Manager) List() []AccountInfo {
	m.mu.Lock()
	out := make([]AccountInfo, 0, len(m.accounts))
	for _, ch := range m.accounts {
		out = append(out, AccountInfo{ID: ch.ID(), Name: ch.Name(), Status: ch.Status()})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// QR 返回某账号当前待扫码二维码内容;不存在或已在线则 ok=false。
func (m *Manager) QR(id string) (string, bool) {
	m.mu.Lock()
	ch, ok := m.accounts[id]
	m.mu.Unlock()
	if !ok {
		return "", false
	}
	content := ch.QR()
	return content, content != ""
}

// Remove 登出并移除账号(取消通道、删除 token 与名称文件)。
func (m *Manager) Remove(id string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	delete(m.accounts, id)
	delete(m.cancels, id)
	m.mu.Unlock()
	if ok {
		cancel()
	}
	_ = os.Remove(m.tokenPath(id))
	_ = os.Remove(m.namePath(id))
	return ok
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}
