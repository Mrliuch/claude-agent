#!/usr/bin/env bash
# 一键构建 claude-agent 部署包（在有 Go 环境的机器上执行）
#
# 用法：
#   ./build_package.sh                 # 默认目标平台 linux/amd64
#   GOOS=linux GOARCH=arm64 ./build_package.sh
#
# 产物：dist/claude-agent-<GOOS>-<GOARCH>.tar.gz
#   内含 claude-agent 二进制 + install.sh，拷到目标机解压后
#   sudo ./install.sh 即完成部署（systemd 托管、token 自动生成）。
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
command -v go >/dev/null || { echo "[打包失败] 未找到 Go，请先安装 golang" >&2; exit 1; }

GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
OUT_DIR=dist
PKG="claude-agent-${GOOS}-${GOARCH}.tar.gz"

echo "[打包] 编译 claude-agent (${GOOS}/${GOARCH}) version=${VERSION}..."
mkdir -p "$OUT_DIR"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o "$STAGE/claude-agent" ./cmd/claude-agent/
install -m 0755 install.sh "$STAGE/install.sh"

tar -czf "$OUT_DIR/$PKG" -C "$STAGE" claude-agent install.sh
echo "[打包] ✅ 产物: dist/$PKG"
echo
echo "目标机部署："
echo "  scp $OUT_DIR/$PKG root@<目标机>:/tmp/"
echo "  ssh root@<目标机> 'cd /tmp && tar xzf $PKG && sudo ./install.sh'"
