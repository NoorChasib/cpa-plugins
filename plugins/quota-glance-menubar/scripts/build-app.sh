#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "$(uname -s)" != Darwin ]]; then
    echo 'Building the app requires macOS 13+ and Xcode Command Line Tools.' >&2
    exit 1
fi

version="${VERSION:-0.1.0}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo 'VERSION must have three numeric components, such as 0.1.0.' >&2
    exit 1
fi
read -r -a architectures <<< "${APP_ARCHS:-arm64 x86_64}"
for arch in "${architectures[@]}"; do
    case "$arch" in arm64|x86_64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
done

swift_bin="$(xcrun --find swift)"
executables=()
for arch in "${architectures[@]}"; do
    "$swift_bin" build -c release --arch "$arch" --scratch-path ".build/$arch" --product QuotaGlance
    bin_path="$("$swift_bin" build -c release --arch "$arch" --scratch-path ".build/$arch" --show-bin-path)"
    executables+=("$bin_path/QuotaGlance")
done

# Stage separately so a failed build never removes the last usable bundle.
mkdir -p dist
stage="$(mktemp -d "$PWD/dist/.bundle.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
app="$stage/Quota Glance.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
if [[ ${#executables[@]} -eq 1 ]]; then
    cp "${executables[0]}" "$app/Contents/MacOS/QuotaGlance"
else
    xcrun lipo -create "${executables[@]}" -output "$app/Contents/MacOS/QuotaGlance"
fi
chmod +x "$app/Contents/MacOS/QuotaGlance"
cp Resources/QuotaReadout.js "$app/Contents/Resources/QuotaReadout.js"
cp Resources/Info.plist "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$app/Contents/Info.plist"
"$swift_bin" scripts/make-icon.swift "$stage/AppIcon.iconset"
xcrun iconutil -c icns "$stage/AppIcon.iconset" -o "$app/Contents/Resources/AppIcon.icns"
codesign --force --sign - "$app"
codesign --verify --strict "$app"
rm -rf 'dist/Quota Glance.app'
mv "$app" 'dist/Quota Glance.app'
echo 'Built dist/Quota Glance.app'
