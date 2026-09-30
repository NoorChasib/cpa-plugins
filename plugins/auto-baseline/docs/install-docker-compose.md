# Docker Compose and Coolify installation

This guide installs `auto-baseline` into a CLIProxyAPI container deployed with Docker Compose or Coolify using the official `eceasy/cli-proxy-api` image. It was verified on `eceasy/cli-proxy-api:v8.0.4`; pinning a tag or digest rather than `latest` keeps an image update from changing CPA under the plugin unannounced.

## 1. Persist the plugin directory and make config.yaml writable

The plugin needs two things the default compose file may not provide:

1. `/CLIProxyAPI/plugins` must persist across container restarts (it holds the `.so` and, by default, `plugins/auto-baseline/state.json`).
2. `/CLIProxyAPI/config.yaml` must be **writable** from inside the container. The official compose file bind-mounts it as a single file (`./config.yaml:/CLIProxyAPI/config.yaml`); do not add `:ro`. CPA itself writes this file (management console edits, hashing of `management.secret-key`), and the plugin writes it the same way (in place, no rename), so a single-file bind mount is fine.

```yaml
services:
  cli-proxy-api:
    image: eceasy/cli-proxy-api:v8.0.4
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

Do not add a Claude header-defaults block unless you already have one; the plugin treats an absent block as the compiled default and creates it on first promotion, as `oauth.providers.claude.header-defaults` in a `config-version: 8` file and as `claude-header-defaults` otherwise. If you want Codex learning to have any effect, also set the cloaking flag, in the layout your file uses:

```yaml
# v8 layout (config-version: 8)
oauth:
  providers:
    codex:
      disable-codex-cloaking: true

# legacy layout
codex:
  disable-codex-cloaking: true
```

Any save from CPA's Management Center panel, including a Plugin Store install, rewrites the whole file in the v8 layout; auto-baseline 0.1.5 reads and writes both (see [Config layouts](../REFERENCE.md#config-layouts-cpa-v8)).

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
VERSION=0.1.5          # release version without the leading v
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
#   pluginhost: plugin registered plugin_id=auto-baseline plugin_name=Auto Baseline version=0.1.5 ...
#   auto-baseline started: learning enabled (dry-run=true, config=/CLIProxyAPI/config.yaml via cwd default)

curl --fail --silent --show-error -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/auto-baseline/status | jq '.config_file, .backup, .providers[].effective_baseline'
```

`config_file.exists`, `config_file.writable` and `backup.writable` must all be `true`, and `config_file.mode_unsupported` must be `false`. `config_file.layout` and each provider's `effective_baseline.sources` / `write_target` show which keys the plugin reads and writes. If `writable` is `false`, fix the mount (section 1); learning continues but nothing can be promoted. If `mode_unsupported` is `true`, see [troubleshooting](troubleshooting.md#cpa-runs-in-home-postgres-object-store-or-git-store-mode).

## Dry-run-first validation

1. Keep `dry-run: true`.
2. Use Claude Code (or your Agent SDK host) through the proxy for a few requests from at least two sessions.
3. Check the status page: the `claude` provider should list a pending candidate with your real version, package-version and runtime-version, and after quorum a `history` entry with `dry_run: true`.
4. CPA logs `auto-baseline: dry-run would promote claude baseline <current> -> <version> (...) at <block>`, where `<block>` is the key path a live write would edit.
5. Set `dry-run: false` in `config.yaml`. CPA hot-reloads, the plugin reconfigures, and the pending candidate is promoted on the next observation (or immediately if quorum is already met).
6. Confirm `config.yaml` now carries the promoted values in that block, `<state-dir>/config.yaml.auto-baseline.bak` holds the previous file, CPA logged `config file changed, reloading`, and the status entry no longer shows `awaiting_reload`.

## Manual update

Copy the new `auto-baseline.so` over the old one and restart the container. A same-path reload without a restart is refused by design (see troubleshooting).

## Upgrading from 0.1.4 to 0.1.5 (CPA v8)

0.1.4 only understands the legacy keys. On a file that a panel save has migrated to the v8 layout it assumes the compiled default, writes a root `claude-header-defaults` every `promotion-cooldown`, and CPA deletes it again on every reload. Never let 0.1.4 run with live writes on a v8-layout file. Upgrade like this:

1. **Stop writes first, without a panel save.** Set `dry-run: true` with the plugin's own switch (**Switch to dry-run** on its sidebar page, or `POST /v0/management/plugins/auto-baseline/dry-run` with `{"enabled": true}`), or by editing `plugins.configs.auto-baseline.dry-run` in `config.yaml` by hand. Both edit the file in place and keep its layout; a panel save would migrate it while 0.1.4 can still write. Wait until status shows `dry_run: true` and `dry_run_awaiting_reload: false`.
2. **Update the plugin** to 0.1.5 (Plugin Store **Update**, which may migrate the file to the v8 layout; that is safe now), or replace the library manually as above, and restart CPA if it asks.
3. **Check what 0.1.5 reads** in the status route or sidebar page:
   - `version` is `0.1.5`, `assumed_cpa_version` names v8.0.4, and `dry_run` is still `true`;
   - `config_file.layout` is `v8` if the file carries `config-version: 8`;
   - for each provider, `effective_baseline.version` and `user_agent` are the baseline you expect CPA to be using, `sources` are `v8` (or `legacy` for keys still at their legacy names), and `write_target` is the block you expect;
   - `disable_codex_cloaking` shows the value and source you expect;
   - no provider is `paused` and `last_error` is empty.

   Let a few requests arrive. A dry-run history entry should say `target: oauth.providers.claude.header-defaults` (or the legacy block, if that is where your keys are), and no entry should propose a promotion from the old compiled default.
4. **Resume writes:** set `dry-run: false` the same way as step 1 (the panel is also fine now). The next promotion is written once, into the block `write_target` named, and status shows it confirmed after CPA's reload.

If Claude API-key credentials relied on the header defaults, note that after the migration they apply to OAuth credentials only (see [Config layouts](../REFERENCE.md#config-layouts-cpa-v8)).

## Manual uninstall

1. Remove `plugins.configs.auto-baseline` from `config.yaml` (or set `enabled: false`).
2. Delete `/CLIProxyAPI/plugins/auto-baseline.so` and, optionally, `/CLIProxyAPI/plugins/auto-baseline/` (state and backup).
3. Restart the container.

The promoted header-defaults values stay in `config.yaml` (under `oauth.providers.*.header-defaults` or the legacy `claude-header-defaults` / `codex-header-defaults`); remove or edit them by hand if you want CPA's compiled defaults back.
