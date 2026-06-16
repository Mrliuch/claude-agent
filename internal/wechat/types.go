package wechat

import "strings"

// iLink 消息项类型。当前 ClawBot 仅支持文本(type=1)。
const itemTypeText = 1

// 消息方向/状态(sendmessage 用)。
const (
	msgTypeReply  = 2 // bot → user 文本回复
	msgStateFinal = 2 // 完整一条(非流式增量)
)

// TextItem 文本内容载体。
type TextItem struct {
	Text string `json:"text"`
}

// Item 消息项;文本消息只用 TextItem。
type Item struct {
	Type     int       `json:"type"`
	TextItem *TextItem `json:"text_item,omitempty"`
}

// Message 是 iLink 收发消息的统一结构体。
// 收消息(getupdates)关心 FromUserID/ContextToken/ItemList;
// 发消息(sendmessage)填 ToUserID/MessageType/MessageState/ContextToken/ItemList。
type Message struct {
	FromUserID   string `json:"from_user_id,omitempty"`
	ToUserID     string `json:"to_user_id,omitempty"`
	MessageType  int    `json:"message_type,omitempty"`
	MessageState int    `json:"message_state,omitempty"`
	ContextToken string `json:"context_token,omitempty"`
	ItemList     []Item `json:"item_list,omitempty"`
}

// Text 把所有文本项拼成一个字符串。
func (m Message) Text() string {
	var b strings.Builder
	for _, it := range m.ItemList {
		if it.TextItem != nil {
			b.WriteString(it.TextItem.Text)
		}
	}
	return b.String()
}

// textMessage 构造一条文本回复消息。
func textMessage(toUser, contextToken, text string) Message {
	return Message{
		ToUserID:     toUser,
		MessageType:  msgTypeReply,
		MessageState: msgStateFinal,
		ContextToken: contextToken,
		ItemList:     []Item{{Type: itemTypeText, TextItem: &TextItem{Text: text}}},
	}
}

// --- 请求/响应包体 ---

type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

type getUpdatesReq struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      baseInfo `json:"base_info"`
}

type getUpdatesResp struct {
	Msgs          []Message `json:"msgs"`
	GetUpdatesBuf string    `json:"get_updates_buf"`
}

type sendMessageReq struct {
	Msg Message `json:"msg"`
}

// sendTypingReq 控制"正在输入"状态;status:1=开始,2=结束。
type sendTypingReq struct {
	ToUserID     string `json:"to_user_id"`
	ContextToken string `json:"context_token,omitempty"`
	Status       int    `json:"status"`
}

// qrCodeResp 扫码登录第一步返回。真实字段(已抓包核实):
//
//	qrcode             轮询 get_qrcode_status 用的 key
//	qrcode_img_content 真正给微信扫描的 URL(https://liteapp.weixin.qq.com/q/...)
type qrCodeResp struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
	URL              string `json:"url"`
	Ret              int    `json:"ret"`
}

// scanContent 返回应编进二维码、供微信扫描的内容(优先 img_content)。
func (q qrCodeResp) scanContent() string {
	if q.QRCodeImgContent != "" {
		return q.QRCodeImgContent
	}
	if q.URL != "" {
		return q.URL
	}
	return q.QRCode
}

// pollKey 返回轮询扫码状态用的 key。
func (q qrCodeResp) pollKey() string { return q.QRCode }

// qrStatusResp 扫码状态轮询返回。真实字段:{"ret":0,"status":"wait"};
// 确认后带回 token(字段名以拿到 token 为准,兼容多种命名)。
type qrStatusResp struct {
	Ret      int    `json:"ret"`
	Status   string `json:"status"`
	BotToken string `json:"bot_token"`
	Token    string `json:"token"`
	BaseURL  string `json:"baseurl"`
	BaseURL2 string `json:"base_url"`
}

// token 返回 bot_token(兼容 bot_token / token)。
func (s qrStatusResp) token() string {
	if s.BotToken != "" {
		return s.BotToken
	}
	return s.Token
}

// baseURL 返回接入域名(兼容 baseurl / base_url)。
func (s qrStatusResp) baseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return s.BaseURL2
}

// confirmed 以是否拿到 token 判定扫码确认完成(比依赖 status 字符串更稳)。
func (s qrStatusResp) confirmed() bool { return s.token() != "" }
