# Pins shared by run-smoke.sh and fuse-smoke.sh (sourced, not executed).
# CPA v8.0.73 = commit 43806c8; codeload archive sha256 checked 2026-11-17.
CPA_VERSION=v8.0.73
CPA_PIN=43806c865da7d47f9252316e95e315ad9f885b84
CPA_ARCHIVE_SHA256=54196b15f8de4c9e32133b7100709738adec33191dde597f08bf48f7dc053acb
# CPA v8.0.76 = commit 7df5607; codeload archive sha256 checked 2026-11-18. Only the acceptance
# phase "failover" runs on it: v8.0.76 runs an in-place Responses WebSocket failover under the
# failed attempt's RequestID, which 0.1.5 keys apart.
CPA_FAILOVER_VERSION=v8.0.76
CPA_FAILOVER_PIN=7df56074b252384d878ec4fca840552c6fb5236e
CPA_FAILOVER_ARCHIVE_SHA256=4dc68704c4fc8c572bd6513c0a18338a8feb853510d68c3c74153453d8ef9361
PYTHON_IMAGE=python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285

# fetch_cpa_source DIR [PIN SHA256 SOURCE_DIR SOURCE_ARCHIVE]: the pinned CPA tree in DIR (a fresh
# copy, safe to patch). Without the optional arguments: CPA_PIN, with CPA_SOURCE_DIR (a git
# checkout) or CPA_SOURCE_ARCHIVE (a downloaded codeload archive) reused when set.
fetch_cpa_source() {
  local dest=$1 pin=${2:-$CPA_PIN} sum=${3:-$CPA_ARCHIVE_SHA256}
  local source_dir=${4-${CPA_SOURCE_DIR:-}} source_archive=${5-${CPA_SOURCE_ARCHIVE:-}}
  mkdir -p "$dest"
  if [[ -n $source_dir ]]; then
    [[ -d "$source_dir/.git" ]] || { printf 'The CPA source directory must be a git checkout.\n' >&2; return 1; }
    [[ $(git -C "$source_dir" rev-parse HEAD) == "$pin" ]] || { printf 'The CPA source directory is not the pinned commit.\n' >&2; return 1; }
    git -C "$source_dir" archive --format=tar "$pin" | tar -x -C "$dest"
    return 0
  fi
  local archive="$dest.tar.gz"
  if [[ -n $source_archive ]]; then
    [[ -f "$source_archive" ]] || { printf 'Requested source archive is missing.\n' >&2; return 1; }
    cp -- "$source_archive" "$archive"
  else
    curl --fail --location --silent --show-error "https://codeload.github.com/router-for-me/CLIProxyAPI/tar.gz/$pin" -o "$archive"
  fi
  printf '%s  %s\n' "$sum" "$archive" | sha256sum -c -
  tar -xzf "$archive" --strip-components=1 -C "$dest"
}

# fetch_cpa_failover_source DIR: the v8.0.76 tree, with CPA_FAILOVER_SOURCE_DIR or
# CPA_FAILOVER_SOURCE_ARCHIVE reused when set.
fetch_cpa_failover_source() {
  fetch_cpa_source "$1" "$CPA_FAILOVER_PIN" "$CPA_FAILOVER_ARCHIVE_SHA256" "${CPA_FAILOVER_SOURCE_DIR:-}" "${CPA_FAILOVER_SOURCE_ARCHIVE:-}"
}
