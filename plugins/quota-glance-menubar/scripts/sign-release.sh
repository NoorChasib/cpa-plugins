#!/bin/bash
# Only used for trusted release tags, after the normal build and checks pass.
set +x
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."

if [[ "$(uname -s)" != Darwin ]]; then
    echo 'Developer ID signing and notarization require macOS.' >&2
    exit 1
fi
for name in MACOS_CERTIFICATE_P12_BASE64 MACOS_CERTIFICATE_PASSWORD APPLE_ID APPLE_APP_SPECIFIC_PASSWORD APPLE_TEAM_ID; do
    if [[ -z "${!name:-}" ]]; then
        echo "Missing release secret: $name. See docs/apple-signing.md." >&2
        exit 1
    fi
done
if [[ ! "$APPLE_TEAM_ID" =~ ^[A-Z0-9]{10}$ ]]; then
    echo 'APPLE_TEAM_ID must be your 10-character Apple Developer team ID.' >&2
    exit 1
fi

app='dist/Quota Glance.app'
bash scripts/verify-bundle.sh "$app"
version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo 'The app bundle must have a three-component numeric version.' >&2
    exit 1
fi

stage="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/quota-glance-sign.XXXXXX")"
keychain="$stage/signing.keychain-db"
keychain_password="$(openssl rand -hex 32)"
profile=quota-glance-notary
search_list_changed=0
original_keychains=()
cleanup() {
    if [[ "$search_list_changed" == 1 ]]; then
        security list-keychains -d user -s "${original_keychains[@]}" || true
    fi
    security delete-keychain "$keychain" >/dev/null 2>&1 || true
    rm -rf "$stage"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Read the existing search list rather than replacing the runner's keychains.
security list-keychains -d user > "$stage/keychains.txt"
while IFS= read -r path; do
    original_keychains+=("$path")
done < <(python3 -c 'import shlex, sys; print("\n".join(shlex.split(open(sys.argv[1]).read())))' "$stage/keychains.txt")

python3 - "$stage/certificate.p12" <<'PY'
import base64
import os
import pathlib
import sys

encoded = "".join(os.environ["MACOS_CERTIFICATE_P12_BASE64"].split())
pathlib.Path(sys.argv[1]).write_bytes(base64.b64decode(encoded, validate=True))
PY
security create-keychain -p "$keychain_password" "$keychain"
security set-keychain-settings -lut 7200 "$keychain"
security unlock-keychain -p "$keychain_password" "$keychain"
security import "$stage/certificate.p12" -k "$keychain" -P "$MACOS_CERTIFICATE_PASSWORD" \
    -T /usr/bin/codesign -T /usr/bin/security >/dev/null
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$keychain_password" "$keychain" >/dev/null
security list-keychains -d user -s "$keychain" "${original_keychains[@]}"
search_list_changed=1
rm "$stage/certificate.p12"

# Select only the imported Developer ID Application certificate for this team.
# A wrong certificate type or a P12 without its private key fails here.
identity="$(security find-identity -v -p codesigning "$keychain" | python3 -c '
import os, re, sys
pattern = r"([A-Fa-f0-9]{40}) \"Developer ID Application: .* \(" + re.escape(os.environ["APPLE_TEAM_ID"]) + r"\)\""
matches = {match.group(1) for match in re.finditer(pattern, sys.stdin.read())}
if len(matches) != 1:
    sys.exit("Expected exactly one valid Developer ID Application identity for APPLE_TEAM_ID, including its private key.")
print(matches.pop())
')"

xcrun notarytool store-credentials "$profile" --keychain "$keychain" \
    --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_SPECIFIC_PASSWORD"

notarize() {
    local artifact="$1" label="$2" result="$stage/$2-notary.json" submitted=0
    xcrun notarytool submit "$artifact" --keychain-profile "$profile" --keychain "$keychain" \
        --wait --timeout 30m --output-format json > "$result" || submitted=$?
    if [[ "$submitted" != 0 ]]; then
        cat "$result"
        echo "Notarization did not finish for $label; no release will be published." >&2
        return "$submitted"
    fi
    # A completed request is not necessarily accepted by Apple.
    if ! python3 - "$result" <<'PY'
import json
import sys

result = json.load(open(sys.argv[1]))
if result.get("status") != "Accepted":
    sys.exit("Apple notarization status: " + str(result.get("status", "missing")))
PY
    then
        local submission
        submission="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$result")"
        xcrun notarytool log "$submission" --keychain-profile "$profile" --keychain "$keychain" || true
        return 1
    fi
}

# Hardened runtime and a secure timestamp are required for notarization. WebKit
# owns its rendering processes; the host app needs no custom JIT entitlements.
codesign --force --sign "$identity" --keychain "$keychain" --options runtime --timestamp "$app"
codesign --verify --strict "$app"
ditto -c -k --sequesterRsrc --keepParent "$app" "$stage/app.zip"
notarize "$stage/app.zip" app
xcrun stapler staple "$app"
xcrun stapler validate "$app"
spctl --assess --type execute --verbose=2 "$app"

# ZIPs cannot carry a stapled ticket themselves. Package the stapled app again;
# never rebuild it here, because a rebuild would replace the signed executable.
ditto -c -k --sequesterRsrc --keepParent "$app" "dist/Quota-Glance-$version-macOS.zip"
NOTARIZED_RELEASE=1 bash scripts/build-dmg.sh
dmg="dist/Quota-Glance-$version-macOS.dmg"
codesign --force --sign "$identity" --keychain "$keychain" --timestamp "$dmg"
notarize "$dmg" dmg
xcrun stapler staple "$dmg"
xcrun stapler validate "$dmg"
codesign --verify --strict "$dmg"
spctl --assess --type open --context context:primary-signature --verbose=2 "$dmg"
echo 'Developer ID signatures, Apple notarization, stapled tickets, and Gatekeeper assessments passed.'
