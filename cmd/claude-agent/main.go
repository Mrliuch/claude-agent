// claude-agent：部署在目标服务器上的轻量代理。
//
// 它以子进程方式驱动本机已部署的 claude code CLI（stream-json 控制协议），
// 对外暴露一个带共享 token 鉴权的 WebSocket 端点，并内置一个零依赖的 Web 控制台；
// 也可作为上游中继的后端被调用。
// 危险操作通过 permission_request 事件转发，由最终用户在浏览器弹窗确认。
package main

import (
	"context"
	"log"
	"os"

	"claude-agent/internal/config"
	"claude-agent/internal/server"
	"claude-agent/internal/wechat"
)

// version 由发布构建通过 -ldflags "-X main.version=vX.Y.Z" 注入；本地构建为 dev。
var version = "dev"

func main() {
	cfg, ok := config.ParseArgs(os.Args[1:], version)
	if !ok {
		os.Exit(0)
	}

	log.Printf("[claude-agent] version %s", version)

	if cfg.Token == "" {
		log.Fatal("[claude-agent] 必须设置 AGENT_TOKEN 或 --token（共享鉴权 token，客户端需携带）")
	}

	srv := server.NewServer(cfg)

	if cfg.WeChatEnabled {
		mgr := wechat.NewManager(context.Background(), cfg)
		mgr.Restore()
		srv.SetWeChat(mgr)
		log.Printf("[claude-agent] 微信多账号通道已启用")
	}

	if err := srv.Run(); err != nil {
		log.Fatalf("[claude-agent] 服务退出: %v", err)
	}
}
