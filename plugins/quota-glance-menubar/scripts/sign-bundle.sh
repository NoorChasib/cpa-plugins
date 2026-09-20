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
    if [[ "$component" == */Downloader.xpc ]]; then
        codesign --force --sign "$identity" "$@" --preserve-metadata=entitlements "$component"
    else
        codesign --force --sign "$identity" "$@" "$component"
    fi
done
