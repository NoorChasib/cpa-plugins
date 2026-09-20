#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
app="${1:-dist/Quota Glance.app}"
test -x "$app/Contents/MacOS/QuotaGlance"
test -s "$app/Contents/Resources/AppIcon.icns"
test -s "$app/Contents/Resources/QuotaReadout.js"
plutil -lint "$app/Contents/Info.plist"
test "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")" = com.noorchasib.quota-glance-menubar
test "$(/usr/libexec/PlistBuddy -c 'Print :LSUIElement' "$app/Contents/Info.plist")" = true
read -r -a architectures <<< "${APP_ARCHS:-arm64 x86_64}"
for arch in "${architectures[@]}"; do
    xcrun lipo "$app/Contents/MacOS/QuotaGlance" -verify_arch "$arch"
done
test -s "$app/Contents/Resources/Sparkle-LICENSE.txt"
framework="$app/Contents/Frameworks/Sparkle.framework"
for binary in "$framework/Sparkle" "$framework/Versions/B/Autoupdate" "$framework/Versions/B/Updater.app/Contents/MacOS/Updater"; do
    test -x "$binary"
    for arch in "${architectures[@]}"; do xcrun lipo "$binary" -verify_arch "$arch"; done
done
otool -L "$app/Contents/MacOS/QuotaGlance" | grep -F '@rpath/Sparkle.framework/Versions/B/Sparkle'
otool -l "$app/Contents/MacOS/QuotaGlance" | grep -F '@executable_path/../Frameworks'
codesign --verify --deep --strict "$app"
echo 'Bundle, menu bar mode, architectures and signature verified.'
