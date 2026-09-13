# Operating Token Usage

This guide describes the **Token Usage 0.1.1**, not the unchanged published v0.1.0 behavior. It is operator guidance, not authorization to publish or change a running CPA deployment. For exact keys, bounds and API fields, use the [runtime contract](runtime-contract.md); for native compatibility and measured upstream omissions, use [upstream compatibility](upstream-compatibility.md).

## Import source and native compatibility

The candidate targets **Linux amd64**, native ABI 1 / RPC schema 6 and **CPA v7.2.155, commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`**. Actual installation/default-storage/restart and native-page browser integration passed in official image **`eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b`**, Linux amd64, Debian bookworm/glibc **2.36**, cwd `/CLIProxyAPI`. The audited console is official Management Center v1.22.15, commit `ed5f1c48e11ba7335f1e8f676f228c280196af85`; browser integration used its codec with a synthetic remembered session and real CPA JSON, **not full console login/sidebar-navigation testing**. Your deployed CPA, console and reverse-proxy policy are unknown. Native-library compatibility is artifact-specific; this glibc result does not imply Alpine/musl support. Final post-review local acceptance and exact artifact/notice checks passed; [candidate verification](verification-sidebar.md) records the local `dist/0.1.1/` deliverable and remaining compatibility limits. These are local prepublication results; no operator installation or deployment is claimed.

Choose the stable/latest source or the version-specific v0.1.1 source:

Use `https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json` as the store source. Keep the identical preview alias if already installed from it. Install or update Token Usage through CPA, preserving its database path and other configured options. See the [quick start](../README.md) and [release guide](../../../docs/releases.md).


For manual installation, download the ZIP, `checksums.txt`, and `registry.json` together from the trusted release, verify `sha256sum -c checksums.txt`, stage/extract, and stop CPA before changing a native library. The ZIP includes the library and project/dependency license notices; retain them. CPA store installation creates a versioned library under the platform directory, unlike the manual unversioned example below. Do not let stale versioned libraries shadow a manually copied binary. Publishing assets does not deploy them or change any operator configuration.

## Storage layout and persistent volumes

Use a **local filesystem supporting SQLite WAL and advisory locking** with one active CPA/plugin owner per database. The plugin holds an exclusive advisory lock, uses WAL with `synchronous=FULL`, and refuses known network filesystems, unsafe ownership/modes, symlinks/hardlinks, corrupt/unversioned nonempty databases and newer schemas. Automatic remote-filesystem detection is not exhaustive: the operator must still choose a suitable local filesystem. Do not use NFS, SMB, a shared multi-replica volume, or a database shared with another process writing independently.

The standard existing-volume layout is:

- Replaceable native library: `/CLIProxyAPI/plugins/linux/amd64/token-usage.so` (the store uses a versioned filename).
- Persistent private data directory: `/CLIProxyAPI/plugins/data/token-usage/`.
- CPA auth files: the existing auth directory, **never** a data-path discovery source or the plug-in data directory.

With CPA's working directory `/CLIProxyAPI` and the existing **`cliproxy-plugins` mount at `/CLIProxyAPI/plugins`**, omit `database-path`. The candidate creates `/CLIProxyAPI/plugins/data/token-usage/usage.sqlite`. **Keep Compose and existing mounts unchanged; no additional volume/mount is required.** This is a dedicated private directory inside the existing plugins volume, not a new volume alongside it.

The general default is `<captured CPA cwd>/plugins/data/token-usage/usage.sqlite`, captured once at native `New()`. Later cwd changes, CPA `plugins.dir`, store URLs and auth directories do not influence it. For a nonstandard cwd/plugin directory, explicitly configure a clean absolute path inside the intended existing persistent storage. Check the actual layout rather than assuming every deployment uses the audited image's working directory.

The final directory must be owned by the CPA UID and mode `0700`. The database, `-wal`, `-shm`, `-journal` and `.lock` companions must be owned by that UID, mode `0600`, and remain together. A missing directory can be created if its parent is writable. Existing unsafe modes/ownership, symlinks/hardlinks and incompatible databases are rejected, not recursively chmodded or recreated. The shared plugins-volume root remains unchanged. Never recursively change ownership over shared plugins/auth/config or a production root; verify the actual runtime UID before any separately chosen repair.

**Preserve any existing explicit `database-path`.** The candidate neither moves old history into its default nor searches for prior databases. Removing an override selects a different location on the next native instance; it does not migrate/merge the old history. An existing v0.1.0 path such as `/CLIProxyAPI/plugin-data/token-usage/usage.sqlite` remains authoritative when explicitly configured. Do not remove its existing mount or path just to adopt this default. A deliberate migration would need an independently planned stopped backup/restore, not an automatic upgrade step.

Confirm the backing volume is truly local; Docker's `local` driver can still mount network storage. Mounting only `.sqlite` is wrong because WAL/SHM/lock need the same durable parent. Container replacement, redeployment and image updates must preserve the existing plugins volume/data directory, including on rollback. Native-binary cleanup must not delete `plugins/data/`.

No health-plugin config/state, reference repository, existing Compose configuration or production credential needs changing. Joint deployment with unrelated plug-ins is not established by synthetic isolated tests.

## Collection and retention policy

The native callback decodes only a bounded whitelist and attempts a **nonblocking** enqueue. It does not query SQLite, call providers, or look up auth. Each admission receives a local ID; retries reuse that ID. Equal-looking separate callbacks are not deduplicated. One writer batches commits; read queries use separate connections with deadlines.

There are two independent loss windows:

1. CPA can lose/omit events before or while delivering to the native plugin. There is no durable replay/acknowledgement. CPA shutdown ordering does not ensure its usage manager reaches the plugin before native shutdown.
2. An admitted event can remain in the plugin RAM queue/batch before commit. A crash can lose it. A normal quiesce/shutdown closes admission, drains/joins the writer and closes storage, but cannot recover host events it never received or guarantee persistence during an unrecoverable disk failure.

`flush-interval`/`batch-size` trade callback-to-query latency and write overhead; they do not make asynchronous admission durable. `synchronous=FULL` protects committed SQLite transactions under the filesystem/storage guarantees; it is not proof against every hardware or filesystem failure. Diagnostics persist on writer flush/clean shutdown and are lifetime **best effort**. A stopped collector remains available for status and counts late callbacks as observed/`dropped_stopped`, but delivery racing or following the final diagnostic sample is RAM-only best effort: those increments may disappear on same-config reopen/unload. Reconfigure resumes durable counters; no worker is kept alive or recreated after final shutdown to save late counts. An unclean-run marker signals possible loss, not the number of missing events.

Raw retention defaults to **720h (30 days)**. Each cleanup step removes at most 512 expired raw rows and 512 unused model keys under a 250ms context deadline, then yields. A full chunk schedules another step after 10ms rather than waiting another `maintenance-interval`; fresh batches flush between cleanup steps and queries use separate WAL readers. The configured interval applies when no more work is indicated, and after a failed step. `collection.retention_cleanup_pending` reports the last successful step's conservative continuation hint, not an exact backlog count (or proof of completeness when cleanup fails). This avoids a fixed 512-per-minute ceiling but is not a guaranteed cleanup throughput. There are no rollups or all-time totals. Query coverage clips initial/observed history by a monotonically advancing retention floor. Increasing retention after restart cannot restore deleted rows. Coverage is an eligible query interval, not proof of uninterrupted receipt: downtime, upstream omissions and empty periods cannot be reconciled automatically.

`max-disk-bytes` counts database/WAL/SHM/rollback-journal/lock bytes; diagnostics live in SQLite. It is a **monitored budget, not a hard filesystem quota**. A transaction/checkpoint can temporarily overshoot. When exhausted or when storage is unavailable, admission stops with visible drops; newer promised history is not silently deleted. Deleting rows need not shrink SQLite's allocated file immediately. Provision real free space beyond the configured budget and monitor the backing volume. Do not delete WAL/SHM or truncate files to reclaim space. Planned offline compaction using an appropriate SQLite procedure needs a backup, extra free space and an outage; it is not automatic.

## Sidebar session and range troubleshooting

Once the candidate is installed/enabled, click **Token Usage** in the console sidebar. A compatible existing remembered login automatically supplies the three private GETs; manual API calls and a second credential form are not required. Use **Refresh** for an explicit reread—there is no periodic polling. Editing any filter clears old totals; Apply filters or Refresh loads the current selection. Model pages contain 25 rows, with combined-executor totals and raw/provenance details.

| Page state | What to do |
|---|---|
| No compatible remembered session | Sign in to the console using the exact origin and API base shown, enable **Remember password**, allow localStorage, then refresh. A session only in the parent page's memory is unavailable to this sidebar. |
| Logged out, remember-off or invalid modern storage | Sign in again through the console. Modern `cli-proxy-auth` presence is authoritative and intentionally blocks stale legacy credentials; do not delete or forge storage entries as a workaround. |
| Key cannot be used in an HTTP header | Internal spaces are supported. Leading/trailing whitespace, CR/LF/NUL and characters outside the browser's header representation are rejected before requests. Use the compatible console login rather than manually editing or re-encoding stored keys. |
| Wrong origin/port/scheme/prefix | Use the same effective origin and reverse-proxy prefix for console login and iframe. Default-port URL normalization and terminal `/v0/management`/trailing slashes are supported; HTTP/HTTPS, aliases or unrelated prefixes are not interchangeable. |
| Console session rejected (401/403) | Requests stop and private values clear. Correct the console login/management policy before retrying; repeated wrong keys can trigger CPA's IP ban. |
| Redirect, HTML login response or incompatible response | Check console/plug-in compatibility and proxy routing. The page refuses redirects and non-JSON/wrong-schema data; do not disable the CSP or pass credentials in URLs to force it through. |
| Session changes while loading | Pending requests are aborted, stale responses discarded and private values/filters cleared. Finish the remembered console sign-in and explicitly refresh. |
| No elapsed coverage interval | A fresh collector has no nonempty query interval yet. Wait for elapsed coverage and refresh; this is not a fabricated zero total. |
| Empty retained result | The covered query matched no observed events. It is valid empty local history, not proof of zero consumption. |
| Storage unavailable | No history/counters are displayed. Correct filesystem/configuration problems and use CPA's reconfigure/restart flow; Refresh does not initialize storage. |
| Stopped collection | Last persistence/diagnostics remain visible; statistics cannot be read until the host opens the collector again. |
| Degraded collection | Readable committed totals remain visible with a warning. Review active faults and best-effort diagnostics; some events may be missing. |

Presets end at server `coverage.to` and visibly disclose clipping to local history. When a preset reaches the **moving retention boundary**, it proactively starts exactly **60 seconds after the reported boundary**. The page shows the excluded `[reported boundary, selected start)` minute before fetching, explains that the margin avoids a moving boundary, and labels the result **not the full retained window**. Excluded usage is not estimated. A preset already inside coverage has no extra minute removed; a young installation's stable history start is preserved exactly.

Custom UTC timestamps use a final `Z`, seconds and up to nine fractional digits; their half-open `[from,to)` range must fit reported coverage. Empty fields suggest the selected range or a suitable range inside coverage, disclosing any retention margin. **Typed dates are not automatically changed**, including dates inside that suggested minute. Invalid or expired dates remain entered with guidance. If a retained interval is too short for the margin, choose explicit custom dates rather than expecting made-up zeros. Filters are exact and case-sensitive.

If a **416 moving-retention** response still occurs, totals clear and the new coverage is shown. **Load range after retention edge** remains an explicit fallback: when a nonempty interval remains, it chooses a custom start at the returned `coverage.from + 60 seconds`, preserves the selected end and discloses the exclusion before clicking/fetching. There is no automatic retry after 416. You may instead enter another custom range or choose a shorter preset; a sufficiently delayed choice can still encounter another 416.

## Health checks and API failures

The sidebar exposes authenticated status for normal use. Optional external monitoring can query `/status` using CPA's existing management authentication; it is not required to open the page. Inspect `state` and the collection fields `degraded`, `reason`, `active_faults`, `last_failure_reason`, `last_persisted_at`, `queue_depth`, `disk_bytes`, `retention_cleanup_pending`, `previous_unclean_runs` and `diagnostics`. `active_faults` shows simultaneous current causes; `reason` is its first entry in stable priority order, not necessarily the last failure. `last_failure_reason` is current-collector history, whereas the error counters survive successful saves/restarts.

Disk checks clear only `disk_unavailable`/`disk_budget`; event writer, diagnostic-save and maintenance causes clear only on a successful matching operation. A known-broken writer stays closed to new observations while a non-event probe tests an insertion and commit at most once per five-second recovery cycle (scheduled on flush ticks, with bounded BUSY/LOCKED retries). The probe retains no event or token count. Disk faults suppress it. It cannot guarantee that the next larger batch fits available space, and dropped observations are not replayed. Diagnostic saves continue on ticks; failed maintenance retries at `maintenance-interval`. `event_id_exhausted` never clears within the collector. Consult `active_faults`, not a successful disk stat alone, before concluding admission has recovered.

Relevant decimal-string diagnostics include `observed_events`, `admitted_events`, `committed_events`, `rejected_events`, `dropped_queue`, `dropped_storage`, `dropped_stopped`, `rejected_retention`, `rejected_cardinality`, `storage_errors`, and `write_retries`. No increase in committed events during an idle period is normal. Upstream completeness is unknown regardless of these values. Lifetime storage-error/drop/rejection counters can keep local status degraded after a transient condition recovers; do not infer a currently broken writer from that flag alone.

Admission and read availability are independent. Disk-budget, write, diagnostic or maintenance faults stop new admission but do not by themselves hide readable committed history: summary/models return 200 with degradation annotations when their query-only connection succeeds. A genuine read error or a closed collector still returns 503, not zero totals. Stopped status remains readable, but it is not an open database for statistics.

| Result | Meaning and operator response |
|---|---|
| 200, running | Local collection is available, not a completeness guarantee. Check last persistence after known traffic. |
| 200, stopped status | Admission and storage are closed; stopped diagnostics remain visible, including best-effort late drops. Summary/models return 503. |
| CPA 401/403 | Missing/invalid management key, remote management restriction, or CPA's failed-auth IP ban. Correct authentication; avoid repeated bad probes. No usage is returned. |
| Public sidebar 200 / unknown route 404 | The exact GET `/v0/resource/plugins/token-usage/status` returns fixed data-free HTML, even when storage is unavailable. Other resource spellings do not expose private JSON. A 404 for the expected page can mean wrong path/prefix, old binary, or an unavailable/unregistered plug-in; check load errors and selected version. |
| 400 `invalid_query` | Missing/duplicate/unknown/empty/malformed parameters, reversed/future interval, excessive span or pagination limit. Use status coverage and RFC3339 with timezone. |
| 416 `outside_retained_coverage` | Query begins before eligible raw coverage. Narrow it to the returned coverage; do not interpret this as zero usage. |
| 503 `collection_unavailable` / `storage_unavailable` | No configured collector, closed statistics storage, or an actual read outage. Admission degradation alone does not force 503 when committed history remains readable. Inspect status and filesystem ownership/capacity/lock health; never substitute fake zero totals. |
| 504 `query_timeout` | Query exceeded its bounded deadline. Narrow interval/filter/page rather than removing resource controls. |
| Valid config, initial storage-open failure | Registration metadata/sidebar/config fields stay available, with sanitized status 503 `storage_unavailable` / `storage_initialization_failed`; no coverage or counters are fabricated. Correct the path/ownership/capacity/lock/schema problem and reconfigure/restart. Corrected configuration can retry until the first successful open. |
| Native configuration/load failure | Unknown key, invalid bounds, malformed/non-mapping store metadata, unsupported protocol, missing captured cwd without explicit path, or incompatible library still fail. After a successful open, changed storage settings require native restart and failed same-config reopen returns a sanitized error. |

Token/model history and errors are private. CPA authenticates the management routes; every private GET has an empty `Menu`. Only the dedicated public resource carries **Token Usage** as its menu and serves fixed bytes with no operational values. The page uses hashed-script/style CSP, same-origin connections/framing, no-store, nosniff and no-referrer; it renders untrusted strings as text. Console localStorage obfuscation is reversible, and other installed same-origin scripts share the console's credential trust boundary—it is not isolation from an untrusted plug-in. Do not broaden CORS/framing, expose management broadly, embed keys in URLs/messages/logs, or treat reverse-proxy headers as new authorization. The plug-in excludes sensitive payload fields from storage/output; **CPA itself may log upstream error bodies**. Secure CPA logs separately.

## Backup

Prefer a supported SQLite backup mechanism or a **cleanly stopped** copy. **Never copy only the main file of a live WAL database**: committed records can still live in `-wal`. Store backups privately/encrypted as appropriate; token/model history is operationally sensitive even without credentials.

One online method, using the SQLite CLI as the CPA UID on the same local machine/volume:

```sh
# Explicit operator-selected paths; run only after checking them.
DB=/CLIProxyAPI/plugins/data/token-usage/usage.sqlite # Or your preserved explicit override.
umask 077
BACKUP_DIR=$(mktemp -d /private/local/backup-parent/token-usage.XXXXXXXX)
sqlite3 "$DB" ".backup '$BACKUP_DIR/usage.sqlite'"
sqlite3 "$BACKUP_DIR/usage.sqlite" 'PRAGMA integrity_check;'
```

`/private/local/backup-parent` is a placeholder for an existing private backup parent; do not run it literally without provisioning it. Confirm `.backup` succeeds and integrity output is exactly `ok`. Record plugin/CPA versions, configuration (without secrets), backup time and checksum with your operational backup records. The SQLite backup API produces a consistent snapshot while collection continues, but events committed after its snapshot are not in that backup. It may capture an open-run marker: restoring that snapshot can correctly report an unclean run.

For an offline copy, stop CPA normally, confirm all native workers/processes have exited, then copy the **entire** dedicated data directory including any remaining companions. Do not manipulate advisory lock files while a process is running. Keep a pristine backup before upgrades/repairs. Test restoration on a separate disposable instance/data directory, never concurrently against the original database. Backups and restores were not executed against operator data by this project.

## Restore

1. Stop CPA and verify no owner is active. Preserve the current data directory separately for rollback; do not overwrite your only recovery copy.
2. Restore a validated backup into a fresh private directory. For a SQLite backup-API output, restore its standalone main database without unrelated/stale WAL/SHM files from another database. For a quiesced directory backup, restore that directory consistently. Never mix generations.
3. Set private ownership/modes for the actual CPA UID, verify integrity offline, and keep the configured absolute path consistent. Do not open an older binary against a newer schema unless its compatibility is established.
4. Start CPA and inspect authenticated status, retained coverage and sample summaries before accepting traffic. A restored backup loses post-snapshot history; an open-run marker can report potential loss. Do not label it complete accounting.

## Update, rollback, and uninstall

**Update:** build/test or obtain a trusted compatible artifact, verify its checksum/notices, back up the DB/config, then stop CPA. Stage the new library and replace only Token Usage. Ensure only one selected version exists across `plugins/` and `plugins/linux/amd64/`; a stale versioned library can shadow the unversioned one. Keep persistent data mounted and unchanged. Apply intended config edits by merging, start CPA, and verify version, schema/storage health, retained totals and one new observation. Do not hot-overwrite a loaded library. All collector/storage setting changes require a native restart.

**Rollback:** stop CPA, preserve the failed/new state, restore the previous library and compatible configuration. The candidate retains database schema version 1, but future migration backward compatibility is not promised. Published v0.1.0 needs an explicit database path, rejects CPA `store` metadata, and has no sidebar; do not assume candidate-generated configuration can be reused unchanged with that binary. Consult its tagged instructions before choosing that rollback. If the previous library cannot read the schema, restore the matching pre-update backup rather than forcing schema numbers or deleting metadata. That forfeits post-backup events. Verify history before resuming traffic.

**Disable/uninstall:** stop CPA cleanly, remove/disable only `plugins.configs.token-usage`, remove all Token Usage library versions from scanned directories, and retain unrelated configuration. Leave `plugins/data/token-usage/`, any explicit data location, the shared plugins volume and backups intact by default. Confirm restart no longer loads Token Usage. Erasing the dedicated Token Usage directory requires a separate intentional decision and no active owner. **Never delete the shared `cliproxy-plugins` volume to uninstall this plug-in.**
