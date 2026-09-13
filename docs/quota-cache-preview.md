# Shared quota cache — first preview

Installable Linux amd64 preview for CPA v7.2.155. This is an opt-in prerelease; the stable catalog continues to serve the existing releases.

Included:

- Quota Cache **0.1.0**: one scheduled weekly-quota poller, shared snapshot, serialized requests, persistent provider-wide 429 cooldowns, and a private status route.
- Account Health Pushover **0.4.1**: optional cache-only weekly quota reads.
- Reset Priority **0.1.5**: optional cache-only weekly reset reads with freshness/recovery checks.

Auto Baseline and Token Usage remain at their existing stable release locations in the preview catalog. They do not poll provider quota endpoints.

## Install through CPA

Preview store source:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/preview/registry.json
```

For an immutable preview source, use the release's `registry.json` asset instead:

```text
https://github.com/NoorChasib/cpa-plugins/releases/download/quota-cache-preview-1/registry.json
```

Pick one source URL and keep it consistent; CPA treats distinct URLs as distinct sources. Use the [migration guide](migration.md) for already-installed consumers: v7.2.155 requires uninstalling to change source, and uninstall deletes the plugin's settings. Preserve and restore those settings before reinstalling. Keep the same volumes, auth files, state paths, and SQLite directory.

Install Quota Cache, wait for its first observations, then install the two updated consumers from this preview source and merge [the shared cache settings](../plugins/quota-cache/config.example.yaml). In both consumers, `quota-cache-path` must point to the exact same file that Quota Cache writes. Merely installing the cache does not turn off the consumers' standalone polling.

The default poll interval is 15 minutes per credential with 10-second spacing between provider requests. Missing/stale/failed cache reads never cause provider fallback in cache mode. Preserve Account Health's `quota-alerts` preference and Reset Priority's existing dry-run/routing settings.

## Verification and limitations

The libraries were tested together in the exact official CPA v7.2.155 image, including native registration, authenticated status, default-volume SQLite/cache paths, and restart. Unit/race/native callback tests cover shared reads, 429 cooldown persistence, and recovery freshness. Archives contain those exact tested bytes; SHA-256 hashes are in `checksums.txt` and the direct-install catalog. See [verification details](quota-cache-verification.md).

This preview targets Linux amd64/glibc, tested on Debian bookworm/glibc 2.36. It is not a musl/Alpine, older-glibc, or other-architecture release. No live provider credentials were used during validation, and the production installation was not changed.

The snapshot initially contains regular weekly quota observations. CPA v7.2.155's stock dashboard still fetches its own quota/profile data through `/api-call`; those requests are not redirected through this cache. The original 429 cause has not been attributed, and dashboard 429s may still occur.

## Roll back

Keep the stopped backup of config, auth and plugin volumes, and prior libraries before installing. Follow [rollback guidance](migration.md#rollback) to restore a coherent configuration/library set. Removing `quota-cache-path` re-enables standalone quota polling, so do not use that as an automatic fallback during rate limiting.
