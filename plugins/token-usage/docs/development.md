# Development, native acceptance and candidate packaging

**Active runtime/build target: 0.1.1.** Published v0.1.0 is unchanged. Runtime, Makefile, local registry/archive contracts and guarded workflows agree on 0.1.1. **All final post-review local gates passed**: 39 Python contracts, Go tests/vet/race/builds, 71 actual browser scenarios, both native ABI probes, 30 nativefixture and 38 production HTTP executions, official-image/store/native-browser/restart acceptance and exact tested-library/package/both SDK validation. The local prepublication snapshot saved files under `dist/0.1.1/`; its results do not establish hosted release status. Never relabel a candidate as 0.1.0 or overwrite the published release.

## Commands and boundaries

Run from the repository root on Linux amd64. Core tools: Go 1.26.0+, GCC/CGO, Make, Python 3.11+, curl, tar, unzip, sha256sum and Docker. Local validation used Go 1.27.1 / GCC 15.2.0. Modules are pinned by `go.sum`; SQLite is bundled. Required browser acceptance pins **Node 26.8.1, npm 11.19.0, agent-browser 0.37.1 and Chrome for Testing 153.0.8010.36**. The npm package/lock under `scripts/browser/` is test-only; no frontend framework, bundler or npm runtime dependency is added to the embedded page.

| Command | Purpose |
|---|---|
| `make fmt-check` | Check Go formatting without editing. |
| `make vet` | Vet production and `nativefixture` builds. |
| `make test` | Go tests and Python release-contract tests. The browser test is opt-in; an ordinary Go pass is not a browser pass. |
| `make test-race` | Uncached production and `nativefixture` race suites. |
| `make build VERSION=0.1.1` | Ordinary Go build; not the native loadable artifact. |
| `make c-shared VERSION=0.1.1` | Candidate production shared library in `dist/token-usage.so`. The generated header is not packaged. |
| `make ci VERSION=0.1.1` | Formatting, vet, unit/Python/race tests and builds. It does not replace native acceptance or the opt-in browser suite. |
| `make browser-tools` | Require exact Node/npm; install the locked test CLI without lifecycle scripts and hash-verified pinned Chrome; verify binary hashes/versions. |
| `make browser` | Required real-browser fixture suite via pinned tools; missing tools/assertions fail. |
| `make smoke` | Native acceptance: both direct ABI libraries, retained Phase A and production CPA/SQLite/auth/restart checks. |
| `make store-smoke CPA_SMOKE_WORK=/tmp/token-usage-smoke.REPLACE` | Real official-image store handler, generated-config/default-volume/native-browser/restart acceptance using that smoke run's exact production library, without rebuilding. |
| `make native-spike` / `bash scripts/native/run-spike.sh` | Retained original nativefixture entry point; not sufficient for production persistence or store-to-startup installation. |
| `make package-current checksums verify-release VERSION=0.1.1 DIST=dist/0.1.1` | Fresh candidate build/package validation, not publication or proof those new bytes passed native acceptance. |
| `make package-tested checksums verify-release VERSION=0.1.1 DIST=dist/0.1.1` | Package the already accepted production `DIST/token-usage.so` without rebuilding; first stage the exact accepted native-smoke library there. |

The candidate gate status and retained evidence are in [verification-sidebar.md](verification-sidebar.md). Version defaults are 0.1.1; explicit versions above make local intent clear. Replace `.REPLACE` with the directory printed by the matching fresh native run. A listed command is not a final post-review pass: changes to source or notices require the affected gates and package/checksum validation again.

For this candidate, use **`DIST=dist/0.1.1`** (or another dedicated staging directory) for packaging so historical dist-root v0.1.0 ZIP/registry/checksum assets are not overwritten; do not mix stale version/platform archives. Intended candidate bundle: `token-usage_0.1.1_linux_amd64.zip`, containing exactly root `token-usage.so`, `LICENSE` and `THIRD-PARTY-NOTICES.txt`. The latter is sourced from `docs/third-party-notices.txt`, including the sidebar/console MIT credits. `checksums.txt` covers the ZIP and generated direct `registry.json`. Repackage/regenerate/verify together after any binary or notice change.

`nativefixture` is regression-only: it exposes up to 64 sanitized observations in authenticated status and is rejected by production packaging. Test-only Go/browser servers and copied console TypeScript fixtures are not production endpoints or a credential setter. ELF/build metadata checks do not replace native loading or authenticate an untrusted artifact.

## Real browser suite

The Go fixture serves the **shipped CSP-protected page** and synthetic typed API responses over loopback. It is not an actual deployed console or CPA authentication middleware. Its controller executes in a real browser using `agent-browser`, including same-origin iframe and cross-tab storage-change cases. Load the browser skill before automating a run in an agent session.

```sh
# Require Node v26.8.1 and npm 11.19.0 on PATH first.
make browser-tools
AGENT_BROWSER_ARGS=--no-sandbox make browser
```

`--no-sandbox` is the explicit **test-only** Chromium exception used on the disposable runner; it is not a production browser recommendation or a relaxation of the page CSP/CPA authentication. Use no real credentials or operator browser profile. `scripts/browser/install-node.sh NEW_DESTINATION` can install the pinned Node archive into a new local directory without a global install or overwriting an existing destination. `make browser-tools` uses `npm ci --ignore-scripts --no-audit --no-fund`, then verifies the exact CLI binary and installs the pinned engine under its ignored test-owned directory. There is no fallback to a moving global CLI/browser in required acceptance.

Integrity pins enforced by the scripts/lock:

| Tool | Version and integrity |
|---|---|
| Node Linux x64 archive | 26.8.1, SHA-256 `3e301118d7df53d563b7e96c1617545f26e2f76f9724be668d6cab65c15dda5d`; npm 11.19.0 is required by `make browser-tools`. |
| agent-browser | 0.37.1, npm package integrity in `scripts/browser/package-lock.json`; Linux x64 binary SHA-256 `f8e5f9294bd0da70dda61854f12004fd61c668cd682bfb600cdf6d0df73dea69`. |
| Chrome for Testing Linux x64 ZIP | 153.0.8010.36, SHA-256 `167a098c4fdec156b58a9f678c90a84f9072d789f9c6e7b35496a6987b8b7ef8`. |

`make browser` sets the Go test opt-in and prints a fresh `/tmp/token-usage-browser-artifacts.*` path; `TOKEN_USAGE_BROWSER_ARTIFACTS` can select an explicit synthetic artifact directory. It retains `go-test.jsonl` and pipes the JSON event stream through `scripts/browser/require-go-pass.py`, which requires **exactly one `TestSidebarBrowser` run/pass plus package pass**. Skip, no matching test, duplicate execution or failure cannot satisfy the mandatory gate. The underlying Go test still skips without its opt-in, so an ordinary Go pass alone is not browser evidence. These test controls require no live login or operator environment access.

Source files:

- `internal/plugin/browser_fixture_test.go`: loopback fixture and test process setup.
- `internal/plugin/frontendtests/sidebar.browser.cjs`: real browser scenarios, request assertions, screenshots and axe audits.
- `internal/plugin/frontendtests/upstream/encryption.ts`, `secureStorage.ts`, `LICENSE`: byte-identical official Management Center v1.22.15 files at `ed5f1c48e11ba7335f1e8f676f228c280196af85`. Node strips TypeScript for execution, so sessions are generated by the real console codec/storage writer, not a separately invented encoding. [Audit](sidebar-audit.md) gives exact upstream filenames and provenance.

The final post-review run passed **71 scenarios**, retained at `/tmp/token-usage-final-validation.DMNvJwhB/browser`, with the mandatory exact test run/pass independently checked; final Go tests and both race variants also passed. Coverage includes modern plaintext/obfuscated sessions, missing/blocked/remember-off states, authoritative modern logout/malformed state versus stale legacy keys, narrow legacy-only recovery, wrong origin/scheme/port/prefix, proxy subpaths, 401/403, redirect/HTML/MIME/schema/counter rejection, timeout/cancellation/stale responses and rejected-stream cleanup, unavailable/stopped/empty/degraded states, exact large counters, hostile strings, filters/pagination, keyboard focus and light/dark/narrow layout. Requests stay same-origin/allowlisted with explicit security flags and no rendered management key.

Retention regressions use **the real `store.MakeCoverage` constructor**, with its clock advancing **2 ms per request** rather than a frozen hand-written boundary. Mature 720h/30d, 168h/7d and 24h/default selections load once with **three requests per selection**, without an initial 416/retry. When a requested preset starts at/before the rolling `coverage.from == retention_floor`, it proactively selects exactly **reported floor + 60 seconds**, preserving nanoseconds and visibly disclosing the excluded interval, moving-boundary reason and **not the full retained window**. Interior presets and young/stable coverage lose no arbitrary minute. Empty custom fields reuse the selected range or suggest an appropriately disclosed range inside coverage; typed custom timestamps never shift automatically. Separate 416 tests retain explicit button recovery as a fallback, not the only source of the minute margin.

Key regressions exercise internal spaces in plaintext/obfuscated remembered sessions and a Latin-1 byte through actual HTTP headers. The controller uses the browser's **Headers ByteString** contract rather than ASCII-only validation, while rejecting CR/LF/NUL, untrimmed values, disallowed controls and unsupported Unicode before requests. The Go fixture expects raw byte **`0xe9`**, not UTF-8 for the visible character; this is not a compatibility claim for arbitrary operator UTF-8 credentials. The final 71-case UI run complements the separately passed final native/store/package gates; it does not replace them.

Retained axe 4.12.1 WCAG 2 A/AA reports have zero violations and zero incomplete checks in each theme. These automated checks and synthetic screenshots are not a universal accessibility certification, a cross-browser guarantee or a deployed-console compatibility test. A same-origin iframe pass and a `frame-ancestors 'self'` header assertion do not alone establish an actual cross-origin frame-blocking execution test.

## Native runner and historical evidence

The retained source-built native runner pins:

- CPA v7.2.155 commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`.
- Source archive SHA-256 `3f829041f62175a546750520d1c160cba1a7618d30f17618731e250c048d77e5`.
- Official Python runtime `python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285`, Linux amd64. **This is not the official CPA application image**: the runner compiles CPA from unmodified verified source.

An optional `CPA_SOURCE_ARCHIVE=/absolute/path/to/verified/cpa.tar.gz make smoke` reuses a local archive only after the same hash check. Otherwise the pinned source is downloaded. Runtime evidence is retained in the fresh `/tmp/token-usage-smoke.*` directory printed by the runner; old temporary directories are not searched implicitly. Go caches can be reused. Build-time dependency/source/image downloads can use the network; runtime requests are synthetic loopback inside a network-disabled container.

The historical runner uses a read-only root, read-only scripts/binaries, a fresh writable synthetic directory, dropped capabilities except narrowly needed CHOWN, no-new-privileges and resource limits. A disposable root UID avoided a host-user inotify quota problem without changing sysctls or unrelated processes. Ownership restoration concerns only the fresh fixture directory; the Docker socket is not mounted into the container. Missing Docker/native loading or failed assertions must fail, not skip.

**Historical v0.1.0**, retained unchanged in [verification.md](verification.md), passed both direct ABI probes, 30 Phase A executions and 38 production executions with `usage-statistics-enabled` false and true. Production committed 18 observations per 19 executions because the known Claude missing-usage case emits no event. Tests checked raw counters, provider/model/executor attribution, exact integers above 2^53, authenticated private APIs, public no-leak behavior then applicable, SQLite integrity/privacy, clean restart and one new increment. Those old all-public-404 assertions are not the candidate contract: the candidate's exact public status resource must return fixed data-free HTML instead.

Historical logs: `/tmp/token-usage-postreview-build.log`, `/tmp/token-usage-postreview-smoke.log`, `/tmp/token-usage-postreview-validator.log`; old runtime evidence: `/tmp/token-usage-smoke.Q95Dylvc/runtime`. These are not release assets or candidate proof.

The **final candidate native acceptance passed** at `/tmp/token-usage-smoke.ubZRD0hP`: **2 ABI probes, 30 nativefixture and 38 production HTTP executions**, with updated private-auth/fixed-public-shell expectations and persistence/restart checks. Final `make ci` passed **39 Python contracts**, production Go tests, production/nativefixture race suites and vet, plus ordinary/production c-shared builds; logs are `/tmp/token-usage-final-validation.DMNvJwhB/make-ci.log` and `native-smoke.log`. The recorded 38 frozen runtime/frontend/package inputs remained unchanged through final acceptance and packaging; subsequent changes would require fresh affected-gate validation.

Direct ABI probes deliberately bypass HTTP auth and test lifecycle, exact counters, stopped diagnostics, closed queries and same-config reopen. They do not establish authentication. Real CPA HTTP tests must separately exercise missing/wrong credentials on every private route, keep each private `Menu` empty, verify the one public shell's fixed/no-private-data bytes across states and prevent resource spellings from reaching private APIs. Valid synthetic management probes reset CPA's failed-auth counter between groups so an IP ban does not obscure individual route results.

## Candidate installation-to-startup gate

**Passed final post-review local acceptance**, retained under `/tmp/token-usage-store-o7yk257l`. The actual CPA **management store-install handler → generated enabled/store-only configuration without database-path → native registration/config-field/menu discovery → executor → default SQLite → same-volume restart → native browser** path was exercised, not merely SDK archive extraction.

Exact official application image: **`eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`**, Linux amd64, Debian bookworm/glibc **2.36**, working directory `/CLIProxyAPI`. CPA source contract: v7.2.155 / `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. This does not establish the image or version of any operator deployment, or promise arbitrary older-glibc/Alpine compatibility.

```sh
# Reuse an already checksum-verified archive, or omit the variable to download it.
CPA_SOURCE_ARCHIVE=/absolute/path/to/verified/cpa.tar.gz make smoke
# Replace .REPLACE below with the fresh directory printed by that successful run.
docker pull --platform linux/amd64 eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b
AGENT_BROWSER_ARGS=--no-sandbox make store-smoke CPA_SMOKE_WORK=/tmp/token-usage-smoke.REPLACE
```

The runner requires the exact production `.so` from `make smoke`, pinned browser tools and pinned images; it never silently rebuilds. The completed regression also used an optional checksum-locked local **published v0.1.0 negative control**: the real handler installed those immutable old bytes, but native registration remained false and authenticated status returned 404. `store_smoke.py --old-library` supports that explicitly supplied retained-artifact control. It was executed **manually/optionally locally**, is not silently downloaded by `make store-smoke`, and is **not an automated CI/release gate**.

The candidate rejected a corrupt archive SHA, installed byte-identical production code, generated exactly `enabled` plus `store` with no database path, exposed the optional path field and one Token Usage public menu, and enforced missing/wrong-key rejection on all private routes. The sole persistent named volume was the existing-layout plugins mount at `/CLIProxyAPI/plugins`; the database opened at `/CLIProxyAPI/plugins/data/token-usage/usage.sqlite`. The plugins root stayed `0755`, private leaf `0700` and SQLite/companions `0600`. Restart preserved the old query unchanged; one new synthetic execution moved the event count from **1 to 2**, with SQLite integrity `ok` and clean run markers.

A real browser loaded the **native resource + pinned official codec synthetic remembered session + actual authenticated CPA JSON**, rendering 1 event before and 2 after restart without external requests. This tests that exact integration boundary; **full console sign-in and clicking through console sidebar navigation were not exercised**. A separate valid-config/unavailable-storage case retained registration/menu, fixed public bytes and private 503 without invented history/coverage.

Isolation differs from the source-built `--network none` runner: this gate uses a Docker **internal bridge with no external egress**, a fixture sharing CPA's loopback namespace, and a fixed-destination reverse proxy bound to **host 127.0.0.1** for browser ingress. A fresh fixture-config bind mount is test scaffolding, not an added production data volume or Compose requirement. The synthetic HTTP registry at `http://127.0.0.1:8318/` has only a narrowly matching fixture `store-auth` rule with `type: none` and `allow-insecure: true`; it does **not** weaken any operator configuration. Do not copy that fixture-only policy into real installation instructions. No operator credentials/configuration, live-provider calls or production mount changes were involved.

## Registry, packaging and actual CPA SDK validation

Registry schema **2** is distinct from native RPC schema **6**:

- **Committed root registry:** GitHub-release mode, repository fixed to `https://github.com/NoorChasib/cpa-plugin-token-usage`. The version is a display fallback; CPA resolves the latest published tag and corresponding `token-usage_<version>_linux_amd64.zip` plus an asset literally named `checksums.txt`. This is not a fixed-tag source.
- **Generated release registry:** direct mode, version `0.1.1`, Linux amd64 only, exact HTTPS archive URL/SHA-256/size under `/releases/download/v0.1.1/`. Release URLs become available upon publication. Local packaging computes a prospective binding; it does not upload it. The committed registry must not be rewritten with local toolchain-specific hashes.

The ZIP contains one root library and license files, never nested/multiple libraries, symlinks or escaping paths. Both install modes verify the whole ZIP checksum, including licenses. A checksum detects corruption, not an untrusted publisher. Stable ZIP timestamps/order/modes make packaging reproducible for identical inputs; unpinned compiler/runner differences can change native bytes, so bit-for-bit cross-run build reproducibility is not promised.

After packaging, use the actual verified CPA source tree printed by a successful native run:

```sh
python3 scripts/verify-cpa-package.py \
  --cpa-source /tmp/token-usage-smoke.REPLACE/source \
  --dist /absolute/path/to/cpa-plugin-token-usage/dist/0.1.1
```

The verifier builds a temporary Go harness against CPA's real SDK and maps intended registry/archive/checksum URLs to local files with synthetic release metadata. It exercises registry/platform/checksum/ZIP installers without network transport, installed-byte equality, idempotent reinstall, corrupt checksums and unsupported platforms. It is **not** the management-handler-to-startup test above and does not prove public hosting. Dependency compilation may populate Go caches. Do not substitute an unverified source tree and call it pinned validation.

**Final candidate packaging and both actual CPA SDK installers passed**, with the exact accepted production library from `/tmp/token-usage-smoke.ubZRD0hP/token-usage-production.so`, pinned CPA source `/tmp/token-usage-smoke.ubZRD0hP/source` and saved output `/home/noor/Code/cpa-plugin-token-usage/dist/0.1.1/`. `package-tested` did **not rebuild** the library; the archive library matches both native-smoke and actual store-installed bytes. The ZIP contains the exact frozen notices, SHA-256 `d389b376b66a71072ff75fb36a7b4a4ce9e2a6bdbc32bca12577ea945132a9b0`. Checksum validation, both SDK modes, installed-byte equality, idempotence and negative checksum/platform cases passed.

Use `DIST=/absolute/staging/path` with Make and the same `--dist /absolute/staging/path` with `scripts/verify-cpa-package.py`. The final candidate was deliberately saved in **`dist/0.1.1/`**, not over the historical v0.1.0 ZIP/registry/checksum assets at the dist root. Its ZIP is **3,215,797 bytes**, SHA-256 `bc27bf8934b01c01452284294eed5c730da8359ebb0cbd14bc10453dafdb0a7f`; `.so` is **7,541,528 bytes**, SHA-256 `081a28f45e20efe320aa80e06e5a8757f4162e96a1990f3505a8783c32e7e6dc`. Matching `registry.json` and `checksums.txt` are in the same folder. [Candidate verification](verification-sidebar.md) records all four file digests and final logs. These are accepted **local** bytes, not published release assets.

Pinned authoritative files: [registry schema](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginstore/registry.go), [ZIP/checksum installer](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginstore/install.go), [GitHub-release installer](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginstore/github.go), [SDK](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginstore/pluginstore.go).

## CI and publication boundary

The current read-only `.github/workflows/ci.yml` and explicitly guarded `v0.1.1` release workflow both require **locked browser-tool installation → make ci → make browser → make smoke → official-image make store-smoke → package-tested/checksum/actual CPA SDK validation**. Packaging uses the same production library accepted by both native gates; comparisons prevent a rebuild from replacing accepted bytes. No required browser/native/image gate is treated as optional. Ordinary CI has read-only permissions, no uploads, no release writes and no workflow-dispatch trigger.

The release workflow additionally checks repository/event/version/tag/main ancestry, refuses any existing release or draft, rechecks the remote tag, downloads/revalidates exact draft assets, and verifies draft state before publication. Only its publish job has write permissions; checkout does not persist credentials and only explicit GitHub steps receive their token. **Local validation does not establish that a hosted release workflow succeeded.**

Final local prepublication acceptance and artifact checks are recorded in [verification-sidebar.md](verification-sidebar.md). Hosted release acceptance must be established by the guarded workflow against its exact commit and artifacts, not inferred from local results. Existing published v0.1.0 and its assets must never be overwritten, deleted or retagged. Publishing a release does not deploy it.

Historical v0.1.0 publication used Ubuntu 24.04 / Go 1.27.1 glibc builds accepted by the source-built Python-image smoke. Earlier local bytes referenced GLIBC through 2.34; that is not a universal maximum or Alpine/musl support. Check the actual candidate artifact and intended image rather than projecting this old result onto a different runtime. No workflow or local test configures an operator deployment, reads production auth/env files or makes billable provider calls.
