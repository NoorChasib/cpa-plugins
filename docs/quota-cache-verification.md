# Quota Cache development verification

Verified locally on 2026-09-13, Linux amd64, Go 1.27.1, at implementation commit `883fdd8f7f990bd6f1b704f66f1809551a253a7f`.

## Passed checks

- Account Health: `make ci c-shared` (format, vet, race tests, Go/native builds).
- Reset Priority: `make fmt-check vet test race build`.
- Quota Cache: `make ci`, including provider-parser regression fixtures, cache scheduling/concurrency/restart tests, client freshness/identity rejection, disabled registration, and private route registration.
- Native ABI probe: actual compiled Quota Cache library with synthetic host callbacks; registration, 100 cache-only status reads, one initial provider request, restart preserving TTL, and restart preserving a one-hour 429 cooldown. Host callback buffers were released.
- Actual CPA integration: all five native libraries loaded in the pinned official v7.2.155 image, authenticated status routes worked, the quota route rejected unauthenticated access, cache reads worked, SQLite/cache used the persistent plugins volume, and all five remained available after restart. The container used the invoking local UID/GID and a disposable empty auth directory.
- Consumer regression tests: Account Health reads the original observation timestamp without provider HTTP; Reset Priority ranks from the cache, makes no provider quota calls when the cache disappears, and rejects pre-recovery observations for recovery promotion.
- Existing consolidation workflows for all four plugins passed on GitHub at commit `ff1ea22` before cache integration.

Exact CPA image: `eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`; runtime reports v7.2.155, commit `7fac6b1`. The image uses Debian bookworm/glibc 2.36. Local Quota Cache native symbols require glibc through 2.34; this does not establish compatibility with older glibc or musl.

## Reproduce

```sh
make -C plugins/quota-cache ci
python3 plugins/quota-cache/scripts/native-probe.py plugins/quota-cache/dist/quota-cache.so
make -C plugins/account-health-pushover ci c-shared
make -C plugins/reset-priority fmt-check vet test race build
make -C plugins/auto-baseline build
make -C plugins/token-usage c-shared
docker pull eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b
python3 scripts/quota-cache-smoke.py
```

## Local tested artifact hashes

These identify local development builds, not hosted release assets. Development consumer binaries still use their existing source version labels; do not confuse them with the original published binaries of those versions.

| Library | SHA-256 |
| --- | --- |
| Quota Cache | `3f5b7aa8e1f04efab56d3eee040d4b40841b5a7bf3ed05cd285e24599d7b69d1` |
| Account Health | `3047a28311f4480bf95e0cebf5f1858619632acd488fb6fcdd6b12325b04e685` |
| Reset Priority | `24a8c3d40bb615837f10cf0d5a976d38cac154b330849521ae4f4d7dd83efeda` |
| Auto Baseline | `bcf1f190e202d5235163f855aea955896b1e6e8b39295666c76d177ec837db85` |
| Token Usage | `b6e2626608d6e14ee17d86064d4e439eeb6bd72a651d341f1df06f19dcfdec84` |

## Limits

No real provider or Pushover requests were made by these test fixtures. The actual CPA test used an empty auth roster; provider traffic, consumer decisions with accounts, and 429 behavior were verified through separate synthetic/unit/native tests. This is not an end-to-end production quota test.

The screenshot's 429 cause has not been attributed to a particular caller. The new cache controls only participating plugins. CPA v7.2.155's stock dashboard is not redirected through the cache. The snapshot initially exposes regular weekly quota observations, not all dashboard windows or billing fields.

The live CPA URL was reachable in a dedicated browser but required sign-in. Its publicly served management JavaScript was inspected without authentication: the Claude quota fetcher calls both `/api/oauth/usage` and `/api/oauth/profile` through CPA `/api-call`, in parallel; the Codex fetcher also uses `/api-call` for `/backend-api/wham/usage`. This is static evidence of request paths, not a measurement of live request frequency. The fetched management HTML SHA-256 was `f11e7f970ed474d262049b236c54f35ef59f4b9a2da5b9fe1946af14a8641553`. Actual installed plugins, effective polling settings, volume names, custom data paths, and live dashboard requests have not been audited. No production configuration, installation, or data was changed.

Migration source conflict/config deletion behavior was inspected in exact v7.2.155 source; the complete uninstall/restore/reinstall migration has not been executed against a production copy. The native suite test verifies loading/persistence/restart, not source reassociation.

As with the existing ABI bridge, a host callback that never returns can block shutdown while it drains. The cache keeps provider calls serialized rather than starting replacements behind a stalled callback.
