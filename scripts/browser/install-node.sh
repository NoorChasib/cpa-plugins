#!/usr/bin/env bash
# CI-only local tool installation, never a global install or runtime dependency.
set -euo pipefail
[[ $# == 1 && -n $1 ]] || { printf 'Usage: install-node.sh NEW_DESTINATION\n' >&2; exit 1; }
DEST=$1
[[ ! -e "$DEST" ]] || { printf 'Destination already exists; refusing overwrite.\n' >&2; exit 1; }
ARCHIVE=$(mktemp /tmp/token-usage-node.XXXXXXXX.tar.xz)
trap 'rm -f -- "$ARCHIVE"' EXIT
curl --fail --location --silent --show-error https://nodejs.org/dist/v26.8.1/node-v26.8.1-linux-x64.tar.xz -o "$ARCHIVE"
printf '%s  %s\n' 3e301118d7df53d563b7e96c1617545f26e2f76f9724be668d6cab65c15dda5d "$ARCHIVE" | sha256sum -c -
mkdir -p -- "$DEST"
tar -xJf "$ARCHIVE" --strip-components=1 -C "$DEST"
[[ $("$DEST/bin/node" --version) == v26.8.1 ]]
