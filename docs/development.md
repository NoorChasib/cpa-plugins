# Development, native acceptance, and releases

## Commands and boundaries

Run from the repository root on Linux amd64. Required tools are Go 1.26.0+, GCC/CGO, Make, Python 3.11+ (stdlib packaging/SQLite client), curl, tar, sha256sum and Docker. Native execution uses the pinned official Python image, not the host's Python packages. Module downloads use `go.sum`; no provider credentials or billable calls are needed.

| Command | Work performed |
|---|---|
| `make fmt-check` | Check Go source formatting without editing it. |
| `make vet` | Vet production and nativefixture Go builds. |
| `make test` | Go unit tests and Python release/CI contract tests under `scripts/tests`. |
| `make test-race` | Production and nativefixture race suites, uncached. |
| `make build` | Ordinary Go build check; this executable is not the loadable plugin. |
| `make c-shared` | Production `dist/token-usage.so`, CGO, Linux amd64, version 0.1.0. The generated header is not packaged. |
| `make ci` | Formatting, vet, unit/Python/race tests, ordinary and c-shared builds. Does **not** replace native smoke. |
| `make smoke` | Mandatory direct ABI, Phase A nativefixture and full production CPA HTTP/SQLite/restart acceptance. Never silently skips. |
| `make native-spike` / `bash scripts/native/run-spike.sh` | Original spike entry point: lifecycle ABI probe plus all 30 upstream nativefixture expectations. |
| `make package-current VERSION=0.1.0` | Fresh production build and deterministic ZIP structure for the only claimed platform. |
| `make package-tested VERSION=0.1.0` | Package the existing `DIST/token-usage.so` without rebuilding. Release workflow first copies the accepted production library from native smoke. |
| `make checksums` | Generate `DIST/registry.json` with the exact public archive URL/SHA/size, then `DIST/checksums.txt` covering the ZIP and release registry. Never rewrite the committed registry. |
| `make verify-release` | Require exactly one claimed archive; validate both registry contracts, checksums, ZIP members/safe modes, original license notices, ELF c-shared identity, and current binary equality. |

Only 0.1.0 is accepted by the current packaging contract. `DIST` can point to a separate staging directory. Do not mix stale version/platform archives in that directory. After any rebuild, rerun package/checksums/verification together. The committed root `registry.json` is the canonical GitHub-release source, independent of compiler-dependent hashes; `DIST/registry.json` binds the actual archive bytes to the intended versioned public URL. Neither command performs network requests or publication. Stable ZIP timestamps/order/modes make packaging reproducible for identical inputs; compiler/libc differences can change the shared object. CI/releases explicitly use Go 1.27.1 on Ubuntu 24.04, but the runner image and GCC are not bit-for-bit pinned; cross-run binary reproducibility is not promised.

`nativefixture` is a regression-only build tag. It exposes up to 64 sanitized observations in authenticated status. The ordinary production library never exposes those observations. `package-current` uses an ordinary build, and the packager rejects libraries whose embedded Go build tags include `nativefixture`. ELF checks and embedded build metadata do not replace native loading or authenticate an untrusted binary.

## Reproducible native fixture runner

The runner pins:

- CPA v7.2.155 commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`.
- CPA source archive SHA-256 `3f829041f62175a546750520d1c160cba1a7618d30f17618731e250c048d77e5`.
- Official Python runtime `python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285`, platform Linux amd64. This image does not supply CPA: CPA is compiled from the verified source.

By default the runner downloads the immutable source archive; it does not search historical `/tmp` paths. To reuse a retained copy explicitly:

```sh
CPA_SOURCE_ARCHIVE=/absolute/path/to/verified/cpa.tar.gz make smoke
```

Every archive is checked against the same pinned hash, including a supplied local copy. An explicit missing archive fails. Extraction and runtime files use a fresh `/tmp/token-usage-smoke.*` directory, printed at startup and retained for inspection. Go's normal build/module cache is reused. Nothing is written into a deployed CPA tree or the health-plugin repository.

During execution Docker uses `--network none`, a read-only root, dropped capabilities except the narrowly needed `CHOWN`, `no-new-privileges`, and PID/memory limits. Only the fresh synthetic runtime directory is writable; source scripts and native/CPA binaries are read-only mounts. Root inside that disposable container avoids the host user's exhausted inotify quota without modifying sysctls or unrelated processes. The exit trap returns ownership of that new directory to the host UID. The daemon socket is not mounted inside the container. Startup/exit failures propagate; no missing-Docker success path exists.

All requests are loopback: synthetic HTTP client → actual CPA routing/executor → mock upstream → usage manager/native ABI → plugin. The production suite checks committed SQLite rows and all seven raw API counters rather than the nativefixture buffer. It reproduces all Phase A cases plus equal-looking repeat observations, the same execution model under two providers, and a large-integer Claude nonstream case. It then stops CPA cleanly, checks SQLite integrity/run markers/privacy, restarts against the same database, verifies unchanged historical queries and exactly one additional observation. Each statistics setting runs independently with a fresh database. Expected omissions are recorded as omissions, not silently changed to make accounting look complete.

Authentication assertions cover every private route with missing/invalid keys and both public resource spellings. Valid management probes between groups reset CPA's failed-auth counter; otherwise CPA's IP-ban response would conceal what later route checks actually tested. Sensitive canaries cover client/upstream/management keys, request/failure bodies and headers; the direct ABI probe additionally covers unused session/source/auth metadata and failed nonzero tokens without an account. SQLite contents/bytes and plugin outputs are checked. CPA's own upstream error logging is outside the plugin's redaction boundary.

Runtime review regressions additionally exercise independent simultaneous disk/write/diagnostics/maintenance/identity faults, repeated successful saves during permanent failures, throttled non-event writer probes, concurrent identity exhaustion, 1800 expired rows in bounded store chunks, three 900-event expiry bursts (plus prior retained fresh events) with prompt worker continuation, readable private summary/models under actual disk-budget and injected SQLite maintenance faults, and late-callback accounting during/after quiesce and concurrent shutdown. Probe tests verify that synthetic recovery inserts never remain in queryable history or advance committed counters, coverage or last-persistence metadata. These tests do not promise a cleanup throughput, immediate file shrink, or persistence of callbacks arriving after the final diagnostic sample.

The successful full-CPA rerun **after the frozen runtime review fixes** retained evidence at `/tmp/token-usage-smoke.Q95Dylvc/runtime` on the development machine. It passed both direct native-library probes, all 30 Phase A executions and 38 production executions, including restart/history/auth/privacy/large-integer assertions. The updated direct probe also requires stopped status 200, summary/models 503 after close, exact late normal/oversized observation and stopped-drop increments without admissions/commits, and same-config reopen resuming durable counters. Build/package checks and the actual pinned CPA installer rehearsal also passed; the packaged library matches the production smoke bytes. Logs are `/tmp/token-usage-postreview-build.log`, `/tmp/token-usage-postreview-smoke.log`, and `/tmp/token-usage-postreview-validator.log`. Those ephemeral paths are not release assets and may later be removed. A clean rerun should be treated as the current evidence after any runtime change. Full upstream expectations/untested executor families remain in [upstream compatibility](upstream-compatibility.md); runtime failure/concurrency tests are described in [runtime contract](runtime-contract.md).

## Public registry contracts and actual CPA validator

The pinned CPA store distinguishes registry schema 2 from native RPC schema 6. It supports these two contracts, both used here:

- **Root `registry.json`: GitHub-release mode.** `install.type` is `github-release`, `repository` is the fixed project GitHub URL, and `version: "0.1.0"` is a display fallback. CPA resolves `/releases/latest`, derives the real version from its tag, and selects exactly `token-usage_0.1.0_linux_amd64.zip` plus an asset literally named `checksums.txt`. Schema 2 does **not** expose a fixed-tag or asset-pattern field for this mode. The stable source follows future latest releases and cannot promise a version lock or enumerate platform support until release assets are resolved. Only Linux amd64 has an asset; other platforms fail installation.
- **Release asset `registry.json`: direct mode.** Schema 2 requires a version without leading `v`, artifact `goos`/`goarch`, HTTP(S) URL, and 64-hex archive SHA-256. This release strictly uses HTTPS, so no insecure-transport override is needed. We include the exact byte size and only Linux amd64. Its URL is fixed under `https://github.com/NoorChasib/cpa-plugin-token-usage/releases/download/v0.1.0/`. This source pins 0.1.0 independently of the latest release. No file/loopback URLs or insecure transport/auth rules are needed.

The archive contains exactly one root dynamic library, `token-usage.so`, plus project/dependency licenses. CPA permits non-library regular files, but rejects nested target libraries, multiple dynamic libraries, symlinks, and escaping archive names. Both installation modes verify the whole archive checksum, which covers the native library and license bytes. `checksums.txt` also includes the generated direct registry digest for operator verification. Download the ZIP, registry, and checksum manifest together before running `sha256sum -c checksums.txt`.

After packaging, exercise the **actual pinned CPA SDK**, not just a Python mirror:

```sh
# Use the verified source directory printed by your successful make smoke run.
python3 scripts/verify-cpa-package.py \
  --cpa-source /tmp/token-usage-smoke.REPLACE/source
```

This creates a temporary Go harness outside both repositories. Its custom HTTP client maps only the exact intended public registry/archive/checksum URLs to local files and supplies a synthetic release-metadata response; there is **no network transport or live store installation**. It invokes CPA's actual registry parsers and both installers, checks byte equality and idempotent reinstall, rejects unsupported arm64 and corrupt checksums in both modes, and checks exact-tag installation and version mismatch rejection for GitHub releases. Local verification does not establish that any URL is already published. Dependency compilation can populate the existing Go cache. Do not substitute an unverified source tree and still describe the result as pinned validation.

Authoritative pinned sources:

- [Registry schema and direct validation](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginstore/registry.go).
- [ZIP contents, checksum installation and atomic target writes](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginstore/install.go).
- [Latest/tag lookup and GitHub-release archive/checksum names](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginstore/github.go).
- [Exported SDK installer API](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginstore/pluginstore.go).
- [Official `plugins.store-sources` configuration](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/config.example.yaml#L89-L101).

## Read-only CI and guarded publication

`.github/workflows/ci.yml` runs on pull requests and pushes to main with read-only repository permissions, checkout credential persistence disabled, no dispatch, no secrets, no uploads, and no release writes. It runs full `make ci`, mandatory `make smoke`, and local package verification. Both workflows use Ubuntu 24.04 amd64 and select/check Go 1.27.1. Checkout v4.2.2 and setup-go v5.5.0 are pinned to full commits, reverified against their official tag refs on 2026-09-09. Preparing workflows or running local tests does not claim a successful hosted run; inspect the actual GitHub Actions run and release assets.

`.github/workflows/release.yml` runs **only on an explicit `v0.1.0` tag push** in `NoorChasib/cpa-plugin-token-usage`. There is no PR or workflow-dispatch publish trigger. A single `publish` job is the only job with `contents: write`; only explicit `gh` steps receive its token as `GH_TOKEN`. Checkout never persists credentials. The job requires the expected repository/event/ref/version, tag/checked-out commit equality, and ancestry on `origin/main`. It fetches and rechecks the remote tag's commit before creating the draft and again before publication, and requires the release still be a draft at the final gate. Existing published releases **or drafts** fail closed rather than being overwritten. API errors also stop the run. Concurrency queues instead of cancelling an active publication.

The release sequence is:

1. Commit the reviewed source/workflows/registry and push main; wait for read-only CI. Create and push the explicit `v0.1.0` tag at the intended main commit. These are intentional maintainer publication actions, not local verification commands.
2. The release job repeats **full `make ci` and mandatory `make smoke`**, including both native ABI probes, original fixture cases, real CPA production SQLite/auth/privacy checks, and clean restart history. Missing Docker, download/hash failure, libc/native-load failure, or test failure blocks publication.
3. Copy the exact accepted smoke production `.so` into `dist`, then `make package-tested checksums verify-release` with **no rebuild**. Compare native bytes, verify checksums, log the actual GLIBC references, and rehearse both CPA SDK installers against the freshly verified CPA source. The root registry must remain unchanged.
4. Use `gh release create --verify-tag --draft` with exactly the ZIP, `checksums.txt`, and direct `registry.json`, plus the reviewed release notes. Download the draft assets, require its draft state/tag/exact asset set, compare all uploaded bytes, and repeat package/checksum/actual SDK validation on the downloads.
5. Only after all gates pass, `gh release edit --verify-tag --draft=false --latest` publishes it. Independently fetch public raw-root/direct registry URLs and release downloads after publication; this checks real hosting/redirects/accessibility that the offline installer rehearsal cannot prove.

A failed upload/draft validation deliberately leaves a draft for inspection. A rerun refuses that existing draft: inspect the failure and use a separately authorized maintainer action to remove/recover it before rerunning the existing tag workflow. Never delete or replace a published release to make a rerun pass. Tag URLs are version-specific but are not cryptographically immutable without repository protections; this workflow does not change repository settings. Source/registry trust remains necessary because checksums detect corruption, not a malicious publisher.

Hosted builds are glibc builds on Ubuntu 24.04, accepted by the pinned native-smoke image above. Do not assume Alpine/musl or older glibc compatibility. Earlier Go 1.27.1/GCC 15.2.0 local artifacts referenced GLIBC through 2.34; inspect the hosted workflow's `readelf` output for the release's actual requirements. No workflow configures a CPA deployment, store source, production credentials, or provider traffic.
