package wechat

import (
	"log"
	"os"

	"github.com/mdp/qrterminal/v3"
)

// printQRCode 在终端渲染登录二维码,供用户用微信扫码授权。
// 优先渲染二维码内容(qr.QRCode),回退到 URL;两者都打印明文便于排查。
func printQRCode(qr qrCodeResp) {
	content := qr.QRCode
	if content == "" {
		content = qr.URL
	}
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
	log.Printf("[wechat] 如终端无法显示，可手动打开二维码内容: %s", content)
}
