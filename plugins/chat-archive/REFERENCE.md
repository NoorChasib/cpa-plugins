# Chat Archive reference

Plugin id `chat-archive`, version 0.1.1. ABI 1; declares RPC schema 7 and refuses a host below 7. Linux amd64 only; the library needs no glibc symbol newer than 2.36.

## Capabilities

`request_interceptor`, `request_interceptor_omit_body_after`, `response_interceptor`, `response_stream_interceptor`, `request_lifecycle_plugin`, `usage_plugin`, `management_api`. It never changes a request or response: every hook answers a pre-encoded pass-through, except that before-auth lists each `X-Archive-*` header present in `ClearHeaders` so hints never reach a provider. It never sets `DropChunk`, never terminates a request and never calls `host.auth.get`.

## Settings (`plugins.configs.chat-archive`)

| Key | Default | Rule |
|---|---|---|
| `archive-url` | `""` | `http://` or `https://` URL without userinfo, query or fragment |
| `source-token-env` / `source-token-file` | `""` | exactly one; env name `^[A-Z_][A-Z0-9_]{0,63}$`; file absolute, `0600`, owned by CPA's user, one link |
| `hmac-key-id` | `k1` | `^[a-z0-9]{1,8}$` |
| `hmac-key-env` / `hmac-key-file` | `""` | exactly one; the key is at least 32 bytes |
| `exact-fingerprints` | `[]` | 64-hex `ExactFP` values of extra secrets to mask exactly |
| `account-labels` | `{}` | AuthIndex or account fingerprint to a display label; at most 256 entries, labels at most 128 characters |
| `data-dir` | `<cwd>/plugins/data/chat-archive` | absolute and clean; never another plugin's directory |
| `outbox-cap-bytes` | 20 GiB | at least 1 GiB |
| `min-free-bytes` / `min-free-percent` | 10 GiB / 5 | free-space guard for the outbox filesystem |
| `replay-retention-hours` | 168 | 24 to 720; acked envelopes kept for archive replay |
| `memory-limit-mib` | 768 | 128 to 4096; applied with `debug.SetMemoryLimit` to the plugin's own Go runtime. The default leaves room for the largest captured bodies: one 50 MB prompt needs about 530 MiB of plugin memory |
| `workers` | 2 | 1 to 4 capture workers |
| `max-rpc-mib` | 160 | 8 to 512; larger body-carrying hook calls pass through uncaptured (`dropped.oversize_rpc`) |
| `capture` | `full` | `full`, `metadata` (no bodies) or `off` (kill switch) |
| `pushover-app-token-env`/`-file`, `pushover-user-key-env`/`-file` | `""` | optional plugin-side alerts |
| `alert-backlog-hours` | 6 | 1 to 168 |

The host-owned `enabled`, `priority` and `store` keys are accepted and ignored. Any other key is a config error; errors name the key, never its value.

## States

| State | Meaning |
|---|---|
| `unconfigured` | No archive URL, source token or HMAC key (the state right after a Plugin Store install), or `capture: off`. Hooks pass through; nothing is captured |
| `running` | Capturing and uploading |
| `degraded` | Running with an active fault (see Faults) |
| `config_error` | A referenced secret is unreadable; fix it and save the config again |
| `storage_unavailable` | The outbox could not be opened (another process holds its lock, or the directory is unusable). Hooks pass through while a background loop retries the open (5 s, doubling to 5 minutes); capture resumes on its own once it succeeds |
| `stopping` / `stopped` | During a graceful stop, and after `plugin.quiesce` (version upgrade) until a reconfigure starts it again |

Saving the CPA config with identical plugin settings is a no-op. Changed settings stop the plugin gracefully and start it again with the new ones.

## What is captured

| Envelope | When | Holds |
|---|---|---|
| `exchange.begin` | every request CPA executes (before-auth), and each Codex duplex follow-up (`input_seq` 1, 2, ...) | classification, kept headers (masked, allowlisted), hints, caller fingerprint, the split request body (prefix-delta against an earlier exchange of the conversation that the archive already acked) |
| `exchange.attempt` | every after-auth call (one per credential and upstream model tried) | auth index, keyed hash of the selected auth id, target format and model, session ids |
| `exchange.response` | each non-stream response and each reassembled stream response (several per Responses WebSocket RequestID) | status, timing, stream counters, bootstrap resets, kept response headers, the split response body |
| `exchange.completion` | every lifecycle completion | outcome (`succeeded`, `failed`, `rejected`, `canceled`), status, masked error text, `begin_written`, time spent in this plugin's hooks |
| `usage.attempt` | every usage record | provider, model, caller fingerprint (from the raw client key, never kept), keyed account fingerprint and label, token counts, latency, masked failure body |

A completion's outcome is CPA's, with one exception: a stream CPA reports `canceled` after every response it carried had already ended with a `completed`, `done` or `incomplete` event (none still open) records `succeeded` with status 200. CPA v8.0.60 cancels a Responses WebSocket turn when the client closes the socket right after the turn's last event, before CPA has read the upstream close, and always cancels a duplex RequestID, which ends only with its socket; for an HTTP Responses stream it already reports `succeeded` once delivery was acknowledged.

A stream response's terminal status (`completed`, `incomplete`, `failed`, `done`, `superseded`, `canceled`) comes from its terminal event, or from how the request ended when none arrived. CPA hands a non-stream response to the plugin only after a successful execution, so it records `completed`, unless a Responses object's own `status` says otherwise: `incomplete` and `failed` as sent, `cancelled` as `canceled`, and no terminal status for a background response that is still `queued` or `in_progress`.

A request or response body labelled with `Content-Encoding` gzip, zstd, deflate or br (alone or stacked, such as `gzip, br`) is decoded before the split and masked like any other, with flag `gunzipped` for gzip and `content_decoded` for the others. Each layer's decoded size is capped like a gzip body's, all layers together at twice that cap, and a body that names more than three codings is not decoded at all. A Brotli stream whose declared window is over that cap (and over 4 MiB, the encoders' default) is refused before its decoder allocates the window; it counts as `dropped.body_budget`, as does any body over the cap. Decoding stops at the masking deadline and at shutdown's drain, as masking does. A client that compresses its request body (Codex sends zstd) is therefore stored, not dropped. CPA decodes compressed upstream bodies itself but still hands the hooks the upstream `Content-Encoding`, so a body that is not in its labelled coding and parses as a JSON object is split as it is, with flag `encoding_ignored`. Deflate and br streams have no magic bytes and can start with `{` or `[`. So a body that is whole valid JSON (any value) is never fed to their decoders, any other body is, and a deflate stream must end exactly where the body ends. A plain body that is not JSON could otherwise "decode" into garbage. Repeated `Content-Encoding` field lines are combined in order, as RFC 9110 combines list headers, and the header is read in any key case. Anything else (a stream that does not decode, an unknown coding, more than three codings, a labelled body that is valid JSON but not an object) may still be compressed: none of it is stored, and the envelope keeps metadata only with `dropped: parse_error`. This is deliberate: compressed bytes cannot be mask-scanned, so storing them would bypass masking. Each such drop is counted in `dropped.parse_error`, which the archive alerts on. The response id and Responses status of a decoded non-stream body are read from the decoded bytes, so the exchange stays linkable to its transcript.

Model-list GETs are ignored by both the response and lifecycle hooks (counter `ignored_model_list`). Provider media URLs (images, video) are replaced by their stripped form in the stored response; the full signed URL is held only in memory and sent once to the archive's media-fetch route.

## Hot path and limits

Hooks do no I/O, compression or masking. Bodies are decoded straight from CPA's call buffer, reserve four times their size from a 256 MiB content pool before decoding, and go to the capture workers through bounded queues. When a budget or queue is full the plugin keeps the metadata and counts the reason under `dropped`: `body_budget`, `handoff_full`, `control_full`, `usage_queue`, `stream_overflow`, `mask_timeout`, `oversize`, `oversize_rpc`, `parse_error`, `disk_guard`, `outbox_cap`, `disk_hard`, `encode_error`, `stopped`, `media_fetch`. Per-call caps: body-carrying hooks `max-rpc-mib`; stream chunks 16 MiB; after-auth, completion and management 1 MiB; usage 4 MiB.

## Management routes

| Route | Auth | Use |
|---|---|---|
| `GET /v0/management/plugins/chat-archive/status` | management key | JSON snapshot: state, version, config summary (presence only), outbox, uploader, hook calls, latency, last activity, drops, panics, faults, dead letters by reason, requeue jobs. Never content; served from memory, never from SQLite |
| `POST /v0/management/plugins/chat-archive/requeue?reason=<r>&remask=0\|1` | management key | queue dead letters of one reason for another upload; answers `202 {"job": id}` at once or `409` while a job runs. `full=1` and `split=1` answer `400 unsupported_option` in this version |
| `GET /v0/resource/plugins/chat-archive/status` | public | the fixed sidebar shell (hashed-script CSP, `no-store`); its script calls the status route with your console session and stops on 401/403 without retrying |

## Faults

`write_unavailable` (outbox writes failing; captures dropped as `disk_hard` until a probe succeeds), `auth_failed` (the archive answered 401/403; uploads, heartbeats and media fetches pause for 15 minutes, never retried in a loop), `replay_gap` (the archive restored behind the local retention window), `server_too_old`, `masker_rebuild`, plus a fault per goroutine that panicked and was restarted (the panic's type is recorded, never its value).

## Heartbeats

Every 60 seconds, and once at shutdown, the plugin posts counts, names and times to `POST /v1/heartbeat` directly (never through the outbox). A blackholed or unreachable archive stops heartbeats, which the archive's dead-man check alerts on; CPA keeps serving and the outbox grows.

Counters are cumulative per `boot_id` (one plugin process), and the archive derives `capture_drop` and the daily `dropped.*` deltas from heartbeats only. So that a crash between heartbeats, a lost final heartbeat, or drops counted while capture drains after the final heartbeat (`dropped.stopped`) lose no count, the plugin saves its `dropped.*` counters in `outbox.sqlite` within about 100 ms of a drop (one write per burst, retried each second while the write fails) and once more when it stops. Once a boot has counted a drop, each of its heartbeats also saves the record, with that heartbeat's `n`, just before it is sent. So the saved `n` and counters are never behind a heartbeat the archive has. The next process that opens the outbox reports each other boot's saved counters in a heartbeat with that boot's `boot_id`, `version` and `started_at`, state `recovered` and `n` one past the saved one, sent just before its own heartbeat. It deletes the record once the archive answers. A record the archive could not take now (unreachable, busy, 5xx) is sent again next minute, ahead of the later ones. A record it refuses for good (a 4xx other than 401, 403 and 429) is deleted unreported and counted in the status counter `recovered_refused`, and the next record goes. A recovered heartbeat carries the current outbox, uploader and mask fields and no hook counters. The archive takes a boot's last heartbeat as its total, so a record reported twice adds no drops. At most 32 boot records are kept, oldest deleted first.

## Data path

`outbox.sqlite` with its `-wal`, `-shm` and `.lock` files and `blobs/` for pieces over 256 KiB. Pending envelopes are never evicted; acked envelopes are kept `replay-retention-hours` for archive replay; dead letters 90 days. Times are UTC.
