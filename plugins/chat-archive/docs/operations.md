# Operations

## Daily checks (archive side)

Agents check the plugin only through the archive (`GET /v1/status`, `GET /v1/canary/latest`, the MCP `sql` tool), never through CPA's management routes: on a host where CPA and agents run together, every process shares one client IP, and five failed management-key attempts ban it for 30 minutes. Only the operator opens the sidebar.

Healthy means: state `running`, a heartbeat younger than two minutes, `outbox.pending` and `oldest_pending_age_s` small, a recent `last_ack_at`, no faults and no growing `dropped.*` counters.

## Sidebar

**Chat Archive** in CPA's management sidebar shows state, version, pending envelopes, the oldest pending age, the last archive ack, faults, drops, recovered panics, dead letters, outbox and uploader counters, per-hook calls and latency, and the requeue jobs of this instance. It refreshes only when you press **Refresh**. A rejected console session stops it; sign in again with "Remember password" and refresh.

## Alerts

With the Pushover settings filled in, the plugin sends at most one message per kind per hour, holding counts and fault names only: oldest pending older than `alert-backlog-hours`, any new `dropped.*`, outbox pressure level 2 or more, and the faults `auth_failed`, `replay_gap` and `write_unavailable`. The archive runs its own dead-man, backlog and capture-stall alerts from heartbeats.

## Common situations

| Symptom | Cause and fix |
|---|---|
| `unconfigured` with reason `archive_url_missing`, `source_token_missing` or `hmac_key_missing` | Fill in the config (README, First use) |
| `config_error` / `secret_unavailable` | The environment variable is unset in CPA's process (set it and redeploy CPA), or the file is not `0600`, not owned by CPA's user, a symlink, or has more than one link |
| `storage_unavailable` | Another process holds the outbox lock, or `data-dir` is not writable. The plugin retries the open in the background and starts capturing once it succeeds; fix the cause, no config change is needed. Hooks meanwhile count `dropped.storage_unavailable` |
| fault `write_unavailable` | An outbox write failed or has been waiting more than 10 s (a slow or stalled disk, or a full one). The writer keeps the group and retries it in order for 60 s; the fault clears with the next successful write. Check the disk (`dmesg`, free space, I/O wait) |
| `dropped.disk_hard` | Outbox writes kept failing for more than 60 s, the outbox's disk is nearly full (pressure level 3), or SQLite refused an envelope: those envelopes were not stored. Free space or fix the disk |
| `dropped.storage_unavailable` | Hook calls arrived while capture was configured on but could not run: a missing secret, an outbox that could not be opened, a reconfigure or an upgrade in progress. Counted per hook call, not per envelope. A few during a reconfigure or an upgrade are expected; a steady count means the plugin is not capturing (see its state) |
| `dropped.hold_oversize` | An envelope kept more than 32 MiB beyond its content-pool charge even as metadata (tens of MiB of headers or fields) and was not stored. Any count here is unexpected: report it |
| fault `auth_failed` | The archive rejected the source token (rotated or revoked). Uploads pause 15 minutes at a time; mint a new token and redeploy |
| `pending` grows, heartbeats stop at the archive | The archive is unreachable. Nothing is lost: envelopes stay pending until the archive answers |
| `dropped.body_budget` | Several very large bodies arrived at once, an outbox stall outlasted the pool (about 224 MiB of queued bodies, see REFERENCE.md's Outbox write stalls), or a compressed body decodes past its share of the pool (a Brotli stream whose window alone is over it counts too); their metadata was kept, their bodies were not. The content pool is a fixed 256 MiB, so raising `memory-limit-mib` does not change this; it only gives the plugin's Go runtime more room. Keep `memory-limit-mib` at 768 or more: one 50 MB prompt needs about 530 MiB |
| `dropped.parse_error` | A body labelled with a `Content-Encoding` could not be read, so only its metadata was kept: a gzip, zstd, deflate or br stream that does not decode (a deflate stream must end where the body ends), an unknown coding, more than three codings, or a body not in its labelled coding that does not parse as a JSON object (a JSON array, string or literal, text, multipart or media may still be compressed, so none of it is stored). A body that decodes is stored with flag `gunzipped` or `content_decoded`, and a labelled body that parses as a JSON object is CPA's decoded body under its stale upstream `Content-Encoding`, stored with flag `encoding_ignored`; neither is counted here. The archive raises `capture_drop` for any increase |
| dead letters `mask_violation` | The archive's re-check found a value the plugin's masker missed (typically one captured before the first exact-set sync). Requeue with `remask=1` |
| `dropped.stopped` after a restart or upgrade | Hooks arrived after the stopping instance closed admission (REFERENCE.md, Stops and upgrades): their data was not captured. A stop first waits up to 15 s for the exchanges open when it began to finish, so a count here means one of them stayed open through that wait (a long stream or an open Responses WebSocket), a new exchange arrived while the instance was stopping, or the final flush ran out of the stop's 25 s. Hooks that arrive after the stop has finished, until the new version serves, count as `dropped.storage_unavailable` instead |
| `dropped.duplicate_key` | The outbox skipped an envelope because an envelope with the same key was already queued, so its content was not stored. Keys are unique per exchange part and a new version continues the numbering of exchanges open across an update, so any count here is unexpected: note the time and version and report it |

## Requeue

`POST /v0/management/plugins/chat-archive/requeue?reason=mask_violation&remask=1` with the management key queues every dead letter of that reason for another upload (re-masked with the current exact set when `remask=1`). It answers `202` with a job id at once; the status route shows the job's progress as counts. Only one job runs at a time (`409` otherwise). Re-masking shares the capture memory budget: a dead letter whose pieces do not fit while capture is busy counts as `failed` and stays a dead letter, so run the requeue again when traffic is quieter.

## Kill switch and removal

Set `capture: off` and save: the plugin keeps registering and stripping `X-Archive-*` headers, but captures nothing. Disabling the plugin in CPA stops it and closes the outbox; the outbox stays on disk. Deleting `plugins/data/chat-archive/` discards anything not yet acked, and the next start uses a new outbox id.

## Upgrades

A new version file makes CPA quiesce the old instance and then load the new one. The old instance keeps capturing until the exchanges open when the quiesce began have finished (at most 15 s; new exchanges do not extend the wait), then stops every goroutine, saves the numbering of the exchanges still open and closes the outbox; everything but the close ends within 25 s. Streams that are open during the switch finish for the client; a stream still open when the old instance closes is written as a partial response (`incomplete_reason` `plugin_stopped`, `truncated`), and the new version captures the rest of it as the exchange's next response. A Responses WebSocket left open across the switch keeps going: the new version numbers its next turns after the old version's. See REFERENCE.md, Stops and upgrades.
