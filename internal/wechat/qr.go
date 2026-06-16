package wechat

import (
	"log"
	"os"

	"github.com/mdp/qrterminal/v3"
)

// printQRCode 在终端渲染登录二维码,供用户用微信扫码授权。
// 优先渲染二维码内容(qr.QRCode),回退到 URL;两者都打印明文便于排查。
func printQRCode(qr qrCodeResp) {
	content := qr.scanContent()
	if content == "" {
		log.Printf("[wechat] 未获取到二维码内容，无法登录")
		return
	}
	log.Printf("[wechat] 请用微信扫描下方二维码登录 ClawBot:")
	qrterminal.GenerateWithConfig(content, qrterminal.Config{
		Level:     qrterminal.M,
		Writer:    os.Stdout,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 1,
	})
	// 后台/journald 场景下 ASCII 二维码会被日志前缀打乱;同时给出可手动生成二维码或手机直接打开的 URL。
	log.Printf("[wechat] 扫码登录链接(手机打开或据此生成二维码): %s", content)
}
