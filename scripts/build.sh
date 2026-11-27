#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64|amd64) TARGET_ARCH="${TARGET_ARCH:-amd64}" ;;
  aarch64|arm64) TARGET_ARCH="${TARGET_ARCH:-arm64}" ;;
  *) TARGET_ARCH="${TARGET_ARCH:-amd64}" ;;
esac

case "$TARGET_ARCH" in
  arm64) FN_PLATFORM="arm" ;;
  *) FN_PLATFORM="x86" ;;
esac

APP_VERSION="$(grep -E '^version=' fpk/manifest | head -n1 | cut -d= -f2 | tr -d '\r ')"

echo "[1/5] 编译 Go 二进制 (linux/${TARGET_ARCH} v${APP_VERSION} -> platform=${FN_PLATFORM})..."
mkdir -p fpk/app/bin dist tools
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH="$TARGET_ARCH" go build -ldflags="-s -w -X fn-nspawn/internal/nspawn.AppVersion=${APP_VERSION}" -o fpk/app/bin/fn-nspawn .

echo "[2/5] 更新 manifest 架构为 platform=${FN_PLATFORM}..."
sed -i -E "s/^platform=.*/platform=${FN_PLATFORM}/" fpk/manifest

echo "[3/5] 规范化脚本与配置换行符和可执行权限..."
for f in fpk/manifest fpk/app/ui/config fpk/cmd/* fpk/config/* fpk/wizard/*; do
  [ -f "$f" ] || continue
  sed -i 's/\r$//' "$f"
done
for f in fpk/cmd/*; do
  [ -f "$f" ] || continue
  chmod 755 "$f"
done
chmod 755 fpk/app/bin/fn-nspawn

echo "[4/5] 检查 fnpack 打包工具..."
FNPACK_BIN="$(command -v fnpack || true)"
if [ -z "$FNPACK_BIN" ]; then
  FNPACK_BIN="$ROOT_DIR/tools/fnpack"
  if [ ! -x "$FNPACK_BIN" ]; then
    FNPACK_VER="${FNPACK_VER:-1.2.3}"
    URL="https://static2.fnnas.com/fnpack/fnpack-${FNPACK_VER}-linux-${TARGET_ARCH}"
    echo "未检测到 fnpack，正在下载: $URL"
    curl -fsSL "$URL" -o "$FNPACK_BIN"
    chmod 755 "$FNPACK_BIN"
  fi
fi

echo "[5/5] 构建 .fpk 安装包..."
rm -f fpk/*.fpk
(
  cd fpk
  "$FNPACK_BIN" build
)

OUT_NAME="fn-nspawn-v${APP_VERSION}-${FN_PLATFORM}.fpk"
mv -f fpk/*.fpk "dist/${OUT_NAME}"
ls -lh dist/*.fpk
echo "[OK] 打包完成，请将 dist/${OUT_NAME} 上传至飞牛「应用中心 -> 设置 -> 手动安装应用」"
