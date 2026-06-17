#!/usr/bin/env bash
# claude-agent 一键安装脚本（在目标服务器上以 root 执行）
#
# 用法（与 claude-agent 二进制放在同一目录后执行）：
#   sudo ./install.sh                          # 默认：监听 :8765，token 自动生成
#   sudo AGENT_TOKEN=xxx ./install.sh          # 指定共享 token
#   sudo RUN_USER=ops AGENT_LISTEN_ADDR=:9876 CLAUDE_WORK_DIR=/data ./install.sh
#
# 可用环境变量：
#   AGENT_TOKEN        与 monitor_v2 约定的共享鉴权 token（缺省自动生成随机串）
#   AGENT_LISTEN_ADDR  监听地址（默认 :8765）
#   RUN_USER           运行 agent 的系统用户（默认 root；该用户须能正常执行 claude）
#   CLAUDE_BIN         claude CLI 路径（默认在 RUN_USER 环境下自动探测）
#   CLAUDE_WORK_DIR    claude 工作目录围栏（默认 RUN_USER 的 HOME）
set -euo pipefail

INSTALL_DIR=/opt/claude-agent
UNIT_FILE=/etc/systemd/system/claude-agent.service
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

err() { echo "[安装失败] $*" >&2; exit 1; }
log() { echo "[claude-agent] $*"; }

[ "$(id -u)" = "0" ] || err "请用 root 执行：sudo ./install.sh"
[ -f "$SCRIPT_DIR/claude-agent" ] || err "未找到 claude-agent 二进制（须与本脚本同目录）"
command -v systemctl >/dev/null || err "未找到 systemd（systemctl），请参考 agent/README.md 手动部署"

RUN_USER="${RUN_USER:-root}"
id "$RUN_USER" >/dev/null 2>&1 || err "系统用户不存在: $RUN_USER"

# 前置检查：运行用户必须已配好 claude CLI（凭据/网关沿用其自身配置作共享默认）
CLAUDE_BIN="${CLAUDE_BIN:-$(su - "$RUN_USER" -c 'command -v claude' 2>/dev/null || true)}"
if [ -z "$CLAUDE_BIN" ]; then
    err "用户 $RUN_USER 环境中未找到 claude CLI。请先安装并配置 claude code（该用户手动执行 claude 能正常对话），或用 CLAUDE_BIN= 指定路径"
fi
log "claude CLI: $CLAUDE_BIN (运行用户: $RUN_USER)"

AGENT_TOKEN="${AGENT_TOKEN:-$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
AGENT_LISTEN_ADDR="${AGENT_LISTEN_ADDR:-:8765}"
CLAUDE_WORK_DIR="${CLAUDE_WORK_DIR:-$(eval echo "~$RUN_USER")}"

# 安装二进制与配置（env 文件 0600，token 不进 unit 文件）
mkdir -p "$INSTALL_DIR"
install -m 0755 "$SCRIPT_DIR/claude-agent" "$INSTALL_DIR/claude-agent"
cat > "$INSTALL_DIR/agent.env" <<EOF
AGENT_TOKEN=$AGENT_TOKEN
AGENT_LISTEN_ADDR=$AGENT_LISTEN_ADDR
CLAUDE_BIN=$CLAUDE_BIN
CLAUDE_WORK_DIR=$CLAUDE_WORK_DIR
EOF
chmod 0600 "$INSTALL_DIR/agent.env"

cat > "$UNIT_FILE" <<EOF
[Unit]
Description=claude-agent (monitor_v2 AI ops assistant)
After=network.target

[Service]
User=$RUN_USER
EnvironmentFile=$INSTALL_DIR/agent.env
ExecStart=$INSTALL_DIR/claude-agent
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now claude-agent
sleep 1

PORT="${AGENT_LISTEN_ADDR##*:}"
IP=$(hostname -I 2>/dev/null | awk '{print $1}')
if systemctl is-active --quiet claude-agent; then
    log "✅ 安装完成，服务已启动"
else
    systemctl status claude-agent --no-pager | tail -5 || true
    err "服务启动失败，请查看: journalctl -u claude-agent -n 50"
fi

cat <<EOF

────────────────────────────────────────────────────────
请把以下信息填入 monitor_v2「系统配置 → AI 运维助手 → 添加 Agent」：

  Agent 地址:  ws://${IP:-<本机IP>}:${PORT}/agent/chat
  共享 Token:  $AGENT_TOKEN

常用命令：
  systemctl status claude-agent      # 查看状态
  journalctl -u claude-agent -f      # 跟踪日志
  vi $INSTALL_DIR/agent.env && systemctl restart claude-agent   # 改配置

⚠️ 安全：请用防火墙/安全组将 ${PORT} 端口只放通 monitor_v2 后端所在机。
────────────────────────────────────────────────────────
EOF
