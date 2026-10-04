#!/usr/bin/env bash
# 构建所有平台的发布包，放到 dist/。
#   用法：scripts/build.sh 1.0.0
# 发版时由 .github/workflows/release.yml 在 Ubuntu 上调用；本地用 Git Bash / macOS / Linux 也能跑（需要 zip）。
#
# 资产名里带上系统和架构（win-x64、mac-arm64 …），工具库靠它自动给访客挑对的安装包。
set -euo pipefail
cd "$(dirname "$0")/.."

VER="${1:-dev}"
NAME=LanTransfer
rm -rf dist build
mkdir -p dist build

# Windows 程序的图标和「属性 → 详细信息」里的版本号
go install github.com/tc-hib/go-winres@v0.3.3
WINRES_VER="$VER"
[[ "$VER" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || WINRES_VER=0.0.0
"$(go env GOPATH)/bin/go-winres" simply \
  --icon winres/icon.png --manifest cli --arch amd64,arm64 \
  --product-name "文件传输助手" --file-description "文件传输助手 · 局域网互传文件" \
  --product-version "$WINRES_VER" --file-version "$WINRES_VER" \
  --copyright "© 2026 haoawake · MIT License" --original-filename "$NAME.exe"
trap 'rm -f rsrc_windows_*.syso' EXIT

LDFLAGS="-s -w -X main.version=$VER"
build() { CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "$LDFLAGS" -o "$3" .; }
label() { [ "$1" = amd64 ] && echo x64 || echo "$1"; }

# Windows：单个 exe，下载就能双击
for arch in amd64 arm64; do
  build windows $arch "dist/$NAME-win-$(label $arch).exe"
done

# macOS：zip 里一个可执行文件（zip 会保留执行权限，双击就在「终端」里运行）
for arch in arm64 amd64; do
  d="build/mac-$(label $arch)"
  mkdir -p "$d"
  build darwin $arch "$d/$NAME"
  (cd "$d" && zip -q -X "../../dist/$NAME-mac-$(label $arch).zip" "$NAME")
done

# Linux
for arch in amd64 arm64; do
  d="build/linux-$(label $arch)"
  mkdir -p "$d"
  build linux $arch "$d/$NAME"
  tar -czf "dist/$NAME-linux-$(label $arch).tar.gz" -C "$d" "$NAME"
done

(cd dist && sha256sum -- * > SHA256SUMS.txt)
ls -l dist
