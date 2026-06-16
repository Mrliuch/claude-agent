package wechat

import (
	"context"
	"log"
	"time"

	"claude-agent/internal/config"
)

const (
	// 登录扫码轮询参数。
	qrPollInterval = 2 * time.Second
	qrPollTimeout  = 3 * time.Minute
	// getupdates 出错后的退避。
	pollBackoff = 3 * time.Second
)

// Channel 是微信 ClawBot 接入通道:扫码登录 → 长轮询 → 分发到会话。
type Channel struct {
	cfg       config.Config
	client    *Client
	tokenPath string
	newBridge bridgeFactory // 可注入,便于测试;默认 defaultBridgeFactory
}

// NewChannel 按配置构造通道。
func NewChannel(cfg config.Config) *Channel {
	tokenPath := cfg.WeChatTokenPath
	if tokenPath == "" {
		tokenPath = defaultTokenPath()
	}
	return &Channel{
		cfg:       cfg,
		client:    NewClient(cfg.WeChatBaseURL),
		tokenPath: tokenPath,
		newBridge: defaultBridgeFactory,
	}
}

// Run 启动通道,阻塞直到 ctx 取消。
func (c *Channel) Run(ctx context.Context) error {
	maxSess := c.cfg.WeChatMaxSessions
	if maxSess <= 0 {
		maxSess = 20
	}
	sm := newSessionManager(ctx, c.cfg, c.client, maxSess)
	if c.newBridge != nil {
		sm.newBridge = c.newBridge
	}
	go sm.reapLoop()
	defer sm.closeAll()

	if err := c.ensureLogin(ctx); err != nil {
		return err
	}

	log.Printf("[wechat] 登录成功，开始接收消息")
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
				log.Printf("[wechat] token 失效，重新登录")
				if lerr := c.relogin(ctx); lerr != nil {
					return lerr
				}
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
func (c *Channel) relogin(ctx context.Context) error {
	c.client.SetToken("")
	qr, err := c.client.GetBotQRCode(ctx)
	if err != nil {
		return err
	}
	printQRCode(qr)

	deadline := time.Now().Add(qrPollTimeout)
	for time.Now().Before(deadline) {
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
			if err := saveToken(c.tokenPath, token); err != nil {
				log.Printf("[wechat] 保存 token 失败(不影响本次运行): %v", err)
			}
			return nil
		}
	}
	return context.DeadlineExceeded
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
