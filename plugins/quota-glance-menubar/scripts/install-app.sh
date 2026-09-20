#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "$(uname -s)" != Darwin ]]; then
    echo 'Installing the app requires macOS.' >&2
    exit 1
fi
if pgrep -x QuotaGlance >/dev/null; then
    echo 'Quit Quota Glance from its menu bar menu, then run make install again.' >&2
    exit 1
fi
install_dir="${INSTALL_DIR:-$HOME/Applications}"
mkdir -p "$install_dir"
stage="$(mktemp -d "$install_dir/.quota-glance-install.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
destination="$install_dir/Quota Glance.app"
ditto 'dist/Quota Glance.app' "$stage/Quota Glance.app"
codesign --verify --strict "$stage/Quota Glance.app"
# Replacing the whole bundle prevents removed resources surviving an upgrade.
if [[ -e "$destination" ]]; then
    mv "$destination" "$stage/previous.app"
fi
if ! mv "$stage/Quota Glance.app" "$destination"; then
    if [[ -e "$stage/previous.app" ]]; then
        mv "$stage/previous.app" "$destination"
    fi
    exit 1
fi
open "$destination"
echo "Installed $install_dir/Quota Glance.app"
