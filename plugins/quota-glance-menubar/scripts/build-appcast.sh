#!/bin/bash
# Run only after signing and notarizing the final DMG. Never mutate it afterward.
set +x
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
: "${SPARKLE_ED25519_PRIVATE_KEY:?Missing SPARKLE_ED25519_PRIVATE_KEY release secret}"
app='dist/Quota Glance.app'
version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")"
stage="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/quota-glance-appcast.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
printf '%s' "$SPARKLE_ED25519_PRIVATE_KEY" > "$stage/key"
# Fail before publishing if the CI secret doesn't match the key in the app.
xcrun swift scripts/check-update-key.swift "$stage/key" "$app/Contents/Info.plist"
mkdir "$stage/updates"
cp "dist/Quota-Glance-$version-macOS.dmg" "$stage/updates/"
dist/sparkle-tools/generate_appcast --ed-key-file "$stage/key" \
    --download-url-prefix "https://github.com/NoorChasib/cpa-plugins/releases/download/quota-glance-menubar/v$version/" \
    --maximum-deltas 0 "$stage/updates"
dist/sparkle-tools/sign_update --ed-key-file "$stage/key" --verify "$stage/updates/appcast.xml"
python3 scripts/publish-appcast.py --validate "$stage/updates/appcast.xml" --version "$version" --assets dist
cp "$stage/updates/appcast.xml" dist/appcast.xml
echo 'Signed update feed generated and verified for the final notarized DMG.'
