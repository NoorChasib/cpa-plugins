#!/bin/bash
# Sign nested code inside out, preserving Sparkle's helper layout.
set -euo pipefail
app="$1"
identity="$2"
shift 2
framework="$app/Contents/Frameworks/Sparkle.framework"
for component in \
    "$framework/Versions/B/XPCServices/Installer.xpc" \
    "$framework/Versions/B/XPCServices/Downloader.xpc" \
    "$framework/Versions/B/Autoupdate" \
    "$framework/Versions/B/Updater.app" \
    "$framework" \
    "$app"; do
    codesign --force --sign "$identity" "$@" "$component"
done
