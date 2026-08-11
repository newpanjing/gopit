#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS="-s -w -X main.version=${VERSION}"

mkdir -p bin

echo "Building pit..."
CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o bin/pit ./cmd/pit

echo "Done."
echo "  bin/pit  ($(du -h bin/pit | cut -f1))"
echo ""
echo "Usage:"
echo "  ./bin/pit start          # 后台启动服务端"
echo "  ./bin/pit stop           # 停止服务端"
echo "  ./bin/pit tui            # 前台管理界面"
echo "  ./bin/pit logs           # 查看服务端日志"
echo "  ./bin/pit join <token>   # 以 Token 加入"
