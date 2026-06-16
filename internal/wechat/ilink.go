package wechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// debugEnabled 由 AGENT_DEBUG 控制,开启后打印 iLink 原始请求/响应,便于联调字段。
var debugEnabled = os.Getenv("AGENT_DEBUG") != ""

// iLink 协议常量。这些是社区逆向得到的非官方契约,腾讯改协议可能失效。
const (
	defaultBaseURL = "https://ilinkai.weixin.qq.com"
	channelVersion = "1.0.2"
	botType        = "3"

	// getupdates 服务端最长保持约 35s,客户端超时需留余量。
	longPollTimeout = 60 * time.Second
	shortTimeout    = 15 * time.Second
)

// ErrUnauthorized 表示 bot_token 失效,需要重新扫码登录。
var ErrUnauthorized = fmt.Errorf("wechat: bot token unauthorized")

// Client 是 iLink HTTP 客户端。零依赖,仅用标准库 net/http。
type Client struct {
	baseURL  string
	botToken string
	longHTTP *http.Client
	httpc    *http.Client
}

// NewClient 创建客户端;baseURL 为空时用默认官方域名。
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		longHTTP: &http.Client{Timeout: longPollTimeout},
		httpc:    &http.Client{Timeout: shortTimeout},
	}
}

// SetToken 设置 bot_token(扫码成功或从文件恢复后调用)。
func (c *Client) SetToken(token string) { c.botToken = token }

// Token 返回当前 bot_token。
func (c *Client) Token() string { return c.botToken }

// randUIN 生成 X-WECHAT-UIN:随机 uint32 的 base64,每次请求重新生成。
func randUIN() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// newRequest 构造带统一 iLink 头的请求。
func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("wechat: marshal body: %w", err)
		}
		if debugEnabled {
			log.Printf("[wechat-debug] %s %s req=%s", method, path, clip(string(data), 800))
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("wechat: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("X-WECHAT-UIN", randUIN())
	if c.botToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.botToken)
	}
	return req, nil
}

// do 执行请求并把响应体反序列化到 out;HTTP 401/403 统一映射为 ErrUnauthorized。
func (c *Client) do(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("wechat: http do: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if debugEnabled {
		log.Printf("[wechat-debug] %s %s -> %d body=%s", req.Method, req.URL.Path, resp.StatusCode, clip(string(data), 800))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wechat: http %d: %s", resp.StatusCode, clip(string(data), 200))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("wechat: decode response: %w", err)
	}
	return nil
}

// GetBotQRCode 拉取登录二维码。
func (c *Client) GetBotQRCode(ctx context.Context) (qrCodeResp, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/ilink/bot/get_bot_qrcode?bot_type="+botType, nil)
	if err != nil {
		return qrCodeResp{}, err
	}
	var out qrCodeResp
	if err := c.do(c.httpc, req, &out); err != nil {
		return qrCodeResp{}, err
	}
	return out, nil
}

// GetQRCodeStatus 轮询扫码状态;confirmed 时返回带 bot_token 的结果。
func (c *Client) GetQRCodeStatus(ctx context.Context, qrcode string) (qrStatusResp, error) {
	path := "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return qrStatusResp{}, err
	}
	var out qrStatusResp
	if err := c.do(c.httpc, req, &out); err != nil {
		return qrStatusResp{}, err
	}
	return out, nil
}

// GetUpdates 长轮询拉取新消息;cursor 为上次返回的 get_updates_buf。
func (c *Client) GetUpdates(ctx context.Context, cursor string) (getUpdatesResp, error) {
	body := getUpdatesReq{GetUpdatesBuf: cursor, BaseInfo: baseInfo{ChannelVersion: channelVersion}}
	req, err := c.newRequest(ctx, http.MethodPost, "/ilink/bot/getupdates", body)
	if err != nil {
		return getUpdatesResp{}, err
	}
	var out getUpdatesResp
	if err := c.do(c.longHTTP, req, &out); err != nil {
		return getUpdatesResp{}, err
	}
	return out, nil
}

// SendMessage 发送一条文本回复。
func (c *Client) SendMessage(ctx context.Context, toUser, contextToken, text string) error {
	body := sendMessageReq{Msg: textMessage(toUser, contextToken, text)}
	req, err := c.newRequest(ctx, http.MethodPost, "/ilink/bot/sendmessage", body)
	if err != nil {
		return err
	}
	return c.do(c.httpc, req, nil)
}

// GetConfig 取某用户的 typing_ticket(typing 保活必需)。
func (c *Client) GetConfig(ctx context.Context, ilinkUserID string) (string, error) {
	body := getConfigReq{IlinkUserID: ilinkUserID, BaseInfo: baseInfo{ChannelVersion: channelVersion}}
	req, err := c.newRequest(ctx, http.MethodPost, "/ilink/bot/getconfig", body)
	if err != nil {
		return "", err
	}
	var out getConfigResp
	if err := c.do(c.httpc, req, &out); err != nil {
		return "", err
	}
	return out.TypingTicket, nil
}

// SendTyping 设置"正在输入"状态(status:1 开始 / 2 结束);失败不致命,调用方可忽略。
// typing 保活让长耗时回复仍落在 ClawBot 的回复窗口内,避免静默丢弃。
func (c *Client) SendTyping(ctx context.Context, ilinkUserID, typingTicket string, status int) error {
	body := sendTypingReq{IlinkUserID: ilinkUserID, TypingTicket: typingTicket, Status: status}
	req, err := c.newRequest(ctx, http.MethodPost, "/ilink/bot/sendtyping", body)
	if err != nil {
		return err
	}
	return c.do(c.httpc, req, nil)
}
