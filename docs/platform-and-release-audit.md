# Token Usage platform parity and independent releases

Audited source: `b4105cf`. **Historical findings below describe that commit.** Independent Linux amd64 release automation has since been implemented; see [the current release guide](releases.md). Additional build targets are deferred at the owner's request. Old repository deletion is left to the owner.

## Findings

1. Token Usage is deliberately restricted to Linux amd64 in its build, package, validation, and native acceptance scripts. Its storage implementation also contains operating-system assumptions that must be adapted before claiming broader support.
2. The latest unified catalog currently offers **Linux amd64 only for all five plugins**. Account Health, Auto Baseline, and Reset Priority previously published Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. Those historical artifacts are preserved in the unified repository, but the current catalog does not advertise them. Restoring current-version parity is broader than adding Token Usage builds.
3. There is no active automatic release publisher at the repository root. Root workflows run CI. Plugin-local release workflows are not discovered by GitHub Actions.
4. The current manual preview packager pins one suite tag and five versions. It is a verified preview publication tool, not a general independent-plugin release system.
5. CPA can already update one plugin independently. Its current direct-install catalog entries specify each plugin's version and exact artifact URL/hash/size, so the other entries can remain unchanged when one plugin is released.

## How Token Usage works

CPA loads a Go/C native shared library through ABI 1 / RPC schema 6. The plugin receives CPA usage events, admits them into a bounded in-memory queue, and writes batches to a local SQLite database. Its sidebar reads retained data through management endpoints. It does not request provider quotas or require Quota Cache.

Defaults include an 8,192-event queue, 256-event batches, a one-second flush interval, 720-hour retention, and a 1 GiB disk budget. The default database is `<captured CPA cwd>/plugins/data/token-usage/usage.sqlite`. These are CPA-reported observations, not guaranteed lossless provider billing records.

The database uses `github.com/mattn/go-sqlite3` with CGO, WAL, synchronous FULL, bounded write waits, and an external single-writer lock. CGO is required for the native plugin ABI as well as SQLite, so changing SQLite drivers alone would not remove the native toolchain requirement.

## Platform blockers

| Area | Current implementation | Work for parity |
| --- | --- | --- |
| Native build | Makefile rejects anything except linux/amd64 and writes `.so` | Select `.so`, `.dylib`, or `.dll` and use a supported target compiler |
| Storage ownership | Unix `Stat_t`, UID, link count, exact 0700/0600 permissions | Preserve Unix behavior; implement equivalent Windows owner/access and file-type checks |
| Single writer | `syscall.Flock` and `O_NOFOLLOW` | Native Windows locking/open behavior, with competing-process tests |
| Filesystem checks | Linux NFS/SMB filesystem magic numbers | OS-specific local-filesystem checks; Darwin filesystem flags/types differ |
| Paths | Rejects symlinks in every existing ancestor; SQLite URI uses a filesystem path directly | Test normal macOS system paths, Windows drive-letter/UNC paths, spaces, Unicode, and platform-native temporary directories |
| Packages | Linux x86-64 ELF-only checks, one claimed archive, `.so` member | Validate ELF on Linux, Mach-O on macOS, and PE on Windows, including architecture and native exports |
| Acceptance | Linux x64 Go/Node/Chrome tooling and pinned Docker image | Run storage/lifecycle/native-load tests on native runners for every claimed platform; preserve existing Linux acceptance |
| Versioning | Version duplicated across runtime, Makefile, scripts, fixture registry, workflow references, and browser package metadata | Establish one per-plugin version source and derive build/package bindings; keep mixed-version rejection tests |

### Compile probe performed

The exact `owned` and `prepare` storage helpers were copied unchanged into a temporary Go package and cross-compiled without SQLite to isolate their OS API requirements:

| Target | Storage-helper compile result |
| --- | --- |
| linux/amd64 | Pass |
| linux/arm64 | Pass |
| darwin/amd64 | Pass |
| darwin/arm64 | Pass |
| windows/amd64 | Fail: undefined `Stat_t`, `Statfs_t`, `Statfs`, `O_NOFOLLOW`, `Flock`, and lock constants |

This was not a full plugin build or runtime test. macOS compiling does not validate its filesystem policy. Linux ARM64 appears to be the smallest expansion, but still needs real native-library, SQLite, and CPA validation.

### Target matrix

For Token Usage, Account Health, Auto Baseline, and Reset Priority, target the historically published matrix:

- Linux amd64 and arm64: `.so`, with a declared/tested glibc floor.
- macOS Intel and Apple Silicon: `.dylib`, with a declared deployment target.
- Windows amd64: `.dll`, with verified native runtime dependencies.

Older Auto Baseline/Reset Priority releases used a manylinux2014/glibc 2.17 build environment. The current preview was validated on Debian bookworm/glibc 2.36. These are different compatibility claims; merely adding architecture names must not imply older-glibc compatibility. Alpine/musl and Windows ARM64 are not part of the historical matrix.

Quota Cache has a separate explicit Linux-only writer implementation (`lock_other.go` returns unsupported). Its read-only client is portable and optional. Token Usage parity does not require porting Quota Cache; standalone consumers must remain usable where the writer is unavailable.

## How releases and updates work now

| Action | Current result |
| --- | --- |
| Push a change to `main` | Starts the root CI workflows; no release is published |
| Open a pull request | Runs CI; no release is published |
| Push a generic or per-plugin version tag | No root release workflow handles it |
| Run plugin-local workflow_dispatch | Nested workflow files are inactive; the root Auto Baseline/Reset Priority dispatches are CI only |
| Run `scripts/package-quota-preview.py` | Packages five already-verified Linux amd64 libraries using a fixed preview tag/version map; rewrites both catalogs locally; does not upload anything |
| Publish a GitHub release manually | Makes assets downloadable; alone it does not advance a direct-install CPA catalog entry |
| Change one catalog entry's version/artifacts | CPA can offer that plugin's new version when it next reads the catalog, assuming the installed source identity matches |
| Select Update in CPA | CPA installs the selected plugin using its platform artifact and checksums; follow any disable/restart requirement for loaded native libraries |

There is no configured automatic deployment to Coolify. Publishing source code or a release does not change running native plugins. Current root CI is not path-filtered, so even a one-plugin change starts other plugin checks; that does not mean they are released or upgraded.

CPA v7.2.155 evidence: `ListPluginStore` compares each installed version with the catalog version; `pluginReleaseKey`/`latestPluginVersion` skip GitHub latest-release discovery for direct-install entries. `InstallPluginFromStore` uses the current platform and returns `plugin_update_requires_restart` when a loaded library cannot be overwritten. `UpdateAvailable` compares dotted numeric versions. Keep the configured source URL stable; root and preview URLs still have different CPA source identities even though their entries currently match.

Plugin-local Token Usage packaging still models a generic `v0.1.2` release and a GitHub-latest source. Those helpers are used by its compatibility tests, not by the actual current preview publication. They need to be reconciled with a per-plugin release scheme before becoming an active publisher.

## Recommended independent release design

Use separate versions and tags, for example `token-usage/v0.1.3`, `auto-baseline/v0.1.4`, and `reset-priority/v0.1.7`. One root release workflow selects exactly one plugin and its supported platform matrix from a strict manifest. Generic `v*` tags and repository-wide latest-release discovery should not select plugins in a monorepo.

For a Token Usage-only release:

1. Update Token Usage and its canonical version. Run its checks and any affected shared-component checks.
2. Push its dedicated release tag. This should be the explicit publication trigger; ordinary main pushes remain CI only.
3. Build and validate only Token Usage's supported platform artifacts from that exact tagged commit. Keep native ABI, SQLite/restart, browser, and package-integrity checks.
4. Stage a draft GitHub release, upload the exact tested bytes and license notices, download and verify every asset, then publish.
5. Update only Token Usage's entry in both catalog aliases, preserving every other plugin's version and artifact records. Publish the catalog only after its downloads are accessible.
6. CPA reads the same source URL and offers Token Usage's update. Other installed plugin versions do not change.

The example tag commands below describe the proposed interface; they **do not trigger a release today**:

```sh
git tag token-usage/v0.1.3
git push origin token-usage/v0.1.3
```

Catalog updates from simultaneous releases must be serialized or reapplied against the latest catalog, with a check that unrelated entries are preserved. Release reruns must verify existing assets rather than replace published bytes. Branch protection may require a focused catalog-update PR; the release logic should support that without changing source URLs.

## Implementation order and acceptance

1. Extract only Token Usage's filesystem ownership/open/lock checks behind platform-specific files, preserving its storage contract and schema.
2. Add Linux ARM64, then macOS, then Windows native validation. Do not mark a platform supported based on cross-compilation alone.
3. Generalize Token Usage archive validation and version bindings. Preserve exact native bytes, architecture checks, and third-party notices.
4. Add the root per-plugin release workflow and targeted catalog updater; use existing plugin-specific build commands rather than forcing a broad code deduplication.
5. Rebuild current Account Health, Auto Baseline, and Reset Priority versions for their full supported matrix before adding those platform artifacts to the current catalogs.

Completion means a Token Usage-only tag produces a verified release for every claimed target and changes only Token Usage's catalog records. Each platform must demonstrate one writer, rejected unsafe paths, persisted usage after restart, correct retained queries, and native CPA load/unload. Existing Linux behavior and all unaffected plugin versions must remain intact.

## Evidence locations

- `plugins/token-usage/abi.go`, `internal/collector/collector.go`, `internal/config/config.go`: native entry point, collection, and defaults.
- `plugins/token-usage/internal/store/store.go:46`: ownership and filesystem/locking implementation.
- `plugins/token-usage/Makefile:22`, `scripts/release.py:17`: platform and artifact restrictions.
- `plugins/token-usage/scripts/check-candidate.py`: duplicated version bindings and guard coverage.
- `.github/workflows/`: active CI only; plugin-local `.github/workflows/release.yml` files are historical references.
- `scripts/package-quota-preview.py:10`, `registry.json`, `preview/registry.json`: actual current preview packaging and all five Linux amd64 catalog entries.
- `plugins/auto-baseline/.github/workflows/release.yml`, `plugins/account-health-pushover/.github/workflows/release.yml`: historical five-platform release matrices.
- CPA exact tag `v7.2.155`, commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`: management/plugin-store update behavior.
