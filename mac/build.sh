#!/bin/sh
# Builds Mem.app (universal: Apple silicon + Intel) with the matching `mem` binary inside.
#   mac/build.sh                 → mac/dist/Mem.app and mac/dist/Mem.zip
#   mac/build.sh --install       → also copies it to ~/Applications and starts it
set -eu
cd "$(dirname "$0")"
VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}
APP=dist/Mem.app
rm -rf dist && mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Helpers" "$APP/Contents/Resources"

echo "· building app"
swift build -c release --arch arm64 --arch x86_64 >/dev/null
cp .build/apple/Products/Release/Mem "$APP/Contents/MacOS/Mem"

echo "· building mem $VERSION"
for arch in arm64 amd64; do
  (cd .. && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "mac/dist/mem-$arch" ./cmd/mem)
done
# Helpers/, not MacOS/: the file system is case-insensitive, so mem would overwrite Mem.
lipo -create dist/mem-arm64 dist/mem-amd64 -output "$APP/Contents/Helpers/mem"
rm dist/mem-arm64 dist/mem-amd64

echo "· icon"
ICONSET=dist/Mem.iconset && mkdir -p $ICONSET
swift scripts/icon.swift dist/icon.png
for s in 16 32 128 256 512; do
  sips -z $s $s dist/icon.png --out $ICONSET/icon_${s}x${s}.png >/dev/null
  sips -z $((s*2)) $((s*2)) dist/icon.png --out $ICONSET/icon_${s}x${s}@2x.png >/dev/null
done
iconutil -c icns $ICONSET -o "$APP/Contents/Resources/Mem.icns"
rm -rf $ICONSET dist/icon.png

SHORT=$(echo "$VERSION" | sed -E 's/^v//; s/-.*//')
case "$SHORT" in [0-9]*) ;; *) SHORT=0.0.0 ;; esac
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Mem</string>
  <key>CFBundleDisplayName</key><string>Mem</string>
  <key>CFBundleIdentifier</key><string>com.github.introvert.memcli</string>
  <key>CFBundleExecutable</key><string>Mem</string>
  <key>CFBundleIconFile</key><string>Mem</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$SHORT</string>
  <key>CFBundleVersion</key><string>$SHORT</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

codesign --force --deep --sign - "$APP" 2>/dev/null
(cd dist && ditto -c -k --keepParent Mem.app Mem.zip)
echo "built mac/$APP ($(du -sh $APP | cut -f1))"

if [ "${1:-}" = "--install" ]; then
  mkdir -p ~/Applications
  pkill -x Mem 2>/dev/null || true
  rm -rf ~/Applications/Mem.app && cp -R "$APP" ~/Applications/
  open ~/Applications/Mem.app
  echo "installed ~/Applications/Mem.app"
fi
