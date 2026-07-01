package wechat

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"time"

	"claude-agent/internal/config"
)

const (
	// 登录扫码轮询参数。
	qrPollInterval = 2 * time.Second
	// 二维码刷新间隔:略短于 ClawBot 端 ~3min 过期,到点自动换新码,避免用户赛跑。
	qrRefreshInterval = 100 * time.Second
	// getupdates / 出码出错后的退避。
	pollBackoff = 3 * time.Second
)

// 账号状态。
const (
	StatusPending = "pending" // 等待扫码
	StatusOnline  = "online"  // 已登录,正常收发
	StatusOffline = "offline" // 未登录/已退出
)

// Channel 是一个微信 ClawBot 账号的接入通道:扫码登录 → 长轮询 → 分发到会话。
type Channel struct {
	id        string
	name      string
	cfg       config.Config
	client    *Client
	tokenPath string
	newBridge bridgeFactory // 可注入,便于测试;默认 defaultBridgeFactory

	mu        sync.Mutex
	status    string
	qrContent string // 当前待扫码的二维码内容(scanContent),在线后清空
}

// NewChannel 按配置构造单账号通道(向后兼容)。
func NewChannel(cfg config.Config) *Channel {
	tokenPath := cfg.WeChatTokenPath
	if tokenPath == "" {
		tokenPath = defaultTokenPath()
	}
	return newAccountChannel(cfg, "default", "默认账号", tokenPath)
}

// newAccountChannel 构造一个带 id/name/独立 token 路径的账号通道。
func newAccountChannel(cfg config.Config, id, name, tokenPath string) *Channel {
	return &Channel{
		id:        id,
		name:      name,
		cfg:       cfg,
		client:    NewClient(cfg.WeChatBaseURL),
		tokenPath: tokenPath,
		newBridge: defaultBridgeFactory,
		status:    StatusOffline,
	}
}

// sessionDir 返回该账号的 claude session_id 持久化目录:
// <tokenPath 所在目录>/sessions/<账号id>/(按账号隔离,tokenPath 为空则不持久化)。
func (c *Channel) sessionDir() string {
	if c.tokenPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.tokenPath), "sessions", c.id)
}

// ID/Name/Status/QR 暴露给管理层与 HTTP。
func (c *Channel) ID() string   { return c.id }
func (c *Channel) Name() string { return c.name }

func (c *Channel) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Channel) QR() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.qrContent
}

func (c *Channel) setStatus(s string) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

func (c *Channel) setQR(content string) {
	c.mu.Lock()
	c.qrContent = content
	c.mu.Unlock()
}

// Run 启动通道,阻塞直到 ctx 取消。
func (c *Channel) Run(ctx context.Context) error {
	maxSess := c.cfg.WeChatMaxSessions
	if maxSess <= 0 {
		maxSess = 20
	}
	sm := newSessionManager(ctx, c.cfg, c.client, maxSess)
	sm.sessionDir = c.sessionDir()
	if c.newBridge != nil {
		sm.newBridge = c.newBridge
	}
	go sm.reapLoop()
	defer sm.closeAll()
	defer c.setStatus(StatusOffline)

	if err := c.ensureLogin(ctx); err != nil {
		return err
	}
	c.setStatus(StatusOnline)
	c.setQR("")

	log.Printf("[wechat] 账号 %s 登录成功，开始接收消息", c.id)
	var cursor string
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := c.client.GetUpdates(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == ErrUnauthorized {
				log.Printf("[wechat] 账号 %s token 失效，重新登录", c.id)
				c.setStatus(StatusPending)
				if lerr := c.relogin(ctx); lerr != nil {
					return lerr
				}
				c.setStatus(StatusOnline)
				cursor = ""
				continue
			}
			log.Printf("[wechat] getupdates 出错，%s 后重试: %v", pollBackoff, err)
			if !sleepCtx(ctx, pollBackoff) {
				return ctx.Err()
			}
			continue
		}
		cursor = resp.GetUpdatesBuf
		for _, msg := range resp.Msgs {
			sm.Dispatch(msg)
		}
	}
}

// ensureLogin 优先用持久化 token;无 token 或失效则扫码登录。
func (c *Channel) ensureLogin(ctx context.Context) error {
	if token := loadToken(c.tokenPath); token != "" {
		c.client.SetToken(token)
		log.Printf("[wechat] 已从 %s 恢复 token", c.tokenPath)
		return nil
	}
	return c.relogin(ctx)
}

// relogin 走完整扫码登录流程并持久化新 token。
// 二维码到点(qrRefreshInterval)或被服务端判过期会自动重新出码,持续循环直到扫码成功或 ctx 取消。
func (c *Channel) relogin(ctx context.Context) error {
	c.client.SetToken("")
	c.setStatus(StatusPending)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		qr, err := c.client.GetBotQRCode(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[wechat] 获取二维码失败,%s 后重试: %v", pollBackoff, err)
			if !sleepCtx(ctx, pollBackoff) {
				return ctx.Err()
			}
			continue
		}
		c.setQR(qr.scanContent()) // 暴露给 Web 页面渲染二维码
		printQRCode(qr)           // 同时打日志(单账号/无页面场景)

		refreshAt := time.Now().Add(qrRefreshInterval)
		for time.Now().Before(refreshAt) {
			if !sleepCtx(ctx, qrPollInterval) {
				return ctx.Err()
			}
			st, err := c.client.GetQRCodeStatus(ctx, qr.pollKey())
			if err != nil {
				log.Printf("[wechat] 查询扫码状态出错: %v", err)
				continue
			}
			if st.confirmed() {
				token := st.token()
				if st.baseURL() != "" {
					c.client = NewClient(st.baseURL())
				}
				c.client.SetToken(token)
				c.setQR("")
				if err := saveToken(c.tokenPath, token); err != nil {
					log.Printf("[wechat] 保存 token 失败(不影响本次运行): %v", err)
				}
				return nil
			}
			if st.expired() {
				log.Printf("[wechat] 二维码已过期,自动重新出码")
				break
			}
		}
		log.Printf("[wechat] 二维码刷新,重新出码")
	}
}

// sleepCtx 睡眠 d,期间 ctx 取消则提前返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
