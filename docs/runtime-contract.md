# Persistent runtime contract

Implementation contract for the Linux amd64 raw-only MVP. CPA ABI/schema and upstream omissions remain documented in `upstream-compatibility.md`.

## Configuration

CPA passes the `plugins.configs.token-usage` mapping as base64 `config_yaml` in lifecycle RPC. Unknown keys and invalid values fail configuration. Only `database-path` is required. `enabled` and `priority` are recognized CPA-owned selection fields, not storage options.

```yaml
plugins:
  enabled: true
  configs:
    token-usage:
      enabled: true
      priority: 20
      database-path: /CLIProxyAPI/plugin-data/token-usage/usage.sqlite
      queue-capacity: 8192
      batch-size: 256
      flush-interval: 1s
      raw-retention: 720h
      maintenance-interval: 1m
      max-disk-bytes: 1073741824
      max-models: 10000
      query-timeout: 5s
```

Bounds: queue 1–65536; batch 1–4096 and no larger than queue; flush 10ms–10s; raw retention 1h–8760h; maintenance 1s–1h; disk budget 1 MiB–1 TiB; model cardinality 1–100000; query timeout 100ms–30s. The database path must be an explicit clean absolute path in a dedicated private directory. Existing final directory must be mode 0700; database/lock/SQLite companions must be regular, owned by this process's UID, mode 0600, and not symlinks. Known network filesystem types are refused; use a local filesystem supporting SQLite WAL, never a shared/network volume.

Repeated identical configuration is idempotent while running. Storage/collector configuration changes, including path changes, require native restart: live changes fail rather than splitting history or changing retention guarantees silently. Host-owned enabled/priority changes do not constitute storage configuration changes. Quiesce closes admission, drains and joins the writer, then closes storage. The stopped collector remains reachable for status and late-delivery diagnostics: callbacks after admission closes increment `observed_events` and `dropped_stopped`, including oversized callbacks. A later register/reconfigure reopens the same configured path with a new collector; a different path remains prohibited until native restart. Final native shutdown prohibits any new worker.

The final diagnostic save samples counters once; callbacks racing that sample or arriving after it are **in-memory best effort only**, not guaranteed durable. They remain visible in stopped status until reconfigure/unload, but same-config reopen resumes the durable snapshot and may not include these late counts. No post-close background worker or database write is created to persist them. Status remains available while stopped; statistics return 503 after the store closes.

## Private API

Only GET `/v0/management/plugins/token-usage/status`, `/summary`, and `/models`. Every route has empty `Menu`; no resources/UI/write routes. CPA supplies authentication. Status returns no raw observations in production and never exposes filesystem paths, keys, account identifiers, bodies, headers, or SQL errors.

Summary/models require `from` and `to` in RFC3339 form with explicit timezone; timestamps normalize to UTC. The interval is exact `[from,to)` using reported request time (or flagged receipt fallback). `from < to`, maximum span is configured raw retention, and future upper bounds are rejected. Optional exact `provider` and `model` filters only. Models additionally supports `limit` (default 100, maximum 1000) and `offset` (default 0, maximum 100000); deterministic provider/model ordering. Duplicate, unknown, empty, or malformed parameters are rejected.

HTTP responses: 400 invalid query/config-independent input; 416 range preceding queryable raw coverage; 503 actual read/storage unavailability or a closed collector; 504 query timeout. Admission faults, including disk budget, failed writes/diagnostic saves and maintenance, do not automatically block committed-history reads. A readable store still returns 200 summary/models with `collection` degradation annotations. A valid covered empty query returns zero strings/empty rows, not an error. An actual read outage or expired interval must not return fake zeros.

Statistics envelope includes `api_schema: 1`, `source: "cpa_reported"`, `interval: {from,to}`, `coverage`, `collection`, `upstream_completeness: "unknown"`, and aggregation semantics. All token/event counters are **decimal JSON strings**, including values above JavaScript's safe integer limit. Raw counters use snake_case: `input_tokens`, `output_tokens`, `total_tokens`, `reasoning_tokens`, `cached_tokens`, `cache_read_tokens`, `cache_creation_tokens`. Auxiliary counters overlap; they are not additive buckets.

`/summary` adds `totals`, whose fields include `observed_events`, `successful_events`, `failed_events`, `anomalous_events`, and `reported_tokens` containing those seven counters. `/models` adds `models` (rows with provider/model and the same totals), `limit`, `offset`, `has_more`. Rows expose a bounded executor provenance list and explicit truncation indicator; totals combine executors within each provider/model. Alias and executor provenance are stored per event; alias/account filters and drilldown are not supported.

`/status` includes version, state, `storage: "sqlite"`, coverage, collection diagnostics, configured resource limits, upstream limitations and unknown completeness. `collection` includes lifetime best-effort diagnostics (observed/admitted/rejected/dropped/committed/storage errors/retries), queue depth, disk usage, last persistence, local degradation reasons, and previous unclean-run count. `active_faults` lists every current cause in stable priority order; `reason` is its first entry or an empty string. `last_failure_reason` preserves the latest failure reason within the current collector; persisted `storage_errors` preserves lifetime failure history even after recovery/restart. Lifetime errors/drops/rejections and unclean runs keep `degraded` true after active causes clear. Diagnostics persist at writer flush and clean shutdown; crash-surviving diagnostics are best effort, not an exact loss estimate. Committed event count updates transactionally with inserts. Statistics are eventually consistent with admission.

Active fault/recovery semantics:

| Cause | Matching recovery |
|---|---|
| `disk_unavailable` / `disk_budget` | A successful footprint/ownership check within budget clears only the disk cause. Checks run on flush ticks and after successful event writes, write probes and maintenance. |
| `write_unavailable` | A successful non-event write probe clears only the write cause. Probe cycles are separated by at least five wall-clock seconds and scheduled on flush ticks, never by admitting a real callback into a known-broken writer. Disk faults suppress probes. |
| `diagnostics_unavailable` | A successful diagnostic save clears only this cause. Saves continue on flush/maintenance and final drain. |
| `maintenance_unavailable` | A successful maintenance step clears only this cause. Failed steps retry at the configured maintenance interval, not the prompt backlog cadence. |
| `event_id_exhausted` | Terminal for that collector. The sequence saturates without wrap/reuse, including concurrent callbacks. Only a new collector/native lifecycle can obtain a new run identity. |

Any active cause stops new admission. Already-queued events are not repeatedly retried once the writer is known broken; they become visible storage drops. A successful disk stat or metadata save cannot clear a write/maintenance/identity fault. Event writes, diagnostic saves and write probes retain the existing bounded three-attempt BUSY/LOCKED policy (two-second contexts; 25/50ms waits); nonretryable failures stop immediately. Maintenance instead has one 250ms-deadline attempt per step. This is deliberately conservative: even a transient maintenance failure pauses admission until the next successful maintenance attempt (normally after the default one-minute interval; up to the configured one-hour maximum). Callbacks during that pause are counted as storage drops, not buffered for replay. Choose the maintenance interval with this recovery tradeoff in mind.

The probe performs a disposable model/event insertion through the event-insert path, rolls it back to a savepoint, and commits an unchanged metadata write. It retains no synthetic event/model and does not advance counters, coverage or last-persistence timestamps. It tests insertion and commit availability, not whether every later larger batch will fit; failures are still possible after recovery. Probes do not replay already dropped events.

Queryable coverage begins at the earliest locally observed request/initial collection start, clipped by the monotonically advancing retention floor. It does **not** imply uninterrupted host receipt. Empty periods, downtime, upstream omissions and unclean runs cannot be reconciled into exact missing-event counts. Increasing retention after restart cannot resurrect previously expired coverage.

## Accounting, lifecycle and resource policy

- Only the narrow usage fields are decoded; unknown wire fields are ignored. Missing provider/model, oversized metadata, invalid/negative/fractional/out-of-range counters and invalid outcomes are rejected visibly. Missing/invalid requested time falls back to local receipt time and is flagged; missing token fields and generation metadata are flagged, without inventing original provider field presence. Missing executor provenance is marked unknown. Excessively future or out-of-retention event times are rejected.
- Every admitted callback receives a local ID. Distinct equal-looking callbacks remain distinct; the same local ID is preserved through bounded write retries. Failed events retain their nonzero counters. Event inserts are idempotent.
- Callback work is narrow decoding/validation plus a nonblocking bounded enqueue, never a database query or provider/host call. Queue full drops the new event visibly. Writer/storage degradation stops new admission with visible drops; callbacks do not wait for queries or SQLite locks.
- One batch writer owns mutations. Queries use separate read connections and bounded context deadlines. Sums are computed with Go arbitrary-precision integers while streaming signed-64-bit raw rows; SQLite `SUM` is not used.
- The store uses one held advisory lock per database, SQLite WAL and `synchronous=FULL`, versioned fail-closed schema, and clean/unclean run markers. Unknown/nonempty unversioned databases, newer versions and corrupt databases fail closed; nothing is silently recreated.
- Retention removes only expired raw rows, at most 512 per step plus 512 unused model keys, under a 250ms context deadline. A full chunk conservatively schedules another step after 10ms, yielding to event handling/flushes, shutdown and independent WAL readers; it does not wait a full maintenance interval for every chunk. Non-full chunks and failed steps return to the configured interval. `retention_cleanup_pending` is the last successful step's continuation hint, not an exact backlog count; false is not a completeness claim before the first success or during an active maintenance fault. This removes the fixed 512-per-interval ceiling, not all possible throughput limits. Each committed cleanup advances the durable monotonic floor; fresh promised rows are never selected. Incremental vacuum/checkpoint remain opportunistic; active readers can defer WAL truncation.
- The disk budget includes database, WAL, SHM, rollback journal and lock files (diagnostics/run metadata are in the database). It is a monitored budget, not a filesystem quota: a transaction/checkpoint may temporarily overshoot. Budget exhaustion stops new admission, not readable committed-history queries; newer promised history is not deleted to recover space. Deleting rows does not necessarily shrink the file immediately.
- Ordinary builds do not retain/expose fixture observations. `nativefixture` builds retain at most 64 sanitized observations solely for the upstream regression harness; they must never be packaged as production artifacts.

## Driver and validation

Selected `github.com/mattn/go-sqlite3 v1.14.52`, official release published 2026-09-05: https://github.com/mattn/go-sqlite3/releases/tag/v1.14.52. It is maintained, bundles SQLite, exposes busy/locked error codes, supports URI connection settings and context cancellation, and adds no platform claim beyond the Linux amd64 CGO native build this project actually validates. CGO is already required by the native plugin ABI. Preserve the driver's MIT notices in redistribution. SQLite WAL requirements: https://sqlite.org/wal.html; synchronous semantics: https://sqlite.org/pragma.html#pragma_synchronous.

## Completed runtime validation

Passed with Go 1.27.1 / GCC 15.2.0 on Linux amd64:

- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go test -race -tags nativefixture -count=1 ./...`
- `go vet ./...` and `go vet -tags nativefixture ./...`
- Production and `nativefixture` `CGO_ENABLED=1 go build -trimpath -buildmode=c-shared` artifacts, with SQLite included.
- A production-library native C ABI probe sent two callbacks containing input `9007199254740993`, observed exact persisted summary `18014398509481986`, shut down/reinitialized the native plugin, and observed the same history with zero previous unclean runs. After the runtime review fixes, `scripts/native/abi_probe.py` was updated on disk and passed for both production and `nativefixture` libraries. It requires stopped status 200, exact late observed/stopped increments for normal and oversized callbacks, unchanged admissions/committed counts, summary/models 503 after close, and same-config reopen from the durable diagnostic snapshot. Direct ABI probes bypass HTTP authentication and are not a substitute for the separately passed CPA HTTP integration test.

Tests cover signed-64-bit raw maximums and aggregates above signed-64-bit, provider/model separation, aliases/executor preservation, failed nonzero tokens, missing account, sensitive canaries, invalid numeric values, timestamp fallback, metadata limits, parameterized filters, exact half-open ranges, bounded pagination, cardinality rejection/release, idempotent inserts, transaction rollback, bounded retention cleanup, non-resurrection of expired coverage, startup corruption/newer/unversioned/read-only/unsafe-permission/symlink/hardlink rejection, concurrent ownership refusal, real SQLite BUSY and SQLITE_FULL, canceled queries, queue overload, injected storage degradation/recovery, stable IDs through an ambiguous-commit retry, diagnostic saturation/persistence, reader/writer shutdown coordination and process-exit recovery of committed WAL data plus unclean-run detection.

`SQLITE_FULL` was induced using SQLite's page limit; the host filesystem was not filled. Abrupt process exit was tested, not physical power loss. The disk limit is a monitored budget rather than a hard filesystem quota. Native filesystem tests ran on the available local Linux filesystem; network mounts and every possible unsupported filesystem were not tested. Only known remote filesystem types are detected automatically; operators must still supply an appropriate local volume.

Final verification completed on 2026-09-09 after the runtime corrections and configured `opus-xhigh` reverification. Both reviewers confirmed their actionable findings fixed; no new correctness regression was found. `make ci package-current checksums verify-release VERSION=0.1.0` passed, including both race variants and 11 Python release-contract tests. `make smoke` passed both direct ABI probes, all 30 retained Phase A executions, and 38 **real CPA production executor → persistence → authenticated query → restart** executions (18 committed observations per 19 executions for each statistics setting, including the known upstream no-event case). The actual pinned CPA registry/checksum/ZIP installer rehearsal passed without network transport, and the packaged production library matched the smoke-tested binary. Existing upstream omissions and the documented conservative maintenance-recovery policy remain unchanged; simplifier cleanups were preserved. See `verification.md` for final evidence, review dispositions and untested boundaries.
