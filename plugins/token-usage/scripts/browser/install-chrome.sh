#!/usr/bin/env bash
# Pinned test-only engine, installed beneath this private npm tools directory.
# Never use agent-browser's moving stable-channel download in release gates.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DEST="$ROOT/node_modules/.cache/token-usage-chrome"
WORK=$(mktemp -d /tmp/token-usage-chrome.XXXXXXXX)
trap 'rm -rf -- "$WORK"' EXIT
if [[ -n ${TOKEN_USAGE_CHROME_ARCHIVE:-} ]]; then
  cp -- "$TOKEN_USAGE_CHROME_ARCHIVE" "$WORK/chrome.zip"
else
  curl --fail --location --silent --show-error https://storage.googleapis.com/chrome-for-testing-public/153.0.8010.36/linux64/chrome-linux64.zip -o "$WORK/chrome.zip"
fi
printf '%s  %s\n' 167a098c4fdec156b58a9f678c90a84f9072d789f9c6e7b35496a6987b8b7ef8 "$WORK/chrome.zip" | sha256sum -c -
unzip -q "$WORK/chrome.zip" -d "$WORK"
# Only replace the ignored, test-owned engine directory, never a user profile.
mkdir -p "$DEST"
cp -a "$WORK/chrome-linux64/." "$DEST/"
[[ $("$DEST/chrome" --version) == 'Google Chrome for Testing 153.0.8010.36 ' ]]
