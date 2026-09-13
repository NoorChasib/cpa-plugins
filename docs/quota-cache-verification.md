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

## Installable preview verification

The final preview uses Quota Cache 0.1.0, Account Health 0.4.1, and Reset Priority 0.1.5. Account Health and Reset Priority unit/race/vet/native gates passed after version/source-link changes. The pinned CPA suite smoke test passed again with these exact versions at source commit `a73a5cd57a0197027fb8c6b2debcd77ba5f264e1`. Quota Cache native callback/429 tests passed; its runtime code is unchanged from the earlier cache verification.

| Preview library | Version reported by CPA | Tested native SHA-256 |
| --- | --- | --- |
| quota-cache | 0.1.0 | `02619e0e7f6886713e31da019e9b53f0e9e29a76eb56fe1580c527e5645c0946` |
| account-health-pushover | 0.4.1 | `c6df62ecaa74f1ac7729916aecbbf75883f17c1033778ad3a6a2413dbe03b5aa` |
| reset-priority | 0.1.5 | `e6ef63700aa80ff11e96b96262ab411ad8bf61bcd38a7c020b68450e4dd2a667` |

`scripts/package-quota-preview.py` checks native hashes and CPA-reported versions against the smoke evidence, packages the exact bytes, verifies ZIP contents, and generates direct-install URLs with archive hashes/sizes. The preview release includes `verification.json`, the catalog, and checksums. Stable catalog artifacts are unchanged.

All five GitHub workflows passed at `ee02efe`, including full Token Usage native/browser/store acceptance and the new quota-cache integration workflow. Later preview version/source-link changes were rechecked locally as described above.


## Preview 2: unified downloads and optional toggle

All five runtime registrations now link to `NoorChasib/cpa-plugins`. Account Health 0.4.2 and Reset Priority 0.1.6 add the `use-quota-cache` boolean; Auto Baseline 0.1.3 and Token Usage 0.1.2 update their repository metadata. Quota Cache remains 0.1.0.

- Consumer config tests cover default standalone operation, opt-in using the default path, custom paths, explicit opt-out overriding a path, and legacy path-only compatibility.
- Existing cache consumer tests still require no direct-provider fallback when a cache disappears or its observations are invalid.
- All four changed plugins passed their local unit, race, formatting, vet, and native build checks.
- The official-image suite verifies all five repository links and runtime registration/status/restart. It then removes the cache library and snapshot and verifies that all four other plugins remain registered/effective with both cache mode enabled (waiting) and explicitly disabled (standalone). The roster is synthetic and empty; account-level fallback behavior is covered by consumer tests.
- Root and preview catalogs contain identical entries with direct, SHA-256-pinned downloads from this repository only. Historical releases were copied byte-for-byte under namespaced tags, with every GitHub asset digest checked against the backup.

Exact archive and library hashes are recorded in preview 2's `checksums.txt` and `verification.json`. Earlier sections above document preview 1, not the current binary versions.
