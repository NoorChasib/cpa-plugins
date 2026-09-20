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
codesign --verify --strict "$app"
echo 'Bundle, menu bar mode, architectures and signature verified.'
