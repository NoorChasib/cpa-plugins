# Quota Cache

One scheduled poller for Claude, Codex, and Grok regular weekly quota observations. Account Health and Reset Priority can read its saved observations instead of each contacting the providers.

**Opt-in preview:** [download and install the first preview](../../docs/quota-cache-preview.md). It includes Quota Cache 0.1.0, Account Health 0.4.1, and Reset Priority 0.1.5 for Linux amd64. All three updated binaries are needed for shared polling. The stable catalog continues to serve the prior consumer versions.

## Install and start

Add `https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/preview/registry.json` as your preview store source, then follow the [preview installation guide](../../docs/quota-cache-preview.md). Read the migration guide before switching already-installed plugins between sources.

### Build from source

Target: Linux amd64, native ABI 1 / schema 4+, including CPA v7.2.155. The writer uses Linux file locking. You need Go 1.26+ and a C toolchain; verification uses Go 1.27.1.

From the repository root:

```sh
make -C plugins/quota-cache ci
make -C plugins/account-health-pushover c-shared
make -C plugins/reset-priority build
```

Outputs are `plugins/quota-cache/dist/quota-cache.so`, `plugins/account-health-pushover/dist/account-health-pushover.so`, and `plugins/reset-priority/reset-priority.so`. These are local builds; the preview release provides checksum-verified artifacts of the tested binaries. Follow the [migration guide](../../docs/migration.md) before replacing installed libraries, especially versioned store-managed files.

Merge the following options into your existing plugin configuration; keep all other options and data paths:

```yaml
plugins:
  enabled: true
  configs:
    quota-cache:
      enabled: true
      cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
      poll-interval: 15m
      request-spacing: 10s
    account-health-pushover:
      quota-cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
    reset-priority:
      quota-cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
```

Install/start the cache first and wait for observations, then enable cache mode in the updated consumers. Preserve Account Health's existing `quota-alerts` preference; the cache does not enable notifications itself. Keep Reset Priority's existing dry-run setting.

In the standard Coolify/Docker layout the cache is inside the existing `/CLIProxyAPI/plugins` volume. No new network port or management key is needed between plugins. The cache directory must be private (`0700`) and owned by the CPA process user; snapshots are written with `0600`. Custom paths must be identical across the three plugins and remain on a local filesystem supporting atomic rename and file locking.

The private management route `GET /v0/management/plugins/quota-cache/status` reports the snapshot and retry schedule through CPA's existing authentication. There is no public quota-data route or sidebar page in this first version.

## Polling behavior

- At most one provider request runs at a time in the cache plugin, spaced at least 10 seconds apart by default.
- Each credential is polled at most once per 15-minute interval by default. The initial accounts are staggered by request spacing.
- On 429, all accounts for that provider pause. Respect `Retry-After`, retain the previous observation, and back off repeated failures up to six hours (a longer server `Retry-After` is still honored).
- Schedules and cooldowns persist before calls; restarts retain them. A second writer for the same cache path is rejected.
- Consumers perform file reads only in cache mode. Missing, failed, stale (>30 minutes), future-dated, or expired-window data does not cause direct provider fallback.
- Reset Priority retains its existing deadline/failure handling and requires an observation after recovery before promoting a recovering account. A refresh failure does not turn expired data into a new reset window.
- No tokens, auth documents, upstream bodies, or arbitrary upstream error messages are saved in the snapshot.

A cache outage can temporarily delay quota notifications and fresh reset confirmation. It deliberately does not trigger a burst of fallback requests. Use the default 15-minute interval with the consumers' 30-minute freshness limit.

## Scope and dashboard behavior

The initial snapshot contains regular weekly used percentage, reset time, observation time, and scheduling/error metadata. It does not yet normalize every five-hour, model-specific, subscription, or billing field shown by the CPA dashboard.

On **CPA v7.2.155**, the stock quota dashboard still makes independent requests. The inspected live management bundle sends both Claude usage and profile requests through CPA’s `/api-call` route for each quota refresh. The newer quota-provider interface found in a later local CPA checkout is absent from the v7.2.155 tag. Dashboard integration therefore needs a separately verified host/console change. This plugin reduces the participating plugins' requests; it cannot guarantee that all dashboard 429s disappear.

A future push-notification plugin can read the same version-1 snapshot or authenticated status route. Reading does not request a refresh. For the same Go monorepo, `client.ReadFresh` provides the read-only interface. Snapshot consumers must respect timestamps/errors rather than treating saved values as current.

## Verification

```sh
make -C plugins/quota-cache ci
python3 plugins/quota-cache/scripts/native-probe.py plugins/quota-cache/dist/quota-cache.so
# After building all five plugins:
python3 scripts/quota-cache-smoke.py
```

See [verification evidence](../../docs/quota-cache-verification.md). No live provider credentials or production changes are required by these tests.
