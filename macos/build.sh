#!/bin/bash
# 建置 Clode Myriagent.app（Intel 與 Apple 晶片通用）：Go 的 agent＋Swift 的視窗程式。
#   macos/build.sh                 臨時簽章（自己這台用）
#   SIGN_IDENTITY="Developer ID Application: …" macos/build.sh
#                                  用開發者帳號簽章（加 hardened runtime，可以送公證）
#   NOTARY_PROFILE=<notarytool 的 keychain profile> 再加上：送 Apple 公證並把結果釘進 app
# 產出：dist/Clode Myriagent.app 與 dist/Clode-Myriagent-mac.zip
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${VERSION:-0.3.0}
BUILD=$(git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
OUT=dist
APP="$OUT/Clode Myriagent.app"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "== Go agent"
for arch in amd64 arm64; do
  GOOS=darwin GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/kevinlincg/myriagent/internal/agent.Version=$VERSION -X github.com/kevinlincg/myriagent/internal/agent.Build=$BUILD" \
    -o "$TMP/myriagent-$arch" ./cmd/myriagent
done
lipo -create -output "$APP/Contents/MacOS/myriagent" "$TMP/myriagent-amd64" "$TMP/myriagent-arm64"

echo "== Swift 視窗程式"
for arch in x86_64 arm64; do
  swiftc -O -target "$arch-apple-macos13.0" -o "$TMP/shell-$arch" macos/main.swift
done
lipo -create -output "$APP/Contents/MacOS/Clode Myriagent" "$TMP/shell-x86_64" "$TMP/shell-arm64"

echo "== Info.plist 與圖示"
sed -e "s/__VERSION__/$VERSION/" -e "s/__BUILD__/$BUILD/" macos/Info.plist > "$APP/Contents/Info.plist"
ICONSET="$TMP/AppIcon.iconset"
mkdir -p "$ICONSET"
sips -s format png src/Myriagent/app.ico --out "$TMP/icon.png" >/dev/null 2>&1 || sips -s format png web/favicon.ico --out "$TMP/icon.png" >/dev/null
for s in 16 32 128 256 512; do
  sips -z $s $s "$TMP/icon.png" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
  d=$((s * 2))
  sips -z $d $d "$TMP/icon.png" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

echo "== 簽章"
if [ -n "${SIGN_IDENTITY:-}" ]; then
  # 內層先簽（agent），再簽整個 app；hardened runtime 是公證的條件
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$APP/Contents/MacOS/myriagent"
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$APP"
else
  codesign --force --deep --sign - "$APP"
fi
codesign --verify --deep --strict "$APP"

ditto -c -k --keepParent "$APP" "$OUT/Clode-Myriagent-mac.zip"
if [ -n "${NOTARY_PROFILE:-}" ] && [ -n "${SIGN_IDENTITY:-}" ]; then
  echo "== 公證"
  xcrun notarytool submit "$OUT/Clode-Myriagent-mac.zip" --keychain-profile "$NOTARY_PROFILE" --wait
  xcrun stapler staple "$APP"
  rm -f "$OUT/Clode-Myriagent-mac.zip"
  ditto -c -k --keepParent "$APP" "$OUT/Clode-Myriagent-mac.zip"
fi
echo "完成：${APP}（${VERSION}-${BUILD}）"
