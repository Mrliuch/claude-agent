// smoke 是一次性诊断客户端：连接 claude-agent，发一个无工具提问，
// 校验 ready→assistant→result 链路。仅用于部署验证，不参与生产。
//
// 用法: smoke <ws_url> <token>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: smoke <ws_url> <token>")
		os.Exit(2)
	}
	url := os.Args[1] + "?token=" + os.Args[2]
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	defer c.Close()

	// idle 模式：只连不发，持续读取直到被服务端关闭，打印存活时长（验证空闲回收）
	if os.Getenv("SMOKE_IDLE") == "1" {
		start := time.Now()
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				fmt.Printf("IDLE_CLOSED after %.1fs: %v\n", time.Since(start).Seconds(), err)
				return
			}
		}
	}

	got := map[string]bool{"ready": false, "assistant": false, "result": false}

	// 可选第三参数=提问内容（用于测试工具/权限链路）；默认无工具问答
	prompt := "只回复两个字：你好。禁止使用任何工具。"
	if len(os.Args) >= 4 {
		prompt = os.Args[3]
	}
	// 像真实前端一样：连上即发消息，不等 ready
	if err := c.WriteJSON(map[string]any{"type": "user_message", "text": prompt}); err != nil {
		fmt.Println("发送失败:", err)
		os.Exit(1)
	}

	c.SetReadDeadline(time.Now().Add(75 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			fmt.Println("读取结束:", err)
			break
		}
		var ev map[string]any
		_ = json.Unmarshal(data, &ev)
		switch ev["type"] {
		case "ready":
			got["ready"] = true
			fmt.Println(">> ready, cwd =", ev["cwd"])
		case "assistant":
			if blocks, ok := ev["blocks"].([]any); ok {
				for _, b := range blocks {
					if blk, ok := b.(map[string]any); ok && blk["kind"] == "text" {
						if t, _ := blk["text"].(string); t != "" {
							got["assistant"] = true
							fmt.Println("ASSISTANT>", t)
						}
					}
				}
			}
		case "permission_request":
			fmt.Printf(">> permission_request: tool=%v input=%v → 自动放行\n", ev["tool_name"], ev["tool_input"])
			_ = c.WriteJSON(map[string]any{
				"type": "permission_response", "request_id": ev["request_id"],
				"allow": true, "tool_input": ev["tool_input"],
			})
			got["permission"] = true
		case "tool_result":
			fmt.Printf(">> tool_result: %v\n", ev["results"])
		case "result":
			got["result"] = true
			fmt.Println(">> result, is_error =", ev["is_error"])
			goto done
		case "error", "closed":
			fmt.Printf(">> %v: %v\n", ev["type"], ev)
		}
	}
done:
	fmt.Printf("CHECKS> %v\n", got)
	if got["ready"] && got["assistant"] && got["result"] {
		fmt.Println("PASS")
		os.Exit(0)
	}
	fmt.Println("PARTIAL")
	os.Exit(1)
}
