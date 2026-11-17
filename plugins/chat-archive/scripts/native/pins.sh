# Pins shared by run-smoke.sh and fuse-smoke.sh (sourced, not executed).
# CPA v8.0.73 = commit 43806c8; codeload archive sha256 checked 2026-11-17.
CPA_VERSION=v8.0.73
CPA_PIN=43806c865da7d47f9252316e95e315ad9f885b84
CPA_ARCHIVE_SHA256=54196b15f8de4c9e32133b7100709738adec33191dde597f08bf48f7dc053acb
PYTHON_IMAGE=python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285

# fetch_cpa_source DIR: the pinned CPA tree in DIR (a fresh copy, safe to patch).
fetch_cpa_source() {
  local dest=$1
  mkdir -p "$dest"
  if [[ -n ${CPA_SOURCE_DIR:-} ]]; then
    [[ -d "$CPA_SOURCE_DIR/.git" ]] || { printf 'CPA_SOURCE_DIR must be a git checkout.\n' >&2; return 1; }
    [[ $(git -C "$CPA_SOURCE_DIR" rev-parse HEAD) == "$CPA_PIN" ]] || { printf 'CPA_SOURCE_DIR is not the pinned commit.\n' >&2; return 1; }
    git -C "$CPA_SOURCE_DIR" archive --format=tar "$CPA_PIN" | tar -x -C "$dest"
    return 0
  fi
  local archive="$dest.tar.gz"
  if [[ -n ${CPA_SOURCE_ARCHIVE:-} ]]; then
    [[ -f "$CPA_SOURCE_ARCHIVE" ]] || { printf 'Requested source archive is missing.\n' >&2; return 1; }
    cp -- "$CPA_SOURCE_ARCHIVE" "$archive"
  else
    curl --fail --location --silent --show-error "https://codeload.github.com/router-for-me/CLIProxyAPI/tar.gz/$CPA_PIN" -o "$archive"
  fi
  printf '%s  %s\n' "$CPA_ARCHIVE_SHA256" "$archive" | sha256sum -c -
  tar -xzf "$archive" --strip-components=1 -C "$dest"
}
