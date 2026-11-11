# chat-archive-import

`chat-archive-import` uploads a machine's local AI-client history to the chat archive: Claude Code transcripts (main sessions, subagents, workflow agents and journals), workflow runs and scripts, agent meta files, saved tool outputs, skills and Codex rollouts. It masks and splits everything with the same `pkg/mask` and `pkg/canon` as the CPA plugin, so a block the plugin captured and the same block in a transcript hash equal. It is pure Go (`CGO_ENABLED=0`) for linux/amd64 and darwin/arm64.

It never prints, logs or stores content. Output, state and heartbeats hold counts, sizes, ids, rule ids, file key shapes and times only. The one exception is `mask-review`, which exists for the operator's own terminal and refuses to run anywhere else.

## Build

```sh
cd plugins/chat-archive
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -o dist/chat-archive-import_linux_amd64  ./cmd/chat-archive-import
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/chat-archive-import_darwin_arm64 ./cmd/chat-archive-import
```

`make importer-dist` (the plugin's Makefile) does the same.

## First use

1. Get an enrollment code (`archive enroll-code --kind importer --label <name>` on the archive, or `POST /v1/admin/enroll-codes`). Codes are single use and expire after 30 minutes.
2. Enroll the machine, with the archive's address on your tailnet or WireGuard network:

   ```sh
   chat-archive-import enroll --archive-url http://archive.example.ts.net:8790 --code cha_enr_… --label macbook [--intermittent]
   ```

   It creates the machine id on the first enrollment and keeps it afterwards. It writes `~/.config/chat-archive/config.json` and the source token `~/.config/chat-archive/token` (0600, in a 0700 directory). The token is never printed. Use `--intermittent` for a laptop (24-hour dead-man threshold).
3. Optionally name the machine's CPA client key fingerprint: `chat-archive-import register-caller --key-file ~/.config/cpa/key --name macbook`. Only the fingerprint leaves the machine, in one attempt.
4. Check without uploading: `chat-archive-import run --dry-run`.
5. **Before the first real upload or backfill**, the operator runs `CHAT_ARCHIVE_MASK_REVIEW_OPERATOR=1 chat-archive-import mask-review --rule generic-api-key --sample 50` in their own terminal and decides whether generic-api-key stays in mask/v1. Over-masking is permanent, so the backfill waits for that decision.
6. Backfill once, then install the timer:

   ```sh
   chat-archive-import backfill
   chat-archive-import install-timer   # prints the systemctl or launchctl command that enables it
   ```

## Commands

| Command | What it does |
|---|---|
| `enroll --archive-url U --code C [--label L] [--intermittent]` | Enrolls this machine (above). Waits for the lock. Re-enrolling starts a fresh outbox. Against a different archive (another archive id), every cursor is reset too, so the next run uploads the whole history there |
| `register-caller --key-file F --name N` | Names `CallerScope(key)` with `PUT /v1/callers/{fp}`, one attempt |
| `run [--session ID] [--max-bytes N] [--dry-run]` | One sweep. Exits 0 at once when another run holds the lock. `--max-bytes` (default 256 MiB, 0 = none) bounds the new bytes read, JSONL lines and whole files alike; the next run continues. `--session` limits the sweep to one Claude Code session (the SessionEnd hook) or Codex thread |
| `backfill` | `run` with no byte limit, files in the order of their first record's timestamp. Waits for the lock |
| `status` | Enrollment, outbox, last run, faults, files tracked, backlog bytes, pending tombstones, dead letters by reason |
| `dead list [--reason R]` | Dead letters: time, key, reason, rule ids, state, file key shape, position |
| `requeue --reason R [--since T] [--remask] [--split] [--dry-run]` | Re-reads each dead record at its recorded position and uploads it again with a new seq |
| `exact-fp` | Reads one value from stdin (echo off on a terminal) and prints only `hex(ExactFP(value))`, for `ARCHIVE_EXTRA_EXACT_FPS`. Sends nothing. A value exact masking can never match (under 20 or over 512 bytes, or with a character outside `[A-Za-z0-9_-+/=.~]`, such as a space, `:` or `!`) gets no fingerprint: it exits 1, and stderr names the reason, never the value. Such a secret needs a regex rule instead |
| `mask-dry-run [--root R] [--sample-bytes N] [--synth-mb N] [--seed S]` | Masks everything without uploading and prints matches per rule and per GB, MB/s per rule and cold-input timings against the masking gate |
| `mask-review --rule R --sample N` | Operator only, in their own terminal: masked spans with 40 characters of context. Refuses unless stdout is a terminal and `CHAT_ARCHIVE_MASK_REVIEW_OPERATOR=1` is set |
| `install-timer` | Writes `~/.config/systemd/user/chat-archive-import.{service,timer}` (Linux) or `~/Library/LaunchAgents/com.chat-archive.import.plist` (macOS): `run` every 5 minutes at nice 10 and idle I/O. The systemd service also sets `GOMEMLIMIT=1GiB`, `MemoryHigh=1536M` and `MemoryMax=2G` |

Exit codes: 0 success (also a `run` that found the lock busy), 1 failure or an incomplete run, 2 usage error.

## Files

| Path | Holds |
|---|---|
| `~/.config/chat-archive/config.json` | `archive_url`, `source_id`, `machine_id`, `label`, `roots` |
| `~/.config/chat-archive/token` | the source token (0600) |
| `~/.local/state/chat-archive/state.sqlite` | `meta`, `cursors`, `dead`, `tombstones` (modernc.org/sqlite, WAL, 0600): positions, reasons and counters, never content |
| `~/.local/state/chat-archive/lock` | the exclusive `flock` that `run`, `backfill`, `enroll` and `requeue` hold |

Roots: `claude-projects` = `~/.claude/projects`, `codex-sessions` = `~/.codex/sessions`, `claude-skills` = `~/.claude/skills` (UTF-8 text files of at most 1 MiB). Under `codex-sessions` the importer reads `rollout-*.jsonl` and `rollout-*.jsonl.zst`. Codex compresses a rollout it has not written to for about a week in place, and restores the plain file when the thread is resumed. A compressed rollout is read as its decompressed content under the plain file's key, so the archive sees one file however Codex stores it. When both forms exist, the plain file is read, as Codex does. `config.json` can point them elsewhere. A root may be a symlink (for example to another disk); file keys stay relative to the root.

## How a run works

1. **Lock and server view.** The run takes the lock, then `GET /v1/sources/self`: exact set (plus the fingerprint of its own token), limits, epoch, archive id and the hw of each of its outboxes.
2. **Epoch.** If the epoch or archive id differs from the stored one (a restore), the importer starts a new `outbox_id` (seq from 1). Every file whose last committed seq is above its outbox's hw is read again from offset 0, and pending tombstones of old outboxes are dropped. Keys are source- and position-independent, so re-sent records come back `duplicate`. An epoch change found mid-run does this and sweeps once more.
3. **Change detection.** A file is skipped when its inode, size and mtime match its cursor. Otherwise it is read from `end_offset`, complete lines only. A changed inode, a size below `end_offset`, or a changed sha256 of its first `min(4 KiB, end_offset)` bytes starts a rescan from 0, and `scan_gen` grows by one. For a compressed rollout, offsets, line ordinals and the head hash are those of the decompressed content. Codex rewrites a rollout only to compress or restore it, with the same content, so a Codex rollout's new inode alone never starts a rescan: the head and the size decide. A cursor carries over in both directions, and only lines written since are uploaded. Content shorter than the cursor's `end_offset` starts a rescan. A compressed stream that is empty, cut short or fails to decompress is never recorded as read (`zst_error` or `read_error` in the run's drops); its cursor stays, and it is tried again on the next run. The backlog counts a compressed rollout's compressed size until it is read, and `mask-dry-run` sizes it by its decompressed length. Whole files (workflow runs, scripts, agent meta, tool outputs, skills) are read again when their stat changes; each re-read after the first increments `scan_gen`.
4. **Records.** Each line becomes one `import.record` envelope, keyed by `sha256` of the masked canonical record (`cc/u/…`, `cc/n/…`, `cc/j/…` or `cx/…`). Locators (`machine_id`, `file_key`, `scan_gen`, 1-based `line_ordinal`, `byte_offset`, `byte_len` without the newline, `end_offset`) travel in `Envelope.Loc`, outside the hashed body. A record re-read elsewhere is `duplicate` with a new locator, and a rewritten record is a new key, never a conflict.
5. **Upload.** Batches of at most 500 envelopes or about 8 MiB go through need, `PUT` (pieces over 4 MiB, or over the inline budget) and commit. A batch also goes once the pieces it holds reach 64 MiB, `PUT` pieces included, so memory stays bounded on large saved outputs. Cursors, `next_seq`, dead letters and tombstones move in one transaction after the commit is acknowledged. A file's cursor stops before its first record without a receipt, and `next_seq` moves back to the first seq without one. Pending tombstones at or above that seq are dropped then, because the next run gives those seqs to new envelopes. Their dead rows stay, so `requeue` still finds them.
6. **Heartbeat.** Every run ends with an importer heartbeat (state `ok` or `partial`), except after a 401. A run that lasts longer than 5 minutes also heartbeats on the way, at most every 5 minutes, with state `partial` and the current backlog bytes. A first backfill can take hours, and the archive's importer dead-man fires after 60 minutes without a heartbeat by default, while a timer `run` that finds the lock busy exits without one.

### What each record becomes

Claude Code (`main`, `subagent`, `workflow_agent`):

- `user`, `assistant`, `system`, `attachment`: the frame is the record minus `message.content`, `toolUseResult`, `wireToolInputs` and `wireIngestContext`. Content is split with the Claude block rules as one `{role, content}` message, after each `tool_use.input` is replaced by `wireToolInputs[<tool_use id>]` when present, so the pieces hash equal to CPA's.
- `toolUseResult`: only its structured fields go to `Extra`, as one json piece: `interrupted`, `returnCodeInterpretation`, `filePath`, `structuredPatch`, agent stats (`agentId`, `agentType`, `resolvedModel`, `status`, `totalDurationMs`, `totalTokens`, `totalToolUseCount`, `usage`, `toolStats`, `toolsUsed`), workflow fields (`runId`, `taskId`, `taskType`, `scriptPath`, `transcriptDir`, `workflowName`) and Bash fields (`gitOperation`, `persistedOutputPath`, `persistedOutputSize`, `backgroundTaskId`, `timedOutAfterMs`). Everything else, string and list results included, is dropped; it repeats the tool_result text.
- `attachment`: `prompt_snapshot` puts each `systemPrompt[]` and `tools[]` entry in its own json piece, and the rest stays in the frame. Any other attachment becomes one json piece, and the frame keeps `{"type": …}`.
- `api-request` and the other records without a uuid: the frame stays inline. Exceptions: `queue-operation.content` and `last-prompt.lastPrompt` become text pieces, `api-request-shape` puts its `shape.system[]` and `shape.tools[]` entries in json pieces, and `api-request-blob` keeps only system-role content (as items); user and assistant blob bodies are dropped.
- Workflow journals: key `cc/j/<runId>/<agentId or ->/<type>/<sha>`; `result.result` becomes a json piece.

A piece taken out of a frame appears as an item `{"f":{"path":"<JSON pointer>"},"p":"<hash>"}`. The pointer is RFC 6901, into the record, with keys as masked in the frame. An object member is removed from the frame, and an array element becomes `null` there.

Codex (`codex_rollout`): key `cx/<thread_id>/<ordinal>/<sha>`, with ids from the first `session_meta`. A forked rollout's second (parent) `session_meta` is skipped, and records with `ordinal < subagent_history_start_ordinal` (other than the first line) are `inherited`. `response_item` payloads use the Responses block rules. `item_completed` CommandExecution drops `stdout` and `formatted_output` where they equal `aggregated_output`. Everything else keeps an inline frame with string leaves of at least 1 KiB as text pieces.

Whole files become `import.file` envelopes. Workflow runs and agent meta are masked JSON pieces. Scripts, skills and saved tool outputs are masked text pieces, and bytes that are not UTF-8 are an attachment through canon's attachment rules. A script that a workflow run in the same session embeds byte for byte is skipped.

### Codex and CPA

For a rollout whose `session_meta.model_provider` is `cliproxyapi`, the importer asks `POST /v1/ingest/session-coverage` for `codex:<session_id>` and `codex:<thread_id>`. Records go metadata-only (items keep their frames, lose payloads and volatile fields; no extra pieces) only when they belong to a response CPA holds with content.

A `token_usage_record` closes a model call and names its response. Only two kinds of record belong to that response: the input items sent with the call (user and developer messages, tool outputs, `agent_message`, `additional_tools`) and its output items (the other `response_item` records). Every other record belongs to no response and always goes full, even inside a covered call: `item_completed` (CPA does not hold `aggregated_output`), `world_state`, `turn_context`, `task_*`, `compacted` and the `token_usage_record` itself. A call counts only when its shape is unambiguous: at least one input, then at least one output, no input after an output, and a response id no other record uses. Anything else, including records after the last `token_usage_record`, goes full.

This relies on the order Codex writes rollouts in: a call's output items, then at most some `item_completed` records, then its `token_usage_record`, then the next call's inputs. A shapes-only probe of 446 real rollouts found that order in every call it could check, and never an input between a call's outputs and its usage record. About 95% of calls passed the shape check. The rest were calls without a usage record of their own, compaction calls and interrupted turns, and they upload full. A Codex version that wrote the usage record before its outputs would leave calls of only inputs, or with outputs first. Those fail the shape check too, so an unexpected order uploads full instead of losing content.

Claude Code always uploads full content; the archive's staging tier drops the CPA copies.

### Errors and dead letters

| Answer | Importer |
|---|---|
| `ok`, `duplicate`, `conflict` | acknowledged |
| `rejected` (any reason) | dead row with that reason; the cursor moves past it |
| `retry` `missing_ref` | need runs again; after 3 in a row, local dead letter `missing_ref` plus a tombstone |
| `retry` `busy`, `store_unavailable` | re-sent with backoff, up to 6 rounds; then the run stops and the next run re-reads from the cursor |
| envelope over `limits.envelope_json`, piece over `limits.text_piece`, attachment file over `limits.attachment` | local dead letter `rejected_too_large` plus a tombstone, never sent. A whole file larger than both piece limits is never held in memory or masked. It is hashed as it streams, and its dead row is keyed by the sha256 of its bytes |
| `PUT` 422 `mask_violation` | local dead letter `mask_violation` (rule ids kept) plus a tombstone |
| 400, 413 | halve the batch; one envelope that still fails becomes `rejected_local` or `rejected_too_large` |
| 401, 403 | no upload for 15 minutes (kept across runs), fault `auth_failed`, never retried, no heartbeat |
| 400 `unsupported_protocol` | nothing dead-lettered, fault `server_too_old` |
| 409 `epoch_changed` | the epoch procedure above |
| 429, 503, 5xx, network | backoff with full jitter (honouring Retry-After), up to 5 failures, then fault `upload_failed` |

Tombstones go in the next commit (and at the end of the run), so the archive's hw passes every locally dead seq. A crash between a commit and its cursor transaction re-reads the same records into the same envelopes, and the archive answers `duplicate`. If files grew in between, a seq may now carry another key, which the archive allows (receipts are by key).

`requeue` rebuilds a dead record from its file at the recorded position. Without `--remask` or `--split` the rebuilt key must equal the dead key, which proves the bytes are unchanged. With `--remask` (after `mask_violation` or `mask_version_retired`) the current masker may produce a new key. `--split` additionally moves frame strings of at least 1 KiB into text pieces, so an envelope that was too large can fit. A record whose file was rescanned since, or whose bytes are no longer one complete line at that position, is marked `gone`.

## mask-dry-run and the masking gate

`mask-dry-run` runs every input (one file, or the `--synth-mb` invented text) through the same builders as `run`, one at a time, each with a cold memo. On Linux the time per input is the thread's CPU time (`RUSAGE_THREAD`), so the numbers are per core even on a busy machine; wall time is printed too. The gate is one cold 8.5 MB-class input in at most 3 s and one 50 MB-class input in at most 20 s per core. The tool reports the inputs nearest those sizes, scaled to them.

It also lists the ten largest inputs and the ten slowest real inputs of at least 1 MiB (kind, size, time, rate and the rule that took most of the input's scan time) and extrapolates the worst real density to the two gate sizes. File names are never printed.

`--sample-bytes N` takes a stratified sample of at most N bytes. It includes the largest file of each kind and the files nearest the two gate sizes, then random files from each (kind, month) stratum in proportion to its bytes (seeded by `--seed`). Per-rule MB/s comes from a second, profiled scan of each input's string leaves and keys (`pkg/mask`'s `(*Masker).ProfileRules`), on Linux scaled by that scan's thread CPU/wall ratio so it reads as per-core time.

## Tests

```sh
cd plugins/chat-archive
CGO_ENABLED=0 go test ./internal/importer/...
CGO_ENABLED=1 go test -race ./internal/importer/...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go test -c -o /dev/null ./internal/importer/   # compile check
```

All test data is synthetic (`internal/importer/synth`). Secret canaries are generated at test time. Regenerate goldens with `go test ./internal/importer/ -run 'Golden' -args -update`.
