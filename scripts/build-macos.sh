#!/usr/bin/env bash
# FFFVedioAgent macOS 打包：前端构建 → Go 编译 → 组装 .app（内置 ffmpeg）→ dmg + zip。
# 必须在 macOS 上执行（Wails 的 darwin 目标依赖 cgo 与系统框架，无法跨平台交叉编译）。
#
# 可选环境变量：
#   VERSION=0.0.1        覆盖版本号（默认读 wails.json 的 productVersion）
#   ARCH=arm64|x86_64    目标架构（默认当前机器架构）
#   SKIP_FRONTEND=1      跳过前端构建
#   SKIP_TESTS=1         跳过 go test
#   FFMPEG_SRC=/path     指定本地 ffmpeg 可执行文件，缺省时联网下载静态构建
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

APP_NAME="FFFVedioAgent"
VERSION="${VERSION:-$(sed -n 's/.*"productVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' wails.json)}"
ARCH="${ARCH:-$(uname -m)}"
BUILD_DIR="$ROOT/build/bin"
STAGE="$BUILD_DIR/stage-$ARCH"
APP="$STAGE/$APP_NAME.app"
CONTENTS="$APP/Contents"

case "$ARCH" in
  arm64)  GOARCH=arm64; FFARCH=arm64 ;;
  x86_64) GOARCH=amd64; FFARCH=x64   ;;
  *) echo "不支持的架构：$ARCH（可选 arm64 / x86_64）" >&2; exit 1 ;;
esac

echo "==> 版本 $VERSION / 架构 $ARCH"

if [[ "${SKIP_FRONTEND:-0}" != "1" ]]; then
  (
    cd frontend
    [[ -d node_modules ]] || npm install
    npm run build
  )
fi

if [[ "${SKIP_TESTS:-0}" != "1" ]]; then
  echo "==> Go 测试"
  go test ./...
fi

echo "==> 编译主程序"
rm -rf "$APP"
mkdir -p "$CONTENTS/MacOS" "$CONTENTS/Resources/bin"
CGO_ENABLED=1 GOOS=darwin GOARCH="$GOARCH" \
  go build -tags desktop,production -trimpath -ldflags "-s -w" \
  -o "$CONTENTS/MacOS/$APP_NAME" .

echo "==> 生成 Info.plist / PkgInfo"
cat > "$CONTENTS/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleName</key><string>$APP_NAME</string>
    <key>CFBundleDisplayName</key><string>FFFVedio 视频生成智能体</string>
    <key>CFBundleExecutable</key><string>$APP_NAME</string>
    <key>CFBundleIdentifier</key><string>com.fffvedioagent.app</string>
    <key>CFBundleIconFile</key><string>appicon</string>
    <key>CFBundleVersion</key><string>$VERSION</string>
    <key>CFBundleShortVersionString</key><string>$VERSION</string>
    <key>CFBundleGetInfoString</key><string>FFFVedioAgent $VERSION</string>
    <key>LSMinimumSystemVersion</key><string>10.15.0</string>
    <key>NSHighResolutionCapable</key><true/>
    <key>NSHumanReadableCopyright</key><string>Copyright © 2026 FFFVedioAgent</string>
    <key>NSSupportsAutomaticGraphicsSwitching</key><true/>
</dict>
</plist>
PLIST
printf 'APPL????' > "$CONTENTS/PkgInfo"

echo "==> 生成 FFF 图标（appicon.icns）"
ICONSET="$BUILD_DIR/appicon.iconset"
rm -rf "$ICONSET"
mkdir -p "$ICONSET"
SRC_PNG="$ROOT/build/appicon.png"
for size in 16 32 64 128 256 512 1024; do
  sips -z "$size" "$size" "$SRC_PNG" --out "$ICONSET/tmp-$size.png" >/dev/null
done
cp "$ICONSET/tmp-16.png"   "$ICONSET/icon_16x16.png"
cp "$ICONSET/tmp-32.png"   "$ICONSET/icon_16x16@2x.png"
cp "$ICONSET/tmp-32.png"   "$ICONSET/icon_32x32.png"
cp "$ICONSET/tmp-64.png"   "$ICONSET/icon_32x32@2x.png"
cp "$ICONSET/tmp-128.png"  "$ICONSET/icon_128x128.png"
cp "$ICONSET/tmp-256.png"  "$ICONSET/icon_128x128@2x.png"
cp "$ICONSET/tmp-256.png"  "$ICONSET/icon_256x256.png"
cp "$ICONSET/tmp-512.png"  "$ICONSET/icon_256x256@2x.png"
cp "$ICONSET/tmp-512.png"  "$ICONSET/icon_512x512.png"
cp "$ICONSET/tmp-1024.png" "$ICONSET/icon_512x512@2x.png"
rm -f "$ICONSET"/tmp-*.png
iconutil -c icns "$ICONSET" -o "$CONTENTS/Resources/appicon.icns"
rm -rf "$ICONSET"

echo "==> 内置 ffmpeg（应用启动时优先读取 Contents/Resources/bin）"
FF_DEST="$CONTENTS/Resources/bin/ffmpeg"
if [[ -n "${FFMPEG_SRC:-}" && -f "${FFMPEG_SRC}" ]]; then
  cp "$FFMPEG_SRC" "$FF_DEST"
elif [[ -f "$ROOT/build/deps/mac/ffmpeg-darwin-$FFARCH" ]]; then
  cp "$ROOT/build/deps/mac/ffmpeg-darwin-$FFARCH" "$FF_DEST"
else
  curl -sSL --fail --retry 3 -o "$FF_DEST" \
    "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0/ffmpeg-darwin-$FFARCH"
fi
chmod +x "$FF_DEST"
if [[ -f "${FFMPEG_SRC_DIR:-}/ffprobe" ]]; then
  cp "${FFMPEG_SRC_DIR}/ffprobe" "$CONTENTS/Resources/bin/ffprobe"
  chmod +x "$CONTENTS/Resources/bin/ffprobe"
fi

echo "==> ad-hoc 签名（分发自签名场景，首次打开需右键「打开」）"
codesign --force --deep --sign - "$APP"
xattr -cr "$APP" || true

DMG="$BUILD_DIR/$APP_NAME-$VERSION-macos-$ARCH.dmg"
ZIP="$BUILD_DIR/$APP_NAME-$VERSION-macos-$ARCH.zip"
rm -f "$DMG" "$ZIP"

echo "==> 生成 dmg"
hdiutil create -volname "$APP_NAME $VERSION" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null

echo "==> 生成 zip"
( cd "$STAGE" && ditto -c -k --sequesterRsrc --keepParent "$APP_NAME.app" "$ZIP" )

echo
echo "dmg：$DMG ($(du -h "$DMG" | cut -f1))"
echo "zip：$ZIP ($(du -h "$ZIP" | cut -f1))"
