#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "$(uname -s)" != Darwin ]]; then
    echo 'Creating the DMG requires macOS and hdiutil.' >&2
    exit 1
fi

app='dist/Quota Glance.app'
bash scripts/verify-bundle.sh "$app"
# Name the disk image after the version actually sealed into the app.
version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo 'The app bundle must have a three-component numeric version.' >&2
    exit 1
fi

stage="$(mktemp -d "$PWD/dist/.dmg.XXXXXX")"
mounted=0
cleanup() {
    if [[ "$mounted" == 1 ]]; then
        # Do not remove files beneath a volume that failed to detach.
        if ! hdiutil detach "$stage/mount" >/dev/null; then
            echo "The verification volume is still mounted at $stage/mount." >&2
            return
        fi
    fi
    rm -rf "$stage"
}
trap cleanup EXIT

mkdir -p "$stage/payload" "$stage/mount"
ditto "$app" "$stage/payload/Quota Glance.app"
ln -s /Applications "$stage/payload/Applications"
cat > "$stage/payload/Install.txt" <<'EOF'
Install Quota Glance

1. Drag Quota Glance.app to Applications.
2. Eject this disk image, then open Quota Glance from Applications.
3. Paste your dashboard URL and sign in on the page.
4. Right-click the menu bar icon, open Settings, and enable Open at login.

Open at login starts Quota Glance when you sign in after restarting your Mac.
Your dashboard URL and saved web session are retained between launches.
If macOS asks for login-item approval, allow Quota Glance in System Settings
under General > Login Items.

Requires macOS 13 or newer.
EOF
if [[ "${NOTARIZED_RELEASE:-0}" == 1 ]]; then
    xcrun stapler validate "$app"
    printf '\nThis release is Developer ID signed and notarized by Apple.\n' >> "$stage/payload/Install.txt"
else
    cat >> "$stage/payload/Install.txt" <<'EOF'

This personal build is ad-hoc signed and is not notarized with Apple.
Downloaded builds may require explicit approval in System Settings >
Privacy & Security before their first launch.
EOF
fi

hdiutil create -volname 'Quota Glance' -srcfolder "$stage/payload" \
    -fs HFS+ -format UDZO "$stage/Quota-Glance.dmg"
hdiutil verify "$stage/Quota-Glance.dmg"
hdiutil attach "$stage/Quota-Glance.dmg" -readonly -nobrowse -noautoopen \
    -mountpoint "$stage/mount"
mounted=1
bash scripts/verify-bundle.sh "$stage/mount/Quota Glance.app"
test "$(readlink "$stage/mount/Applications")" = /Applications
test -s "$stage/mount/Install.txt"
hdiutil detach "$stage/mount"
mounted=0

# Publish only after the image and its mounted payload pass verification.
destination="dist/Quota-Glance-$version-macOS.dmg"
mv -f "$stage/Quota-Glance.dmg" "$destination"
echo "Created $destination"
