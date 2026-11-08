#!/usr/bin/env bash
# Source from acceptance runners. No download and no fallback to a global tool.
set -euo pipefail
BROWSER_TOOLS_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
[[ $(node --version) == v26.8.1 ]] || { printf 'Required browser-test Node version: 26.8.1\n' >&2; exit 1; }
export CHAT_ARCHIVE_AGENT_BROWSER=${CHAT_ARCHIVE_AGENT_BROWSER:-$BROWSER_TOOLS_ROOT/node_modules/agent-browser/bin/agent-browser-linux-x64}
[[ -x "$CHAT_ARCHIVE_AGENT_BROWSER" ]] || { printf 'Run make browser-tools before browser acceptance.\n' >&2; exit 1; }
printf '%s  %s\n' f8e5f9294bd0da70dda61854f12004fd61c668cd682bfb600cdf6d0df73dea69 "$CHAT_ARCHIVE_AGENT_BROWSER" | sha256sum -c -
[[ $("$CHAT_ARCHIVE_AGENT_BROWSER" --version) == 'agent-browser 0.37.1' ]] || { printf 'Required agent-browser version: 0.37.1\n' >&2; exit 1; }
export AGENT_BROWSER_EXECUTABLE_PATH="$BROWSER_TOOLS_ROOT/node_modules/.cache/chat-archive-chrome/chrome"
[[ -x "$AGENT_BROWSER_EXECUTABLE_PATH" ]] || { printf 'Run make browser-tools for the pinned test engine.\n' >&2; exit 1; }
[[ $("$AGENT_BROWSER_EXECUTABLE_PATH" --version) == 'Google Chrome for Testing 153.0.8010.36 ' ]] || { printf 'Required Chrome for Testing version: 153.0.8010.36\n' >&2; exit 1; }
