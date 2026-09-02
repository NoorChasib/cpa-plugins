# Custom Plugin Store installation

CLIProxyAPI can install plugins from third-party registries listed under `plugins.store-sources`. This repository publishes a `registry.json` at its root so the Management Center and Management API can install, update and uninstall `auto-baseline` like an official-store plugin.

## Prerequisites

- A published GitHub release `vX.Y.Z` carrying the five platform ZIPs and `checksums.txt` (see [release.md](release.md)).
- `/CLIProxyAPI/plugins` persisted (see [install-docker-compose.md](install-docker-compose.md)).
- CPA able to reach `raw.githubusercontent.com` and `github.com`.

## Add the registry source

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-auto-baseline/main/registry.json"
```

CPA hot-reloads the change. The built-in official registry stays available.

## Install through the Management Center

1. Open the Management Center, go to Plugins, then Plugin Store.
2. Find "Auto Baseline" (tags: Fingerprint, Baseline, Headers, Claude, Codex) and click Install. CPA downloads the release ZIP for the container's platform, verifies it against `checksums.txt`, and places the platform-native shared library into the plugin directory under a versioned, platform-specific path that the loader scans (the exact layout is CPA's; the library basename `auto-baseline` is what defines the plugin ID).
3. Enable the plugin and fill in the configuration fields (they are surfaced from the plugin's metadata). Start with `dry-run` on.
4. Restart CPA if the plugin does not appear in the loaded list (a first-time install normally loads without a restart; a same-path reinstall requires one).

## Install through the Management API

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'
BASE=http://127.0.0.1:8317

# Confirm the registry entry is visible
curl --fail --silent --show-error -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" "${BASE}/v0/management/plugin-store" | jq '.[] | select(.id=="auto-baseline")'

# Install
curl --fail --silent --show-error -X POST -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" "${BASE}/v0/management/plugin-store/auto-baseline/install"

# Enable and configure
curl --fail --silent --show-error -X PATCH -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" -H "Content-Type: application/json" \
  --data '{"enabled":true}' "${BASE}/v0/management/plugins/auto-baseline/enabled"
curl --fail --silent --show-error -X PUT -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" -H "Content-Type: application/json" \
  --data '{"enabled":true,"dry-run":true,"min-observations":3,"min-distinct-sessions":2}' \
  "${BASE}/v0/management/plugins/auto-baseline/config"
```

## Configure and validate

```bash
curl --fail --silent --show-error -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" "${BASE}/v0/management/plugins/auto-baseline/status" \
  | jq '{stopped, faulted, dry_run, config: .config_file, backup, claude: .providers[0].effective_baseline, warnings}'
```

Follow the dry-run-first steps in [install-docker-compose.md](install-docker-compose.md#dry-run-first-validation) before switching `dry-run` off.

## Update

When a new release is tagged, the store shows an update. Install it, then **restart CPA**: the resident library's Go runtime cannot be re-initialized at the same path without a process restart.

## Disable without uninstalling

```bash
curl --fail --silent --show-error -X PATCH -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" -H "Content-Type: application/json" \
  --data '{"enabled":false}' "${BASE}/v0/management/plugins/auto-baseline/enabled"
```

The plugin stops learning and promoting; state and history are retained.

## Uninstall

```bash
curl --fail --silent --show-error -X DELETE -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" "${BASE}/v0/management/plugins/auto-baseline"
```

Remove the `store-sources` entry and `plugins.configs.auto-baseline` if no longer needed, delete `/CLIProxyAPI/plugins/auto-baseline/` to drop the state file, and edit or remove the promoted `claude-header-defaults` / `codex-header-defaults` values by hand if you want CPA's compiled defaults back.
