#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "$(uname -s)" != Darwin ]]; then
    echo 'Building the app requires macOS 13+ and Xcode Command Line Tools.' >&2
    exit 1
fi

version="${VERSION:-0.5.0}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo 'VERSION must have three numeric components, such as 0.5.0.' >&2
    exit 1
fi

# Apple silicon (arm64) only; Intel and universal builds are not supported.
swift_bin="$(xcrun --find swift)"
"$swift_bin" build -c release --arch arm64 --scratch-path .build/arm64 --product QuotaGlance
bin_path="$("$swift_bin" build -c release --arch arm64 --scratch-path .build/arm64 --show-bin-path)"
sparkle_artifact="$PWD/.build/arm64/artifacts/sparkle/Sparkle"

# Stage separately so a failed build never removes the last usable bundle.
mkdir -p dist
stage="$(mktemp -d "$PWD/dist/.bundle.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
app="$stage/Quota Glance.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin_path/QuotaGlance" "$app/Contents/MacOS/QuotaGlance"
chmod +x "$app/Contents/MacOS/QuotaGlance"
mkdir -p "$app/Contents/Frameworks"
# Sparkle ships only a universal framework. `ditto --arch arm64` thins every
# Mach-O in it (Sparkle, Autoupdate, Updater.app and both XPC services) and
# copies symlinks as links. sign-bundle.sh below re-signs all of them.
ditto --arch arm64 "$sparkle_artifact/Sparkle.xcframework/macos-arm64_x86_64/Sparkle.framework" "$app/Contents/Frameworks/Sparkle.framework"
cp "$sparkle_artifact/LICENSE" "$app/Contents/Resources/Sparkle-LICENSE.txt"
# Use the exact tools whose archive SwiftPM verified, also for signing appcasts.
mkdir -p dist/sparkle-tools
ditto "$sparkle_artifact/bin" dist/sparkle-tools
cp Resources/QuotaReadout.js "$app/Contents/Resources/QuotaReadout.js"
# Provider badges. Swap a logo by replacing its SVG here (docs/logos.md).
mkdir -p "$app/Contents/Resources/Logos"
cp Resources/Logos/*.svg "$app/Contents/Resources/Logos/"
cp Resources/Info.plist "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$app/Contents/Info.plist"
"$swift_bin" scripts/make-icon.swift "$stage/AppIcon.iconset"
xcrun iconutil -c icns "$stage/AppIcon.iconset" -o "$app/Contents/Resources/AppIcon.icns"
bash scripts/sign-bundle.sh "$app" -
codesign --verify --deep --strict "$app"
rm -rf 'dist/Quota Glance.app'
mv "$app" 'dist/Quota Glance.app'
echo 'Built dist/Quota Glance.app'
