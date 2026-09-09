# Operating Token Usage

This guide describes operator-controlled installation and maintenance. It does not authorize or imply changes to a running CPA deployment. For exact keys, bounds and API fields, use the [runtime contract](runtime-contract.md); for native compatibility and measured upstream omissions, use [upstream compatibility](upstream-compatibility.md).

## Import source and native compatibility

Version 0.1.0 is for **Linux amd64, glibc**, native ABI 1 / RPC schema 6, tested with **CPA v7.2.155, commit `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`**. Other CPA versions and Alpine/musl are unvalidated; use Ubuntu 24.04 or a compatible newer glibc runtime for the hosted binary. Review the release workflow's `readelf` output if evaluating older libc compatibility. This project has not inspected or changed your deployed CPA version.

After the release is published, append **one** registry URL under the existing CPA `plugins.store-sources` list:

- Stable/latest: `https://raw.githubusercontent.com/NoorChasib/cpa-plugin-token-usage/main/registry.json`.
- Fixed 0.1.0/digest: `https://github.com/NoorChasib/cpa-plugin-token-usage/releases/download/v0.1.0/registry.json`.

Keep existing store sources and `plugins.configs` entries. CPA's built-in official registry stays included automatically. Adding a source does not install/enable the plugin or provision its database. Refresh the store, select Token Usage, then finish the explicit private storage/configuration from the [README](../README.md#install-and-configure) and follow CPA's native restart procedure. The stable GitHub-release source follows latest; its displayed version is not a tag lock. Prefer the direct source for controlled version pinning. Neither source URL is a claim of availability before successful publication.

For manual installation, download the ZIP, `checksums.txt`, and `registry.json` together from the trusted release, verify `sha256sum -c checksums.txt`, stage/extract, and stop CPA before changing a native library. The ZIP includes the library and project/dependency license notices; retain them. CPA store installation creates a versioned library under the platform directory, unlike the manual unversioned example below. Do not let stale versioned libraries shadow a manually copied binary. Publishing assets does not deploy them or change any operator configuration.

## Storage layout and persistent volumes

Use a **local filesystem supporting SQLite WAL and advisory locking** with one active CPA/plugin owner per database. The plugin holds an exclusive advisory lock, uses WAL with `synchronous=FULL`, and refuses known network filesystems, unsafe ownership/modes, symlinks/hardlinks, corrupt/unversioned nonempty databases and newer schemas. Automatic remote-filesystem detection is not exhaustive: the operator must still choose a suitable local filesystem. Do not use NFS, SMB, a shared multi-replica volume, or a database shared with another process writing independently.

Keep these separate:

- Replaceable native library: `/CLIProxyAPI/plugins/linux/amd64/token-usage.so`.
- Persistent private data directory: `/CLIProxyAPI/plugin-data/token-usage/`.
- CPA auth files: the existing auth directory, **never** the plugin data directory.

The final data directory must be owned by the CPA UID and mode `0700`. The database, `-wal`, `-shm`, and `.lock` companions must be owned by that UID, mode `0600`, and remain together on the persistent mount. A missing dedicated directory can be created by the plugin if its parent is writable. Existing unsafe files/directories are rejected, not chmodded or recreated automatically. Verify the actual runtime UID before creating/chowning a directory; do not blindly copy a sample UID. Never change ownership recursively over a shared auth/config or production root.

For Docker Compose/Coolify, **merge** a dedicated local volume into the existing CPA service instead of replacing its existing mounts/configuration:

```yaml
services:
  cli-proxy-api: # Use the actual existing service name.
    volumes:
      # Keep existing config, auth and plugin mounts unchanged.
      - token_usage_data:/CLIProxyAPI/plugin-data
volumes:
  token_usage_data:
    driver: local
```

Use `database-path: /CLIProxyAPI/plugin-data/token-usage/usage.sqlite` inside CPA, not the host-side volume location. Confirm the volume is truly local; a Docker `local` driver can still be configured to mount network storage. Provision its private subdirectory for the CPA UID. Mounting only the `.sqlite` file is wrong: WAL/SHM/lock need the same durable parent. Container replacement, application redeployment and image updates must retain this volume. A rollback to an old image must not point to an ephemeral path.

No health-plugin config or state needs changing. Distinct IDs/data paths allow configuration alongside unrelated plugins, but joint deployment with an existing health plugin was not exercised by the synthetic tests.

## Collection and retention policy

The native callback decodes only a bounded whitelist and attempts a **nonblocking** enqueue. It does not query SQLite, call providers, or look up auth. Each admission receives a local ID; retries reuse that ID. Equal-looking separate callbacks are not deduplicated. One writer batches commits; read queries use separate connections with deadlines.

There are two independent loss windows:

1. CPA can lose/omit events before or while delivering to the native plugin. There is no durable replay/acknowledgement. CPA shutdown ordering does not ensure its usage manager reaches the plugin before native shutdown.
2. An admitted event can remain in the plugin RAM queue/batch before commit. A crash can lose it. A normal quiesce/shutdown closes admission, drains/joins the writer and closes storage, but cannot recover host events it never received or guarantee persistence during an unrecoverable disk failure.

`flush-interval`/`batch-size` trade callback-to-query latency and write overhead; they do not make asynchronous admission durable. `synchronous=FULL` protects committed SQLite transactions under the filesystem/storage guarantees; it is not proof against every hardware or filesystem failure. Diagnostics persist on writer flush/clean shutdown and are lifetime **best effort**. A stopped collector remains available for status and counts late callbacks as observed/`dropped_stopped`, but delivery racing or following the final diagnostic sample is RAM-only best effort: those increments may disappear on same-config reopen/unload. Reconfigure resumes durable counters; no worker is kept alive or recreated after final shutdown to save late counts. An unclean-run marker signals possible loss, not the number of missing events.

Raw retention defaults to **720h (30 days)**. Each cleanup step removes at most 512 expired raw rows and 512 unused model keys under a 250ms context deadline, then yields. A full chunk schedules another step after 10ms rather than waiting another `maintenance-interval`; fresh batches flush between cleanup steps and queries use separate WAL readers. The configured interval applies when no more work is indicated, and after a failed step. `collection.retention_cleanup_pending` reports the last successful step's conservative continuation hint, not an exact backlog count (or proof of completeness when cleanup fails). This avoids a fixed 512-per-minute ceiling but is not a guaranteed cleanup throughput. There are no rollups or all-time totals. Query coverage clips initial/observed history by a monotonically advancing retention floor. Increasing retention after restart cannot restore deleted rows. Coverage is an eligible query interval, not proof of uninterrupted receipt: downtime, upstream omissions and empty periods cannot be reconciled automatically.

`max-disk-bytes` counts database/WAL/SHM/rollback-journal/lock bytes; diagnostics live in SQLite. It is a **monitored budget, not a hard filesystem quota**. A transaction/checkpoint can temporarily overshoot. When exhausted or when storage is unavailable, admission stops with visible drops; newer promised history is not silently deleted. Deleting rows need not shrink SQLite's allocated file immediately. Provision real free space beyond the configured budget and monitor the backing volume. Do not delete WAL/SHM or truncate files to reclaim space. Planned offline compaction using an appropriate SQLite procedure needs a backup, extra free space and an outage; it is not automatic.

## Health checks and API failures

Poll authenticated `/status`; inspect `state` and the collection fields `degraded`, `reason`, `active_faults`, `last_failure_reason`, `last_persisted_at`, `queue_depth`, `disk_bytes`, `retention_cleanup_pending`, `previous_unclean_runs` and `diagnostics`. `active_faults` shows simultaneous current causes; `reason` is its first entry in stable priority order, not necessarily the last failure. `last_failure_reason` is current-collector history, whereas the error counters survive successful saves/restarts.

Disk checks clear only `disk_unavailable`/`disk_budget`; event writer, diagnostic-save and maintenance causes clear only on a successful matching operation. A known-broken writer stays closed to new observations while a non-event probe tests an insertion and commit at most once per five-second recovery cycle (scheduled on flush ticks, with bounded BUSY/LOCKED retries). The probe retains no event or token count. Disk faults suppress it. It cannot guarantee that the next larger batch fits available space, and dropped observations are not replayed. Diagnostic saves continue on ticks; failed maintenance retries at `maintenance-interval`. `event_id_exhausted` never clears within the collector. Consult `active_faults`, not a successful disk stat alone, before concluding admission has recovered.

Relevant decimal-string diagnostics include `observed_events`, `admitted_events`, `committed_events`, `rejected_events`, `dropped_queue`, `dropped_storage`, `dropped_stopped`, `rejected_retention`, `rejected_cardinality`, `storage_errors`, and `write_retries`. No increase in committed events during an idle period is normal. Upstream completeness is unknown regardless of these values. Lifetime storage-error/drop/rejection counters can keep local status degraded after a transient condition recovers; do not infer a currently broken writer from that flag alone.

Admission and read availability are independent. Disk-budget, write, diagnostic or maintenance faults stop new admission but do not by themselves hide readable committed history: summary/models return 200 with degradation annotations when their query-only connection succeeds. A genuine read error or a closed collector still returns 503, not zero totals. Stopped status remains readable, but it is not an open database for statistics.

| Result | Meaning and operator response |
|---|---|
| 200, running | Local collection is available, not a completeness guarantee. Check last persistence after known traffic. |
| 200, stopped status | Admission and storage are closed; stopped diagnostics remain visible, including best-effort late drops. Summary/models return 503. |
| CPA 401/403 | Missing/invalid management key, remote management restriction, or CPA's failed-auth IP ban. Correct authentication; avoid repeated bad probes. No usage is returned. |
| Plugin route 404 | Unknown route/method, plugin unavailable/not registered, or public resource probe. No public usage resource/UI exists. Check CPA load errors and binary selection. |
| 400 `invalid_query` | Missing/duplicate/unknown/empty/malformed parameters, reversed/future interval, excessive span or pagination limit. Use status coverage and RFC3339 with timezone. |
| 416 `outside_retained_coverage` | Query begins before eligible raw coverage. Narrow it to the returned coverage; do not interpret this as zero usage. |
| 503 `collection_unavailable` / `storage_unavailable` | No configured collector, closed statistics storage, or an actual read outage. Admission degradation alone does not force 503 when committed history remains readable. Inspect status and filesystem ownership/capacity/lock health; never substitute fake zero totals. |
| 504 `query_timeout` | Query exceeded its bounded deadline. Narrow interval/filter/page rather than removing resource controls. |
| Native configuration/load failure | Unknown key, invalid bounds, unsafe/unavailable path, another owner, incompatible/corrupt schema, or incompatible shared library. CPA receives a sanitized plugin error; use this checklist, not an expectation of a leaked SQL/path error. |

Token/model history and errors are private. CPA authenticates the management routes; every private GET has an empty `Menu` and no public resource is registered. Do not expose management broadly, embed keys in URLs, or treat reverse-proxy headers as a new authorization scheme. The plugin excludes sensitive payload fields from storage/output; **CPA itself may log upstream error bodies**. Upstream/error fixtures intentionally demonstrate such data can exist outside the plugin, so secure and rotate CPA logs separately.

## Backup

Prefer a supported SQLite backup mechanism or a **cleanly stopped** copy. **Never copy only the main file of a live WAL database**: committed records can still live in `-wal`. Store backups privately/encrypted as appropriate; token/model history is operationally sensitive even without credentials.

One online method, using the SQLite CLI as the CPA UID on the same local machine/volume:

```sh
# Explicit operator-selected paths; run only after checking them.
DB=/CLIProxyAPI/plugin-data/token-usage/usage.sqlite
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

**Rollback:** stop CPA, preserve the failed/new state, restore the previous library and compatible configuration. Database schema version 1 is the initial version; future migration backward compatibility is not promised. If the previous library cannot read the new schema, restore the matching pre-update database backup instead of forcing schema numbers or deleting metadata. That rollback deliberately forfeits events after the backup. Verify status/history before resuming traffic.

**Disable/uninstall:** stop CPA cleanly, remove/disable only `plugins.configs.token-usage`, remove all Token Usage library versions from scanned directories, and retain unrelated plugin configuration. Leave the private data volume and backups intact by default. Confirm restart no longer loads Token Usage. Delete the dedicated data/backup volume only after a separate intentional decision to erase history and after confirming no active owner; uninstalling a binary does not require deleting data.
