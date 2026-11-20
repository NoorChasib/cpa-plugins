#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
app="${1:-dist/Quota Glance.app}"
test -x "$app/Contents/MacOS/QuotaGlance"
test -s "$app/Contents/Resources/AppIcon.icns"
test -s "$app/Contents/Resources/QuotaReadout.js"
for logo in claude codex xai; do test -s "$app/Contents/Resources/Logos/$logo.svg"; done
plutil -lint "$app/Contents/Info.plist"
test "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")" = com.noorchasib.quota-glance-menubar
test "$(/usr/libexec/PlistBuddy -c 'Print :LSUIElement' "$app/Contents/Info.plist")" = true
test -s "$app/Contents/Resources/Sparkle-LICENSE.txt"
# Thinning must keep the framework's versioned symlink layout.
test "$(readlink "$app/Contents/Frameworks/Sparkle.framework/Versions/Current")" = B
test "$(readlink "$app/Contents/Frameworks/Sparkle.framework/Sparkle")" = Versions/Current/Sparkle
# Apple silicon only: each binary's architectures must be exactly arm64.
require_arm64() {
    local archs
    archs="$(xcrun lipo -archs "$1")"
    if [[ "$archs" != arm64 ]]; then
        echo "Expected only arm64 in $1, found: $archs" >&2
        exit 1
    fi
}
framework="$app/Contents/Frameworks/Sparkle.framework/Versions/B"
for binary in "$app/Contents/MacOS/QuotaGlance" "$framework/Sparkle" "$framework/Autoupdate" \
    "$framework/Updater.app/Contents/MacOS/Updater" \
    "$framework/XPCServices/Installer.xpc/Contents/MacOS/Installer" \
    "$framework/XPCServices/Downloader.xpc/Contents/MacOS/Downloader"; do
    test -x "$binary"
    require_arm64 "$binary"
done
# Also catch any other Mach-O, such as a helper added by a later Sparkle. Match
# the magic number rather than file(1): thin 64- and 32-bit in either byte
# order, fat and fat64. A Java class also starts cafebabe; lipo then fails.
found=0
# Listed into a file first: a find that fails part-way through a process
# substitution would not stop the script.
files="$(mktemp)"
trap 'rm -f "$files"' EXIT
find "$app" -type f -print0 > "$files"
while IFS= read -r -d '' file; do
    magic="$(od -An -N4 -tx1 "$file" | tr -dc '0-9a-f')"
    case "$magic" in
        cffaedfe|feedfacf|cefaedfe|feedface|cafebabe|bebafeca|cafebabf|bfbafeca)
            require_arm64 "$file"
            found=$((found + 1)) ;;
    esac
done < "$files"
# A scan that misses the six binaries named above is not checking anything.
if (( found < 6 )); then
    echo "The Mach-O scan found only $found binaries in $app." >&2
    exit 1
fi
otool -L "$app/Contents/MacOS/QuotaGlance" | grep -F '@rpath/Sparkle.framework/Versions/B/Sparkle'
otool -l "$app/Contents/MacOS/QuotaGlance" | grep -F '@executable_path/../Frameworks'
codesign --verify --deep --strict "$app"
echo 'Bundle, menu bar mode, arm64-only binaries and signature verified.'
