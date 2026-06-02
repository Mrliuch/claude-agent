# ── 构建阶段：编译静态二进制 ──────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/claude-agent ./cmd/claude-agent

# ── 运行阶段：内置 Node + claude code CLI ──────────────────────────────────
# 注意：容器内的 claude 只能访问容器内部。若要排查“宿主机”问题，
# 推荐宿主机直跑二进制（见 README），或给容器挂载宿主机目录 / 用 host 网络。
FROM node:20-alpine

RUN npm install -g @anthropic-ai/claude-code \
    && apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai

COPY --from=builder /out/claude-agent /usr/local/bin/claude-agent

EXPOSE 8765
ENV AGENT_LISTEN_ADDR=:8765

ENTRYPOINT ["/usr/local/bin/claude-agent"]
