# Historical local prepublication verification — Token Usage 0.1.1 — 2026-09-09

**Historical snapshot, not current release status.** All references below to uncommitted/unpublished state, unavailable URLs or unexecuted hosted workflows describe the local prepublication snapshot. The artifact hashes identify that local validation run only; they do not establish the bytes or status of a later hosted release.

**Version 0.1.1, local, uncommitted and unpublished**, on `feature/token-usage-sidebar`. **All required final post-review local acceptance gates passed.** This records the implemented candidate separately from the unchanged [historical v0.1.0 verification](verification.md) and [v0.1.0 release notes](release-notes-0.1.0.md). The prior published release is not being overwritten. No new hosted workflow run, candidate publication, deployment or production credential/configuration inspection is claimed.

## Completed final local results

The consolidated manifest is `/tmp/token-usage-final-validation.DMNvJwhB/manifest.json`. The documentation pass inspected it, the final gate logs, saved native-browser results and artifacts; independently confirmed all six recorded gate exit codes were zero; parsed the mandatory browser run/pass; and verified the final asset sizes/hashes and exact ZIP members. The table describes this final run, not a relabeled earlier snapshot.

| Check | Evidence/status |
|---|---|
| Final `make ci` | **Passed**, `make-ci.log` under the manifest directory: **39 Python release/store/acceptance-guard contracts**, production Go tests, production/nativefixture race suites and vet, ordinary and production c-shared builds. Native smoke separately builds/loads the nativefixture library. |
| Shipped-page real browser suite | **71 scenarios passed**, with pinned `agent-browser`/Chrome and the shipped page. `browser.log` and `browser/go-test.jsonl` retain exactly one `TestSidebarBrowser` run/pass plus package pass, independently checked; skip/no-tests cannot satisfy this gate. Real `store.MakeCoverage` drives moving-retention fixtures; actual CPA JSON integration is separately covered below. |
| Candidate native smoke | **Passed**, `native-smoke.log`, work `/tmp/token-usage-smoke.ubZRD0hP`: **2 direct ABI probes, 30 nativefixture and 38 production HTTP executions**, updated fixed-public-shell/private-auth contract and persistence/restart checks. |
| Official-image actual store handler to startup | **Passed**, `store.log`, work `/tmp/token-usage-store-o7yk257l`: generated enabled/store-only config, default storage on one plugins volume, registration/menu/private auth, restart and exactly one new event. Exact image and assertions below. |
| Native-page browser with real CPA JSON | **Passed** before and after restart: native resource + pinned official codec synthetic remembered session + actual authenticated CPA status/summary/models rendered **1 then 2 events**, with actual HTTP security-header checks, no external requests and a positive CSP probe blocking an unapproved inline script (`script-src-elem`). **Not full console login or sidebar-navigation testing.** |
| Final candidate package and CPA SDK | **Passed**, `package.log`: accepted production-library equality without rebuilding, exact ZIP/license/frozen-notice/checksum/registry checks and **both actual pinned CPA SDK installer modes**, including idempotence, unsupported-platform and corrupt-checksum rejection. Final local assets are recorded below. |
| Light accessibility | Final retained axe **4.12.1**, WCAG 2 A/AA: **25 passes, 0 violations, 0 incomplete**, 35 inapplicable checks. JSON independently inspected during the documentation update. |
| Dark accessibility | Same final result: **25 passes, 0 violations, 0 incomplete**, 35 inapplicable checks. JSON independently inspected during the documentation update. |
| Browser screenshots | Final light/dark desktop and narrow dark screenshots retained below; the test uses 1280×960 and 390×844 viewports, checks document overflow and a keyboard focus transition. This is not universal visual/accessibility or multi-browser certification. |
| Static checks and cleanup | **Passed** per final manifest: actionlint **1.7.7**, Bash/Node syntax and `git diff --check`; no repository test databases or disposable store containers/volumes/networks left. **38 frozen runtime/frontend/package inputs stayed unchanged** throughout final validation. |
| Official console source fixture provenance | Independently downloaded the three exact pinned public source files during the documentation update and compared to repository bytes: **all byte-identical**, hashes below. No operator storage/session was read. |
| Reference attribution | Pinned reference `LICENSE` objects read locally; Account Health uses `Copyright (c) 2026 NoorChasib`; Auto Baseline/Reset Priority use `Copyright (c) 2026 Noor Chasib`. Console fixture LICENSE is `Copyright (c) 2026 Router-For.ME`. Full MIT attribution is included in the frozen `docs/third-party-notices.txt`. |
| Documentation validation | **Passed** for all eight active Markdown files: `git diff --check -- README.md docs`, balanced fences/whitespace, **46 local links/anchors**, reference definitions and **43 source links** (full commit pins except deliberately historical tagged README links). Frozen notices retained their digest; historical `docs/verification.md`, `docs/release-notes-0.1.0.md` and original `token-usage-project-handoff.md` remained byte-identical to HEAD. All listed reference files exist at their exact pinned git objects. |

### Retained browser artifacts

- `/tmp/token-usage-final-validation.DMNvJwhB/browser/go-test.jsonl`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/sidebar-light.png`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/sidebar-dark.png`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/sidebar-narrow-dark.png`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/sidebar-moving-retention.png`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/accessibility-light.json`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/accessibility-dark.json`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/retention-mature720h-30d.json`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/retention-mature168h-7d.json`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/retention-mature24h-24h.json`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/retention-mature12h-24h.json`
- `/tmp/token-usage-final-validation.DMNvJwhB/browser/retention-mature168h-30d.json`

These are the final **71-case local acceptance run's** disposable evidence files, not public release assets. Earlier UI evidence at `/tmp/token-usage-sidebar-opus-fixes` and other intermediate directories does not substitute for this run. Commands and exact test-only Node/npm/CLI/Chrome pins are in [development](development.md#real-browser-suite).

### Verified console fixture hashes

Official Management Center **v1.22.15**, commit `ed5f1c48e11ba7335f1e8f676f228c280196af85`:

| Repository fixture under `internal/plugin/frontendtests/upstream/` | Exact upstream filename | SHA-256 |
|---|---|---|
| `encryption.ts` | `src/utils/encryption.ts` | `d40ea30965f76400ee4152e113bd73266d17215cf5fbf74239cdeec709236f6b` |
| `secureStorage.ts` | `src/services/storage/secureStorage.ts` | `573222bfa00616bf311bad3bfdf6e5847350b0eb347871faa648650c54c97138` |
| `LICENSE` | `LICENSE` | `48fc0d19e5d0918e7e2a7df2deed33b167d9aad1aebb07891a32a8e6f71f80ee` |

The actual console storage writer/codec produces browser-test sessions after Node type stripping. The production page contains its own constrained compatible reader, not these test setters or a new bundled framework. Source links, pins and design/auth rationale are in [sidebar-audit.md](sidebar-audit.md).

## What the completed tests cover

Implementation Go regressions include mapping-shaped CPA `store` metadata with strict real-option rejection; default path captured once at `New`; unchanged shared-root/private leaf/file permissions; preserved explicit history and no store/auth/cwd discovery; initial storage failure with discoverable metadata/sanitized 503 and no fabricated history; corrected configuration before first success; same-config reopen/history binding; terminal shutdown; exact public resource registration/dispatch; fixed bytes/no-private-data across state and query variations; CSP/security headers.

The **71 browser scenarios** exercise remembered modern plain/obfuscated sessions; same-origin iframe auto-loading; real cross-tab storage changes; prefix/trailing-management normalization; narrow legacy-only recovery; missing, malformed, remember-off, invalid-version, blocked-storage and stale-legacy logout cases; wrong origin/scheme/port/prefix/userinfo/query/fragment; 401/403; redirects and HTML/MIME/schema/source/counter rejection; timeouts/aborted stale work and rejected-stream cleanup; stopped/unavailable/empty/degraded states; exact counters above 2^53; inert hostile model text; filters/pagination; custom nanoseconds/calendar validation; proactive rolling-retention margins and explicit 416 fallback.

The latest expansion added **18 real-browser regressions**. Moving-coverage fixtures call actual `store.MakeCoverage` with a clock advancing **2 ms on every status/summary/models request**. Mature 720h/30d, 168h/7d and 24h/default selections load automatically with **exactly three requests per selection**, rather than failing at the old boundary and retrying. A preset requested at/before the rolling floor selects exactly **reported floor + 60 seconds**; the tests assert the excluded `[reported floor,new from)` interval, moving-boundary reason, non-estimation and **not the full retained window** disclosure. Interior presets and young/stable history—including exact equality at a stable start—omit no arbitrary minute.

Custom prefill reuses a suitable selected range or suggests an interior range with the same disclosure. **Typed custom dates never auto-shift**: a valid start inside the suggested one-minute margin stays exact; an expired start stays entered and stops after status validation. The separate 416 case still makes only status plus rejected summary before offering explicit button recovery; it never automatically retries. The fallback is not the only way a one-minute margin is selected.

Key tests now accept internal spaces in plaintext/obfuscated remembered sessions and verify printable Latin-1 **raw header byte `0xe9`**, not Go's UTF-8 encoding of that character. Validation uses the browser's **Headers ByteString** rules while rejecting CR/LF/NUL, untrimmed values, other disallowed controls and unsupported Unicode before requests. This is not a compatibility claim for arbitrary operator UTF-8 credentials. The suite also checks allowlisted same-origin requests, explicit authorization/security flags, no rendered management key and no unintended CSP violations.

This is scoped evidence: default-port matching is visible in the URL-normalization implementation, but a separate default-port browser case is not listed in the reported suite. Same-origin frame execution is covered; an actual cross-origin frame-denial execution is **not claimed** solely because the correct CSP header is present.

## Completed official-image regression and exact tested boundary

**Image:** `eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`, tested Linux amd64, Debian bookworm/glibc **2.36**, cwd **`/CLIProxyAPI`**. CPA contract: v7.2.155 / `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. This is the actual official CPA application image, not the source-built native suite's separate Python runtime image. The native build used Go 1.27.1/GCC 15.2.0 glibc; no general older-libc or Alpine/musl claim follows.

The coordinator relayed the completed final run; this documentation update inspected its manifest/log, result JSON, generated config, permissions and native-browser enforcement result. Assertions in `scripts/native/store_smoke.py` establish:

1. **Exact prior-release negative control, optional/manual local execution:** immutable checksum-verified local published v0.1.0 bytes installed successfully through the actual management handler, but `registered`/`effective_enabled` stayed false and authenticated status returned **404**. This reproduces the original registration failure rather than mistaking inventory HTTP 200 for native success. It is **not an automated CI/release gate**.
2. **Actual candidate installation:** the handler rejected corrupt archive SHA before enabling a plug-in, then installed byte-identical accepted production `.so` bytes. Generated plug-in config contained **exactly `enabled` and `store`**, with no `database-path`; reinstalling the same artifact was idempotent.
3. **Discovery/privacy:** native metadata/config field became discoverable, exactly one public **Token Usage** menu was registered, and all private routes rejected missing/wrong credentials. The public shell stayed fixed across auth/query/history states, with no private canaries; alternative resource/private spellings did not expose history.
4. **One persistent plugins volume:** SQLite opened at `/CLIProxyAPI/plugins/data/token-usage/usage.sqlite` beneath the sole named volume mounted at `/CLIProxyAPI/plugins`. The shared root stayed **0755**, the private leaf **0700**, and the database/SHM/WAL/lock **0600**. Test fixture configuration uses separate disposable scaffolding, not a production Compose/data-mount addition.
5. **Real collection and restart:** one synthetic executor observation committed; restart preserved the historical query unchanged, then one new execution increased the count from **1 to 2**. The quiescent SQLite copy had integrity `ok`, two rows, input 200/output 40, and no unclean run markers. No replay or lossless-delivery inference is made.
6. **Native browser integration:** a real browser opened the native resource, used a synthetic remembered session generated by the pinned official console codec, and consumed real CPA-authenticated status/summary/models JSON. It rendered **1 event before restart and 2 after**, with actual HTTP security-header checks and no external requests. An unapproved inline-script probe **did not execute** and produced the expected **`script-src-elem`** CSP violation, separately from the zero unintended violations in normal page operation. **The actual console sign-in screen and sidebar-navigation click path were not exercised.**
7. **Unavailable-storage case:** valid generated configuration still registered and exposed the menu/resource when default storage could not open. Authenticated APIs returned sanitized **503** without fabricated totals/coverage, and the public bytes matched the healthy instance even after attempted usage.

Retained evidence:

- `/tmp/token-usage-store-o7yk257l/old-release-red/inventory.json`
- `/tmp/token-usage-store-o7yk257l/installed/handler-generated-plugin-config.json`
- `/tmp/token-usage-store-o7yk257l/installed/inventory.json`
- `/tmp/token-usage-store-o7yk257l/installed/results.json`
- `/tmp/token-usage-store-o7yk257l/installed/permissions.txt`
- `/tmp/token-usage-store-o7yk257l/installed/browser-before-restart.log`
- `/tmp/token-usage-store-o7yk257l/installed/browser-after-restart.log`
- `/tmp/token-usage-store-o7yk257l/installed/browser-after-restart/native-browser.json`
- `/tmp/token-usage-store-o7yk257l/installed/browser-after-restart/native-sidebar.png`
- `/tmp/token-usage-store-o7yk257l/storage-unavailable/inventory.json`

Runtime isolation uses a Docker **internal bridge with no external egress**, a fixture sharing CPA's loopback namespace, and a fixed-destination **host-127.0.0.1** reverse proxy for browser ingress. The registry/upstream are synthetic loopback fixtures. A fixture-only store-auth rule matches **only `http://127.0.0.1:8318/`**, with `type: none` and `allow-insecure: true`; no real operator config or security policy is changed. The no-Compose-change guarantee concerns the existing plugins data volume, not a claim that the test has no fixture scaffolding.

Reproduction uses `make browser-tools`, `AGENT_BROWSER_ARGS=--no-sandbox make browser`, `CPA_SOURCE_ARCHIVE=/absolute/path/to/verified/cpa.tar.gz make smoke`, an exact-digest Linux-amd64 `docker pull`, then `AGENT_BROWSER_ARGS=--no-sandbox make store-smoke CPA_SMOKE_WORK=/tmp/token-usage-smoke.REPLACE`. The optional old-library control requires already retained checksum-matching published bytes. [Development](development.md#candidate-installation-to-startup-gate) gives full commands, tool integrity pins and isolation details.

## Final accepted local artifacts

Saved directory: **`/home/noor/Code/cpa-plugins/plugins/token-usage/dist/0.1.1/`**. This separate version directory preserves the historical v0.1.0 ZIP/registry/checksum files at the dist root. The following are accepted local candidate bytes, **not published assets**:

| File in the saved directory | Size (bytes) | SHA-256 |
|---|---:|---|
| `token-usage_0.1.1_linux_amd64.zip` | 3,215,797 | `bc27bf8934b01c01452284294eed5c730da8359ebb0cbd14bc10453dafdb0a7f` |
| `token-usage.so` | 7,541,528 | `081a28f45e20efe320aa80e06e5a8757f4162e96a1990f3505a8783c32e7e6dc` |
| `registry.json` | 829 | `44113b8140a1547de32c46e88026610377eafff7f62297f1a53eece656efa330` |
| `checksums.txt` | 180 | `09fc224d1d6e9f81b48a3aa2175baffc31712236e7bcdb8c5830d7b0f9d5f8b2` |

The ZIP has exactly root `token-usage.so`, `LICENSE` and `THIRD-PARTY-NOTICES.txt`. Its library is byte-identical to `/tmp/token-usage-smoke.ubZRD0hP/token-usage-production.so` and `/tmp/token-usage-store-o7yk257l/installed/installed.so`: **no rebuild occurred between native/store acceptance and packaging**. The notice bytes equal the frozen repository file, SHA-256 **`d389b376b66a71072ff75fb36a7b4a4ce9e2a6bdbc32bca12577ea945132a9b0`**. The documentation pass independently checked all four final file sizes/digests, the ZIP's exact members/library/licenses and all **38** input digests in `inputs-before.json`.

`package.log` records `scripts/release.py package`, `checksums` and `verify` with version `0.1.1` and the saved directory; archive/registry checksum checks both report `OK`. Both real CPA SDK registry modes passed installed-byte equality, idempotence, unsupported-platform and corrupt-checksum rejection against `/tmp/token-usage-smoke.ubZRD0hP/source`. Local URL interception makes this an installer contract check, not a public-hosting assertion.

## Final review and acceptance status

The coordinator reports core/UI review findings independently resolved, including the real moving-retention and HTTP-header-key corrections; active test counts are **71**. Integration-review findings were corrected and independently reverified: actual HTTP CSP/security-header checks plus the positive blocked-inline-script probe; mandatory browser test **run/pass** rather than skip/no-tests; version-consistency guards; and response/observer cleanup. The optional published-v0.1.0 negative control is accurately recorded as manual/local, not a mandatory CI gate. The documentation-status concern is resolved. No required local review or acceptance blocker remains. A small shared-fixture extraction suggestion was consciously declined rather than adding another fixture package; it was not a correctness defect.

The final consolidated **39-contract CI / 71-case browser / native / official-image store / package / static** gates all exited **0**. Their logs and `.exit` records reside in `/tmp/token-usage-final-validation.DMNvJwhB/`. Frozen runtime/frontend/package inputs were unchanged through the sequence; the final manifest records cleanup and unchanged historical dist assets. This documentation finalization changes neither the native library nor the frozen notices.

Both current workflows make browser, source-built native and official-image/native-browser gates mandatory before package validation or publication. **No required local gate is pending.** The new workflow steps have **not run on hosted CI** because no candidate commit/push/tag occurred. Prepared workflows and local passes do not authorize those actions, a release or a deployment. No v0.1.1 public URL or hosted candidate release is available, and published v0.1.0 remains unchanged.

## Limits and work not performed

- Deployed CPA/console versions, reverse-proxy origin/prefix/framing policy, production auth/config/env files, existing volumes and live traffic were not inspected or changed.
- No provider calls, billing tests, physical power-loss test, arbitrary filesystem/libc/platform guarantee or claim of lossless upstream delivery. Retained raw counters remain CPA-reported; completeness is unknown.
- No new credential bridge, operator login flow, Compose edit/additional mount, reference-project modification, history migration/reset, commit/push/tag/publication/deployment is performed by this documentation task.
- Full upstream console sign-in/sidebar-navigation clicks, a separate default-port browser execution and actual cross-origin frame-denial execution were not tested. Positive inline-script CSP enforcement is not a substitute for those tests; they are coverage limitations, not unfinished required local gates.
- Same-origin plug-ins share the console credential trust boundary. Remembered storage uses obfuscation, not secure encryption or isolation. Remember-off/parent-memory-only sessions remain unavailable to the sidebar by design.
- The historical v0.1.0 artifact hashes, all-public-404 behavior, test counts and prepublication statements remain historical. They are not the current sidebar contract or proof of candidate acceptance.
