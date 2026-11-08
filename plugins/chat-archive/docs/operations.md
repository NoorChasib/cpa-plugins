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
| `storage_unavailable` | Another process holds the outbox lock, or `data-dir` is not writable. The plugin retries the open in the background and starts capturing once it succeeds; fix the cause, no config change is needed |
| fault `auth_failed` | The archive rejected the source token (rotated or revoked). Uploads pause 15 minutes at a time; mint a new token and redeploy |
| `pending` grows, heartbeats stop at the archive | The archive is unreachable. Nothing is lost: envelopes stay pending until the archive answers |
| `dropped.body_budget` | Several very large bodies arrived at once; their metadata was kept, their bodies were not. The content pool is a fixed 256 MiB, so raising `memory-limit-mib` does not change this; it only gives the plugin's Go runtime more room. Keep `memory-limit-mib` at 768 or more: one 50 MB prompt needs about 530 MiB |
| dead letters `mask_violation` | The archive's re-check found a value the plugin's masker missed (typically one captured before the first exact-set sync). Requeue with `remask=1` |
| `dropped.stopped` after a restart or upgrade | Bodies in flight when the plugin stopped are not written; their metadata is |

## Requeue

`POST /v0/management/plugins/chat-archive/requeue?reason=mask_violation&remask=1` with the management key queues every dead letter of that reason for another upload (re-masked with the current exact set when `remask=1`). It answers `202` with a job id at once; the status route shows the job's progress as counts. Only one job runs at a time (`409` otherwise). Re-masking shares the capture memory budget: a dead letter whose pieces do not fit while capture is busy counts as `failed` and stays a dead letter, so run the requeue again when traffic is quieter.

## Kill switch and removal

Set `capture: off` and save: the plugin keeps registering and stripping `X-Archive-*` headers, but captures nothing. Disabling the plugin in CPA stops it and closes the outbox; the outbox stays on disk. Deleting `plugins/data/chat-archive/` discards anything not yet acked, and the next start uses a new outbox id.

## Upgrades

A new version file makes CPA quiesce the old instance (it stops every goroutine and closes the outbox) and load the new one. Streams that are open during the switch finish for the client; the old instance counts their bodies as `stopped`.
