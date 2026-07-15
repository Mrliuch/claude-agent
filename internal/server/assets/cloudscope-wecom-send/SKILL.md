---
name: cloudscope-wecom-send
description: 通过 CloudScope 受控接口向已同步的企业微信员工发送文字、图片、图文、语音、视频或文件。仅在用户明确要求发送且完成发送前确认时使用。
---

# CloudScope 企业微信发送

只有当用户在当前对话中明确要求向企业微信发送消息时，才使用此 Skill。不要把“提醒一下”“通知团队”等模糊表述视为发送授权。

## 强制安全规则

1. 先向用户复述接收人、消息类型、内容摘要和附件名；在用户明确确认后才发送。
2. 只能使用当前会话注入的 `CLOUDSCOPE_WECOM_SEND_URL` 与 `CLOUDSCOPE_WECOM_SEND_TOKEN`。不得读取、输出或索要企业微信 CorpSecret、AgentId 或其他凭据。
3. 先用收件人搜索接口找到员工；搜索结果不唯一时必须向用户澄清，绝不能猜测。
4. 不在命令输出、任务记录或回复中展示 Authorization 值、完整企微 UserID 或上传文件内容。
5. 若环境变量不存在、接口拒绝权限或用户未确认，停止发送并解释原因。

## 收件人搜索

```bash
curl -sS -G "$CLOUDSCOPE_WECOM_SEND_URL/recipients" \
  -H "Authorization: Bearer $CLOUDSCOPE_WECOM_SEND_TOKEN" \
  --data-urlencode "q=<姓名或部门>"
```

从返回结果选取唯一的 `value` 作为 `<recipient_id>`；不要在面向用户的回复中回显该值。

## 发送方式

文字：

```bash
curl -sS -X POST "$CLOUDSCOPE_WECOM_SEND_URL/send" \
  -H "Authorization: Bearer $CLOUDSCOPE_WECOM_SEND_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"recipient_id":"<recipient_id>","message_type":"text","content":"<文字>"}'
```

图文最多 8 篇，每篇必须有标题和跳转 URL；封面 `picurl` 可选，均为 HTTP(S) 地址：

```bash
curl -sS -X POST "$CLOUDSCOPE_WECOM_SEND_URL/send" \
  -H "Authorization: Bearer $CLOUDSCOPE_WECOM_SEND_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"recipient_id":"<recipient_id>","message_type":"news","articles":[{"title":"<标题>","description":"<摘要>","url":"https://...","picurl":"https://..."}]}'
```

图片、语音、视频、文件使用 multipart 上传。允许：图片 JPG/JPEG/PNG（≤10MB）、语音 AMR（≤2MB）、视频 MP4（≤10MB）、文件（≤20MB）：

```bash
curl -sS -X POST "$CLOUDSCOPE_WECOM_SEND_URL/send" \
  -H "Authorization: Bearer $CLOUDSCOPE_WECOM_SEND_TOKEN" \
  -F "recipient_id=<recipient_id>" \
  -F "message_type=<image|voice|video|file>" \
  -F "file=@<local-file-path>"
```

平台会把素材临时上传给企业微信后立即发送；不要保存或复述其临时素材标识。
