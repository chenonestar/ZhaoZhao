#!/usr/bin/env bash
# 下载 PDFium（bblanchon/pdfium-binaries 预编译版，固定版本，便于复现）并放到指定目录。
# PDFium 只依赖系统自带的 DLL，不需要 VC++ 运行库。
#
# 用法: packaging/fetch-pdfium.sh <win|linux> <目标目录>
# 环境变量 PDFIUM_VERSION 可指定 chromium 版本号（默认见下）。
set -euo pipefail
VER="${PDFIUM_VERSION:-7811}"
case "${1:-}" in
  win) asset=pdfium-win-x64.tgz; lib=bin/pdfium.dll ;;
  linux) asset=pdfium-linux-x64.tgz; lib=lib/libpdfium.so ;;
  *) echo "用法: $0 <win|linux> <目标目录>" >&2; exit 2 ;;
esac
dest="$2"
mkdir -p "$dest"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
url="https://github.com/bblanchon/pdfium-binaries/releases/download/chromium%2F$VER/$asset"
for i in 1 2 3 4; do
  if curl -fsSL --retry 2 -o "$tmp/pdfium.tgz" "$url"; then break; fi
  [ "$i" = 4 ] && { echo "下载 PDFium 失败: $url" >&2; exit 1; }
  echo "第 $i 次下载失败，稍后重试"; sleep $((i * 10))
done
tar -xzf "$tmp/pdfium.tgz" -C "$tmp"
cp "$tmp/$lib" "$dest/"
cp "$tmp/LICENSE" "$dest/LICENSE.txt"
rm -rf "$dest/licenses" && cp -r "$tmp/licenses" "$dest/licenses"
echo "PDFium chromium/$VER → $dest/$(basename "$lib")"
