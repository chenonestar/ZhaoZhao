#!/usr/bin/env bash
# 在任意平台上交叉编译 Windows x64 版本，并打包成便携版目录。
#
# 用法: packaging/build-windows.sh [输出目录，默认 dist]
#
# 产物（dist/ZhaoZhao-portable/）：
#   zhaozhao.exe            主程序（同时兼作解析子进程）
#   components/libreoffice  格式转换组件（需要自行放入，见 components/README.txt）
#   components/ocr          OCR 组件 RapidOCR-json（需要自行放入）
#   README.txt
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="${1:-dist}"
VER="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

echo "==> 构建界面"
(cd web && npm ci --no-audit --no-fund && npm run build)

echo "==> 交叉编译 zhaozhao.exe ($VER)"
PKG="$OUT/ZhaoZhao-portable"
rm -rf "$PKG"
mkdir -p "$PKG/components/libreoffice" "$PKG/components/ocr"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -H=windowsgui -X main.version=$VER" \
  -o "$PKG/zhaozhao.exe" ./cmd/zhaozhao

cp packaging/components-README.txt "$PKG/components/README.txt"
cp packaging/portable-README.txt "$PKG/README.txt"
# 便携版标记：数据目录放在程序旁边而不是 %LOCALAPPDATA%
: > "$PKG/portable.flag"

(cd "$OUT" && rm -f "ZhaoZhao-portable-$VER.zip" && zip -qr "ZhaoZhao-portable-$VER.zip" ZhaoZhao-portable)
echo "==> 完成: $OUT/ZhaoZhao-portable-$VER.zip"
