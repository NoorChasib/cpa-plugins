# Local validation stage — 2026-09-09

This is a historical snapshot **before release preparation/publication**. Artifact hashes, loopback registry details, command/test counts, and work-not-performed statements below describe that local stage, not the current release workflow or a hosted run. See [development and releases](development.md) for the current packaging/publication contract.

The scoped Token Usage 0.1.0 MVP was implemented and simplified by configured `gpt-6-astra-xhigh` subagents, then reviewed and reverified by configured `opus-xhigh` subagents. No model overrides were used. The original handoff remains unchanged.

## Tested target

- CPA v7.2.155, commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`, unmodified source built from the checksum-verified upstream archive.
- Linux amd64 only; native ABI 1, RPC schema 6; Go 1.27.1 and GCC 15.2.0.
- Locally built artifact references GLIBC versions through 2.34. Alpine/musl and other platforms are not covered.
- Synthetic local OpenAI-compatible and Claude upstreams inside a digest-pinned network-disabled container. Build-time source, dependency and image downloads can use the network.
- Deployed CPA version and production configuration were not inspected.

## Standards review

One documented-policy defect was fixed and independently reverified: disk-budget and other admission faults no longer prevent querying readable committed history. Actual reader errors and closed storage still fail; successful degraded queries carry collection-health annotations.

Three nonblocking judgement-call cleanups were intentionally skipped: retention-floor helper extraction across packages, SQLite companion-suffix deduplication, and abstraction of three explicit route names. No safety defect was established for these; they do not block the scoped implementation.

## Spec review

All three confirmed findings were fixed and independently reproduced after correction:

1. **Persistent faults were being masked by unrelated successes.** Faults are now operation-specific, recovery probes do not admit trial events, and lifetime storage errors remain visible. Permanent maintenance failure stays degraded; permanent writer failure refuses subsequent observations instead of silently reopening admission.
2. **Retention cleanup had a fixed 512-events-per-interval ceiling.** Cleanup now uses bounded steps with prompt continuation and yields between steps. The reviewer drained 20,000 expired rows in 40 direct store steps, preserving fresh history; collector tests also exercise scheduled continuation. This is a regression measurement, not a production throughput guarantee.
3. **Late callbacks were invisible after quiesce.** Stopped status remains available and counts late normal/oversized observations and stopped drops. Same-config reopen is supported; post-final-sample counters remain explicitly RAM-only best effort and may disappear on reopen/unload.

No new correctness regression was found during reverification. A documented conservative policy remains: any active maintenance failure stops admission until the next successful configured maintenance attempt (default one minute). Reads can remain available. This trades possible additional collection drops during recovery for fail-closed admission; there is no replay.

Review totals: Standards — 1 actionable defect fixed, 3 nonblocking cleanup suggestions skipped; Spec — 3 defects fixed. No unresolved confirmed defect from either review axis.

## Final commands and results

```sh
make ci package-current checksums verify-release VERSION=0.1.0
make smoke
python3 scripts/verify-cpa-package.py --cpa-source /path/to/pinned-cpa-source
```

The final smoke invocation explicitly reused the already hash-verified archive through `CPA_SOURCE_ARCHIVE=/tmp/token-usage-smoke.XVovALv1/cpa.tar.gz`; omitting the variable downloads the same pinned archive. The installer rehearsal used `/tmp/token-usage-smoke.Q95Dylvc/source`.

All final checks passed; no required check was silently skipped:

- Formatting, vet under both build tags, Go unit tests, production and `nativefixture` race suites, ordinary build and both native shared libraries.
- 11 Python release-contract tests, shell/Python syntax checks, ZIP/checksum/registry checks, actual fixture-binary rejection and packaged-library equality with the final production smoke library.
- Both direct native ABI probes: lifecycle, bounded envelopes, sanitized errors, exact large counters, SQLite persistence, quiesce/late diagnostics, closed queries, same-config reopen, shutdown/reinitialization.
- 30 retained Phase A HTTP executions through the real CPA executor-to-native path, with `usage-statistics-enabled` false and true.
- 38 production HTTP executions, 19 per setting, producing 18 committed observations per setting because the known Claude missing-usage case emits no native event. All seven raw counters, provider/model grouping, executor provenance, authentication, public no-leak, SQLite integrity/privacy, clean restart history and a subsequent single increment were asserted.
- Input `9007199254740993` stayed exact through Claude's real nonstream executor, native ABI, SQLite and authenticated JSON. The separate ABI probe preserved exact aggregate `18014398509481986` across restart.
- Actual CPA registry/platform/checksum/ZIP installation, installed-byte equality, idempotent reinstall and corrupt-checksum rejection, using a file-backed test transport with no network requests.

Earlier development failures were resolved rather than hidden: exhausted host inotify quota was isolated using the disposable container without changing system limits; fixture attribution/assertions were reconciled with real pinned behavior; the prepared registry uses inert HTTPS loopback metadata rather than requiring insecure transport. See the compatibility/development documents for context.

## Final local artifact

`dist/token-usage_0.1.0_linux_amd64.zip`

- Size: **3,186,792 bytes**.
- SHA-256: **`e31c14454d15c64256a356a5817805a462172d578603aacab2e7496416d9a06f`**.
- Contents: production `token-usage.so`, `LICENSE`, `THIRD-PARTY-NOTICES.txt`.
- `dist/checksums.txt` and the local preparation `registry.json` match this artifact.

These bytes reflect the local toolchain and verified source at handback. Rebuild and regenerate checksums/registry after code, dependency or build changes. Nothing is hosted at the inert loopback registry URL.

## Local evidence

These disposable paths are retained on this machine, not checked-in portable fixtures:

- `/tmp/token-usage-postreview-build.log`
- `/tmp/token-usage-postreview-smoke.log`
- `/tmp/token-usage-postreview-validator.log`
- `/tmp/token-usage-smoke.Q95Dylvc/runtime/token-usage-production-fztf61ac/results.json`

The scripts, synthetic fixture definitions and regression tests are in the repository so results can be reproduced without these temporary paths.

## Limits and work not performed

- No billable/live-provider requests, production access/configuration changes, deployment, publication, commit, push, tag, release or hosted CI dispatch.
- No claims for other platforms, libc variants, CPA versions, live provider accuracy, physical power loss or a truly full host filesystem. SQLITE_FULL was induced with SQLite's page limit; abrupt process exit/WAL recovery was tested.
- Real executor coverage is OpenAI-compatible/Claude for the documented paths, not Codex/Gemini/OAuth/WebSockets/translated protocols/downstream cancellation/retry orchestration/additional image-model events.
- CPA omissions, asynchronous admission-to-commit loss, raw-only retention, eventual query consistency and best-effort diagnostics remain explicit. The disk budget is monitored, not a hard filesystem quota.
- No UI, rollups, CSV, account/alias drilldown, pricing or cross-instance aggregation. Operator deployment and backup/restore procedures are documented, not executed against an operator installation.
- Source is an initial uncommitted project. No existing health-plugin implementation was modified.
