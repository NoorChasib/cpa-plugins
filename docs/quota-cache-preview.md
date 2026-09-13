# Unified plugins — preview 2

Installable Linux amd64 preview for CPA v7.2.155. This is a prerelease containing all five plugins hosted in `NoorChasib/cpa-plugins`. Quota Cache is optional.

Included:

- Quota Cache **0.1.0**: one scheduled weekly-quota poller, shared snapshot, serialized requests, persistent provider-wide 429 cooldowns, and a private status route.
- Account Health Pushover **0.4.2**: optional cache-only weekly quota reads.
- Reset Priority **0.1.6**: optional cache-only weekly reset reads with freshness/recovery checks.

- Auto Baseline **0.1.3** and Token Usage **0.1.2**: native repository links now point to the unified repository. Neither uses Quota Cache or polls provider quota endpoints.

## Install through CPA

Canonical store source:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

`main/preview/registry.json` contains the same catalog for existing users. Keep your already-added URL; CPA treats distinct URLs as separate sources. Both catalogs download exclusively from this repository.

For an immutable preview source, use the release's `registry.json` asset instead:

```text
https://github.com/NoorChasib/cpa-plugins/releases/download/quota-cache-preview-2/registry.json
```

Pick one source URL and keep it consistent; CPA treats distinct URLs as distinct sources. Use the [migration guide](migration.md) for already-installed consumers: v7.2.155 requires uninstalling to change source, and uninstall deletes the plugin's settings. Preserve and restore those settings before reinstalling. Keep the same volumes, auth files, state paths, and SQLite directory.

Install whichever plugins you want. All consumers work without Quota Cache by default. To share quota polling, install/start Quota Cache, wait for its observations, then switch **use-quota-cache** on in Account Health and Reset Priority's config panels:

```yaml
plugins:
  enabled: true
  configs:
    quota-cache:
      enabled: true
      poll-interval: 15m
      request-spacing: 10s
    account-health-pushover:
      use-quota-cache: true
      # Keep quota-alerts: true if you want quota notifications.
    reset-priority:
      use-quota-cache: true
```

The default shared path is `plugins/data/quota-cache/snapshot.json` relative to CPA's working directory. A custom `quota-cache-path` in each consumer must match the writer's `cache-path`. Toggle off (`use-quota-cache: false`) restores standalone polling, even if a custom path remains. A legacy path-only configuration remains opted in when the new toggle is omitted.

With the toggle on, a missing, stopped, stale, or failing cache does **not** trigger direct provider requests. Consumers wait for usable observations. Installing Quota Cache alone never changes another plugin's polling mode.

The default poll interval is 15 minutes per credential with 10-second spacing between provider requests. Missing/stale/failed cache reads never cause provider fallback in cache mode. Preserve Account Health's `quota-alerts` preference and Reset Priority's existing dry-run/routing settings.

## Verification and limitations

The libraries were tested together in the exact official CPA v7.2.155 image, including native registration, authenticated status, default-volume SQLite/cache paths, and restart. Unit/race/native callback tests cover shared reads, 429 cooldown persistence, and recovery freshness. Archives contain those exact tested bytes; SHA-256 hashes are in `checksums.txt` and the direct-install catalog. See [verification details](quota-cache-verification.md).

This preview targets Linux amd64/glibc, tested on Debian bookworm/glibc 2.36. It is not a musl/Alpine, older-glibc, or other-architecture release. No live provider credentials were used during validation, and the production installation was not changed.

The snapshot initially contains regular weekly quota observations. CPA v7.2.155's stock dashboard still fetches its own quota/profile data through `/api-call`; those requests are not redirected through this cache. The original 429 cause has not been attributed, and dashboard 429s may still occur.

## Roll back

Keep the stopped backup of config, auth and plugin volumes, and prior libraries before installing. Follow [rollback guidance](migration.md#rollback) to restore a coherent configuration/library set. Removing `quota-cache-path` re-enables standalone quota polling, so do not use that as an automatic fallback during rate limiting.
