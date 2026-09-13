# Docker Compose and Coolify installation

This guide installs `auto-baseline` into a CLIProxyAPI container deployed with Docker Compose or Coolify using the official `eceasy/cli-proxy-api:latest` image.

## 1. Persist the plugin directory and make config.yaml writable

The plugin needs two things the default compose file may not provide:

1. `/CLIProxyAPI/plugins` must persist across container restarts (it holds the `.so` and, by default, `plugins/auto-baseline/state.json`).
2. `/CLIProxyAPI/config.yaml` must be **writable** from inside the container. The official compose file bind-mounts it as a single file (`./config.yaml:/CLIProxyAPI/config.yaml`); do not add `:ro`. CPA itself writes this file (management console edits, hashing of `remote-management.secret-key`), and the plugin writes it the same way (in place, no rename), so a single-file bind mount is fine.

```yaml
services:
  cli-proxy-api:
    image: eceasy/cli-proxy-api:latest
    ports:
      - "8317:8317"
    volumes:
      - ./config.yaml:/CLIProxyAPI/config.yaml     # writable
      - cpa-auth:/root/.cli-proxy-api
      - cpa-plugins:/CLIProxyAPI/plugins

volumes:
  cpa-auth:
  cpa-plugins:
```

In Coolify, add a persistent storage entry for `/CLIProxyAPI/plugins` and confirm the config file mount is not read-only.

## 2. Merge the plugin configuration

Merge the `plugins` block from [`config.example.yaml`](../config.example.yaml) into your existing `config.yaml`. Minimum:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    auto-baseline:
      enabled: true
      dry-run: true
```

Do not add `claude-header-defaults` unless you already have one; the plugin treats an absent block as the compiled default and creates the block on first promotion. If you want Codex learning to have any effect, also set:

```yaml
codex:
  disable-codex-cloaking: true
```

## 3. Choose an installation method

- [Custom Plugin Store](custom-plugin-store.md) (recommended once a release is published).
- Manual installation (below).

## Manual Linux installation

### Determine the container architecture

```bash
docker exec <container> uname -m     # x86_64 -> linux_amd64, aarch64 -> linux_arm64
```

### Install a release archive

```bash
VERSION=0.1.4          # release version without the leading v
PLATFORM=linux_amd64   # or linux_arm64
curl --fail --silent --show-error --location --remote-name \
  "https://github.com/NoorChasib/cpa-plugins/releases/download/auto-baseline/v${VERSION}/auto-baseline_${VERSION}_${PLATFORM}.zip"
curl --fail --silent --show-error --location --remote-name \
  "https://github.com/NoorChasib/cpa-plugins/releases/download/auto-baseline/v${VERSION}/checksums.txt"
grep "auto-baseline_${VERSION}_${PLATFORM}.zip" checksums.txt | sha256sum --check
unzip -o "auto-baseline_${VERSION}_${PLATFORM}.zip" auto-baseline.so
docker cp auto-baseline.so <container>:/CLIProxyAPI/plugins/auto-baseline.so
docker restart <container>
```

The library must be at the plugin directory root and named exactly `auto-baseline.so`; the basename is the plugin ID.

### Install a local build

Build on a machine whose GLIBC is not newer than the container's, or use the manylinux release workflow:

```bash
make build
docker cp auto-baseline.so <container>:/CLIProxyAPI/plugins/auto-baseline.so
docker restart <container>
```

### Verify load

```bash
docker logs <container> 2>&1 | grep -E "auto-baseline|pluginhost"
# expect:
#   pluginhost: plugin loaded plugin_id=auto-baseline ...
#   pluginhost: plugin registered plugin_id=auto-baseline plugin_name=Auto Baseline version=0.1.2 ...
#   auto-baseline started: learning enabled (dry-run=true, config=/CLIProxyAPI/config.yaml via cwd default)

curl --fail --silent --show-error -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/status | jq '.config_file, .backup, .providers[].effective_baseline'
```

`config_file.exists`, `config_file.writable` and `backup.writable` must all be `true`, and `config_file.mode_unsupported` must be `false`. If `writable` is `false`, fix the mount (section 1); learning continues but nothing can be promoted. If `mode_unsupported` is `true`, see [troubleshooting](troubleshooting.md#cpa-runs-in-home-postgres-object-store-or-git-store-mode).

## Dry-run-first validation

1. Keep `dry-run: true`.
2. Use Claude Code (or your Agent SDK host) through the proxy for a few requests from at least two sessions.
3. Check the status page: the `claude` provider should list a pending candidate with your real version, package-version and runtime-version, and after quorum a `history` entry with `dry_run: true`.
4. CPA logs `auto-baseline: dry-run would promote claude baseline 2.1.220 -> <version> (...)`.
5. Set `dry-run: false` in `config.yaml`. CPA hot-reloads, the plugin reconfigures, and the pending candidate is promoted on the next observation (or immediately if quorum is already met).
6. Confirm `config.yaml` now contains the block, `<state-dir>/config.yaml.auto-baseline.bak` holds the previous file, CPA logged `config file changed, reloading`, and the status entry no longer shows `awaiting_reload`.

## Manual update

Copy the new `auto-baseline.so` over the old one and restart the container. A same-path reload without a restart is refused by design (see troubleshooting).

## Manual uninstall

1. Remove `plugins.configs.auto-baseline` from `config.yaml` (or set `enabled: false`).
2. Delete `/CLIProxyAPI/plugins/auto-baseline.so` and, optionally, `/CLIProxyAPI/plugins/auto-baseline/` (state and backup).
3. Restart the container.

The promoted `claude-header-defaults` / `codex-header-defaults` values stay in `config.yaml`; remove or edit them by hand if you want CPA's compiled defaults back.
