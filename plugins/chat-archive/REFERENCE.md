# Chat Archive reference

Plugin id `chat-archive`, version 0.1.6. ABI 1; declares RPC schema 7 and refuses a host below 7. Linux amd64 only; the library needs no glibc symbol newer than 2.36.

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
| `config_error` | A referenced secret is unreadable; fix it and save the config again. Hooks pass through, each one capture would have taken counted `dropped.storage_unavailable` |
| `storage_unavailable` | Reason `outbox_unavailable`: the outbox could not be opened (another process holds its lock, or the directory is unusable). A background loop retries the open (5 s, doubling to 5 minutes), and capture resumes on its own once it succeeds. Reason `working_directory_unavailable`: `data-dir` is empty and CPA's working directory could not be read when the plugin loaded, so there is no default `data-dir`. Nothing retries: set `data-dir` to a usable directory and save the config, or restart CPA from a readable working directory. In both cases hooks pass through, each one capture would have taken counted `dropped.storage_unavailable` |
| `stopping` / `stopped` | During a graceful stop (reason `draining` while it still captures, see Stops and upgrades), and after `plugin.quiesce` (version upgrade) until a reconfigure starts it again |

Saving the CPA config with identical plugin settings is a no-op. Changed settings stop the plugin gracefully and start it again with the new ones.

## Stops and upgrades

CPA sends every hook to the running instance until that instance stops. For a version update it calls `plugin.quiesce` on the old instance first, keeps sending hooks to it while the call runs, and switches to the new version only after the call returned and the new library is loaded. A hook that reaches an instance whose admission is closed is lost: counted `dropped.stopped` while that instance is stopping, and `dropped.storage_unavailable` once its stop has finished while capture is still configured on. A stop therefore keeps admission open as long as it can:

1. A quiesce, and a reconfigure that keeps capture on, keep capturing until the exchanges that were open when the stop began have finished (each had its completion), for at most 15 seconds. Exchanges that begin during this wait do not extend it, so steady traffic does not hold every stop to the bound. The replies and completions of exchanges begun before the stop are captured as usual. CPA's Plugin Store update runs the reload in the background, so the wait delays only the switch to the new version.
2. Still capturing, the uploader stops (its in-flight upload, then the final heartbeat), and the background loops end.
3. Admission closes. The plugin processes everything the hooks had queued, writes each stream that is still open as a partial response, flushes the outbox writer, saves the `dropped.*` counters and the numbering of every exchange still open (below), and closes the outbox.

The wait, the uploader's stop, the final flush and the saves share one deadline 25 seconds after the stop began, under the 30 seconds CPA allows; only the outbox close follows it: its clean mark and WAL checkpoint run under a deadline half a second past the stop's (and at most 5 seconds each close), and closing its databases and lock afterwards is not bounded. 25.5 seconds is the budget for the work a stop can cancel, not a guarantee of when it returns: a call that cannot be cancelled holds the stop until it returns. These are an SQLite or filesystem call already in progress (a commit, a blob file's fsync), a wait for the outbox's write mutex, the maintenance checkpoint, the status snapshot, and the close's teardown of the outbox's databases and lock; each returns only when the disk does ([Outbox write stalls](#outbox-write-stalls)). When the close cannot write its clean mark in time, the run stays marked unclean, which the next start reports in `unclean_runs`; a checkpoint cut short after the clean mark was written leaves the run clean. The wait ends at least 10 seconds before that deadline, and the uploader's stop and the final flush are cut short when they would run past it. An upload cut short stays pending in the outbox and goes out after the next start. A queued body that the final flush has no time left to process counts as `dropped.stopped`; when the exchange's completion was waiting for that body, the completion is still written, with `begin_written` false or without that response in `responses`, or counted `dropped.stopped` as well if the writer cannot take it before the deadline.

A partial response has `terminal_status` `incomplete`, `incomplete_reason` `plugin_stopped` and `truncated` set. The rest of that stream reaches the next runtime or version, which writes it as the exchange's next response, also `truncated`. A new version continues the numbering of every exchange that was open when the old instance stopped: at its start it takes the `input_seq`, attempt `ordinal` and response `index` each one reached, whether its begin was written and how many responses were, from the outbox, so the rest of the exchange is stored under new keys. A Responses WebSocket left open across an update therefore keeps its follow-up begins, attempts and responses numbered in call order across both versions, and the exchange's single completion counts every response and reports `begin_written` for the begin the old version wrote. An exchange still waiting for a non-stream reply when admission closes gets nothing written for it at the stop; its response and completion are written by the next runtime or version. The saved numbering is taken once, by the next start on that outbox within 2 minutes; a shutdown or turning capture off saves none, because no exchange continues. The same save keeps, in a `completion_numbering` table, the numbering and hook times of every RequestID closed in the last 10 minutes and of every open one, so a lifecycle that CPA starts under a reused RequestID after the update still gets new keys ([Reused RequestIDs](#reused-requestids)). Versions before 0.1.5 ignore the table. An envelope the outbox still skips because its key is already queued is not stored, and counts as `dropped.duplicate_key`. Hooks that arrive after the close and before the new version is serving are lost, counted `dropped.storage_unavailable`; after the wait above, that window holds only the local flush, the outbox close and CPA's load of the new library.

Turning capture off (`capture: off`) or removing the archive settings stops at once, without the wait. A shutdown (CPA exiting, or the plugin being disabled or removed) does not wait either: CPA has already stopped sending hooks to the plugin.

## What is captured

| Envelope | When | Holds |
|---|---|---|
| `exchange.begin` | every request CPA executes (before-auth), and each Codex duplex follow-up (`input_seq` 1, 2, ...) | classification, kept headers (masked, allowlisted), hints, caller fingerprint, the split request body (prefix-delta against an earlier exchange of the conversation that the archive already acked) |
| `exchange.attempt` | every after-auth call (one per credential and upstream model tried) | auth index, keyed hash of the selected auth id, target format and model, session ids |
| `exchange.response` | each non-stream response and each reassembled stream response (several per Responses WebSocket RequestID) | status, timing, stream counters, bootstrap resets, kept response headers, the split response body |
| `exchange.completion` | every lifecycle completion, at `ex/<rid>/completion`, or at `ex/<rid>/completion/<started_at>` for a later lifecycle of a reused RequestID (below) | outcome (`succeeded`, `failed`, `rejected`, `canceled`), status, masked error text, `begin_written`, time spent in this plugin's hooks, and `lifecycle_input_seq` when a later lifecycle's replayed begin is known |
| `usage.attempt` | every usage record | provider, model, caller fingerprint (from the raw client key, never kept), keyed account fingerprint and label, token counts, latency, masked failure body |

A completion's outcome is CPA's, with one exception: a stream CPA reports `canceled` after every response it carried had already ended with a `completed`, `done` or `incomplete` event (none still open) records `succeeded` with status 200. CPA v8.0.60 cancels a Responses WebSocket turn when the client closes the socket right after the turn's last event, before CPA has read the upstream close, and always cancels a duplex RequestID, which ends only with its socket; for an HTTP Responses stream it already reports `succeeded` once delivery was acknowledged.

### Reused RequestIDs

From v8.0.76, CPA fails a Responses WebSocket turn over to another credential in place when the pinned credential fails before any output: the retry runs on the same socket, under the failed attempt's RequestID, as a second lifecycle with its own `started_at`. A client that sends its own `request_id` metadata twice has the same effect on any CPA version. The plugin keeps every lifecycle apart:

- **Numbering continues.** The retry's begin, attempts and responses take the next `input_seq`, attempt `ordinal` and response `index`, so no key is used twice. When a completion closes a RequestID, or the idle sweep evicts it, the plugin keeps the RequestID's numbering and hook times for 10 minutes (at most 256 per shard of 64; one dropped for capacity counts `numbering_evicted`). A completion keeps the RequestID open while a later lifecycle's hooks are still arriving.
- **Completion keys.** A completion is bare (`ex/<rid>/completion`) when nothing the plugin knows shows an earlier lifecycle: no hook of the RequestID started before its `started_at`, and no other completion took the bare key. That is every completion of a RequestID that is never reused. Any other completion is written at `ex/<rid>/completion/<started_at>`, the start in Unix nanoseconds (19 digits), which is unique per lifecycle. When the in-memory record is gone (expired, evicted, or older than a stop's 2-minute carry), the bare key's own record decides: if the outbox already holds it, the completion is inserted at its `started_at` key in the same write (`completion_rekeyed.outbox`), and if the archive answers `conflict` for it, the uploader queues it again there (`completion_rekeyed.archive`). A RequestID of 167 bytes or more has no room for the suffix and keeps the bare key. `reused_request_id` counts every completion written at a `started_at` key. An archive server decides an exchange's outcome by the completion with the latest `started_at`; update the server before this plugin.
- **Replayed input.** A later lifecycle starts by replaying the turn's input as a new begin. When the plugin observed that before-auth and can prove no hook of the lifecycle came first, the completion names it in `lifecycle_input_seq` (always 1 or more), so the archive does not show the replay as a new input. It cannot prove it after a decode error, a restart or update, an idle eviction, or four or more duplex follow-ups before the completion is processed. Such completions are counted in `lifecycle_unverified`, and the replayed input may show twice. No real input is ever hidden.
- **Streams.** Each stream attempt is a segment. A header-init that comes after a before-auth of the same RequestID parks the earlier segment instead of discarding it (`segments_parked`). A bootstrap retry with no before-auth in between is still a reset, counted in `bootstrap_resets`. A completion writes only the segments its lifecycle times prove are its own, with its status. When a segment could belong to either of two lifecycles, the completion is held (`ownership_held`) until a later hook or the other completion settles it. A `succeeded` completion is never held, because CPA retries only a lifecycle that did not succeed. If nothing settles it within a minute, the plugin writes every candidate segment with the completion's status, counts `segments_uncertain`, and releases nothing; the check runs once a minute, so this takes 1 to 2 minutes. The same hold applies to a non-succeeded stream whose segment cannot be proven its own on a RequestID that is never reused: one that spans a restart or update, follows an undecodable hook, or is a duplex stream with a follow-up before or between its header-inits. Its response is the same as before, written up to 2 minutes later. In the last case the earlier attempt is also kept, as an extra partial response after the final one.

All these counters are in the status route's `counters`, not in heartbeats.

A stream response's terminal status (`completed`, `incomplete`, `failed`, `done`, `superseded`, `canceled`) comes from its terminal event, or from how the request ended when none arrived. CPA hands a non-stream response to the plugin only after a successful execution, so it records `completed`, unless a Responses object's own `status` says otherwise: `incomplete` and `failed` as sent, `cancelled` as `canceled`, and no terminal status for a background response that is still `queued` or `in_progress`.

A request or response body labelled with `Content-Encoding` gzip, zstd, deflate or br (alone or stacked, such as `gzip, br`) is decoded before the split and masked like any other, with flag `gunzipped` for gzip and `content_decoded` for the others. Each layer's decoded size is capped like a gzip body's, all layers together at twice that cap, and a body that names more than three codings is not decoded at all. A Brotli stream whose declared window is over that cap (and over 4 MiB, the encoders' default) is refused before its decoder allocates the window; it counts as `dropped.body_budget`, as does any body over the cap. Decoding stops at the masking deadline and at shutdown's drain, as masking does. A client that compresses its request body (Codex sends zstd) is therefore stored, not dropped. CPA decodes compressed upstream bodies itself but still hands the hooks the upstream `Content-Encoding`, so a body that is not in its labelled coding and parses as a JSON object is split as it is, with flag `encoding_ignored`. Deflate and br streams have no magic bytes and can start with `{` or `[`. So a body that is whole valid JSON (any value) is never fed to their decoders, any other body is, and a deflate stream must end exactly where the body ends. A plain body that is not JSON could otherwise "decode" into garbage. Repeated `Content-Encoding` field lines are combined in order, as RFC 9110 combines list headers, and the header is read in any key case. Anything else (a stream that does not decode, an unknown coding, more than three codings, a labelled body that is valid JSON but not an object) may still be compressed: none of it is stored, and the envelope keeps metadata only with `dropped: parse_error`. This is deliberate: compressed bytes cannot be mask-scanned, so storing them would bypass masking. Each such drop is counted in `dropped.parse_error`, which the archive alerts on. The response id and Responses status of a decoded non-stream body are read from the decoded bytes, so the exchange stays linkable to its transcript.

Model-directory GETs (`/v1/models`, `/v1/models/{id}`, `/v1beta/models` and CPA's other model catalogs) are ignored by both the response and lifecycle hooks. Each hook that ignores one adds 1 to `ignored_model_list`, so a call usually adds 2; the counter is in the status route's `counters` and not in heartbeats. A call counts as one when it has no model, no requested model, no request body (response hook), no `requested_model` key in its Metadata, and no before-auth for its RequestID. CPA sets `requested_model` on every model execution, while a model-directory call carries null Metadata or, since CPA v8.0.70, only the caller's identity. Provider media URLs (images, video) are replaced by their stripped form in the stored response; the full signed URL is held only in memory and sent once to the archive's media-fetch route.

## Hot path and limits

Hooks do no I/O, compression or masking. Bodies are decoded straight from CPA's call buffer, reserve four times their size plus their decoded headers from a 256 MiB content pool before decoding, and go to the capture workers through bounded queues. Once a worker has built a body's envelope, its reservation drops to the bytes the queued envelope keeps (about 1.15 times the body), and the writer returns those once it has written or dropped the envelope. When a budget or queue is full the plugin keeps the metadata where it can and counts the reason under `dropped`. Per-call caps: body-carrying hooks `max-rpc-mib`; stream chunks 16 MiB; after-auth, completion and management 1 MiB; usage 4 MiB.

Each `dropped.*` counter has one unit, except `stopped` and `usage_queue`, which count the events their rows list; counters of different units are not comparable:

| Unit | Counters |
|---|---|
| a body or attachment shed, its envelope kept as metadata | `body_budget` (the content pool, the split's decode limits, or the write path's 32 MiB per-envelope ceiling), `handoff_full`, `disk_guard`, `outbox_cap`, `mask_timeout`, `oversize` (an attachment kept as a reference, or a text piece over the archive's limit), `parse_error` |
| a whole envelope not stored | `control_full`, `disk_hard`, `encode_error`, `duplicate_key` (the outbox skipped it because its key was already queued), `hold_oversize` (an envelope that keeps more than 32 MiB beyond its pool charge even as metadata) |
| a hook call, queued item or envelope lost to a stop | `stopped`: once for each hook call refused while the plugin is stopping; once for each item a stop drains from the hook queues unprocessed (a begin, response, attempt, completion or usage record, or a single stream event); once for each envelope the stop cut short (a body split or usage record cancelled, a stream still open when the stream goroutine ends, or an envelope the writer did not take or write before the stop's deadline) |
| a usage record shed or dropped | `usage_queue`: once when the record does not fit the usage queue whole, however many fallback steps follow (failure body shed, response headers shed, record dropped); once more when the usage channel's 16,384 slots are full, which drops the record |
| a stream event refused | `stream_overflow` |
| a hook call | `oversize_rpc` (over its per-call cap), `storage_unavailable` (capture is configured on but unavailable: a missing secret, no working directory, an outbox that cannot be opened, or a start or stop in progress); a payload chunk counts in full capture only |
| a media fetch | `media_fetch` |

A usage record that does not fit the 32 MiB usage queue sheds its failure body, then its response headers, and is dropped only when it does not fit bare; `usage_queue` counts it once whichever step it reaches. Each step charges the record's metadata (its model, base URL, session id and the other fields it keeps) at their full size, and its decoded response headers with their storage (a slot for every key and value, however short), so the queue holds at most 32 MiB whatever the fields carry.

### Outbox write stalls

The outbox writer writes up to 50 envelopes in one transaction. While the plugin runs, a write has no timeout: when the disk or another user of the outbox's single write connection stalls, the writer waits, and the envelopes behind it wait in its queue. A write that fails with an error a retry can fix (busy, locked, an I/O error, a full disk, a file that cannot be opened, a blob file error) is retried in order after a backoff that doubles from 25 ms to 2 s, for 60 seconds from the moment its first failed attempt began. The window counts from that attempt's start, not from when it returned, so an attempt that took 55 seconds to fail leaves about 5 seconds of retries. The group is dropped as `dropped.disk_hard` only when an attempt that started 60 seconds or more after the first failed attempt began fails too; the next group gets its own 60 seconds. An error no retry can fix (the outbox is closed, an envelope does not encode, SQLite refuses its content) drops the group as `disk_hard` at once. Pressure level 3 still drops the group as `disk_hard`. `write_unavailable` is raised by the first failed attempt or by an attempt that has not returned after 10 seconds, and cleared by the next successful write; nothing is dropped because of it.

The writer's queue holds 16,384 envelopes, about 2.75 MiB of slots. A 64 MiB hold budget bounds the bytes queued envelopes keep beyond what the content pool or the usage queue already charges for them (an envelope without a body or usage record: its keys, JSON and commit callback's data; a body's envelope: what its split decoded beyond the body's reservation), with first-come admission. When it is full, the producer waits, as at a full queue, and the queues before it shed with their counters. One envelope may hold at most 32 MiB: a begin or response over that is written as metadata only (`body_budget`); an envelope still over it is not written (`hold_oversize`). So queued envelopes add at most about 67 MiB to the content pool and the usage queue. A 60 second stall at about 90 envelopes a second with two bursts of 640 envelopes queues about 6,700 envelopes and 5.3 MiB of hold. The status route's `write_path` shows the queue's length and peak and the hold's bytes and peak.

With large bodies, the content pool fills before the queue does. On the stall test harness, with request bodies shaped like production's over 7 days (median 390 KiB, p99 2.2 MiB, largest 5.3 MiB), the pool held about 224 MiB of queued bodies before the next body no longer fit. How long a stall lasts before the first body is shed depends on the body bytes a minute:

| Body bytes a minute | Stall before the first shed |
|---|---|
| 33 MiB (production's p99 active minute) | 6 min 47 s |
| 91 MiB (production's peak minute) | 2 min 27 s |
| 182 MiB (twice the peak minute) | 1 min 14 s |

A 60 second stall at twice the peak minute, with 90 small envelopes a second on top, stored every envelope and shed no body; the pool peaked at 208 MiB. In a longer stall, bodies are shed as `body_budget` and their envelopes are kept as metadata.

At a stop, the stop's deadline also ends a write still waiting for the connection and a retry's backoff, and the writer counts every group it has not written as `dropped.stopped`, without touching the disk once the deadline has passed. A write already inside SQLite's commit or a blob file's fsync, or waiting for the outbox's write mutex, returns only when the disk does, as in 0.1.5.

## Management routes

| Route | Auth | Use |
|---|---|---|
| `GET /v0/management/plugins/chat-archive/status` | management key | JSON snapshot: state, version, config summary (presence only), outbox, uploader, hook calls, latency, last activity, drops, panics, faults, dead letters by reason, requeue jobs. Never content; served from memory, never from SQLite |
| `POST /v0/management/plugins/chat-archive/requeue?reason=<r>&remask=0\|1` | management key | queue dead letters of one reason for another upload; answers `202 {"job": id}` at once or `409` while a job runs. `full=1` and `split=1` answer `400 unsupported_option` in this version |
| `GET /v0/resource/plugins/chat-archive/status` | public | the fixed sidebar shell (hashed-script CSP, `no-store`); its script calls the status route with your console session and stops on 401/403 without retrying |

## Faults

`write_unavailable` (an outbox write failed or has not returned after 10 s; the writer keeps retrying, see [Outbox write stalls](#outbox-write-stalls)), `auth_failed` (the archive answered 401/403; uploads, heartbeats and media fetches pause for 15 minutes, never retried in a loop), `replay_gap` (the archive restored behind the local retention window), `server_too_old`, `masker_rebuild`, plus a fault per goroutine that panicked and was restarted (the panic's type is recorded, never its value).

## Heartbeats

Every 60 seconds, and once at shutdown, the plugin posts counts, names and times to `POST /v1/heartbeat` directly (never through the outbox). A blackholed or unreachable archive stops heartbeats, which the archive's dead-man check alerts on; CPA keeps serving and the outbox grows.

Counters are cumulative per `boot_id` (one plugin process), and the archive derives `capture_drop` and the daily `dropped.*` deltas from heartbeats only. So that a crash between heartbeats, a lost final heartbeat, or drops counted while capture drains after the final heartbeat (`dropped.stopped`) lose no count, the plugin saves its `dropped.*` counters in `outbox.sqlite` within about 100 ms of a drop (one write per burst, retried each second while the write fails) and once more when it stops. Once a boot has counted a drop, each of its heartbeats also saves the record, with that heartbeat's `n`, just before it is sent. So the saved `n` and counters are never behind a heartbeat the archive has. The next process that opens the outbox reports each other boot's saved counters in a heartbeat with that boot's `boot_id`, `version` and `started_at`, state `recovered` and `n` one past the saved one, sent just before its own heartbeat. It deletes the record once the archive answers. A record the archive could not take now (unreachable, busy, 5xx) is sent again next minute, ahead of the later ones. A record it refuses for good (a 4xx other than 401, 403 and 429) is deleted unreported and counted in the status counter `recovered_refused`, and the next record goes. A recovered heartbeat carries the current outbox, uploader and mask fields and no hook counters. The archive takes a boot's last heartbeat as its total, so a record reported twice adds no drops. At most 32 boot records are kept, oldest deleted first.

## Data path

`outbox.sqlite` with its `-wal`, `-shm` and `.lock` files and `blobs/` for pieces over 256 KiB. Pending envelopes are never evicted; acked envelopes are kept `replay-retention-hours` for archive replay; dead letters 90 days. Times are UTC.
