# Verification record

Native acceptance of chat-archive 0.1.0, run 2026-11-08 on a shared Linux amd64 host with synthetic data only. CPA v8.0.60 was built from commit `91e0dee827abf476f9f86246f68c500a112f7155` (`make smoke` and `make fuse-smoke` with `CPA_SOURCE_DIR` set to a checkout of that commit). The acceptance container ran with `--network none`, 2 CPUs and 3 GiB. The plugin ran with its default `memory-limit-mib` of 768.

`make smoke`, all phases (main, duplex, fixture, memory, latency, blackhole): **74 checks passed, 0 failed**. `make fuse-smoke` passed. The official-image store check (`make store-smoke`) was not rerun in this pass; its last result (passed without peers, because GitHub release downloads answered 504) applies to code it does not exercise differently.

## Cases

| Case | Result | Evidence |
|---|---|---|
| ABI probe, production and nativefixture libraries | pass | 44 checks each |
| Claude, OpenAI chat, Responses (Codex), Gemini: stream and non-stream | pass | begin, attempt, response and completion for each, with kept headers, hints and response headers checked. Each response's terminal status is checked too: `completed` (or `done`) for a stream, `completed` for a non-stream response. That check was added later and passed in a rerun of the `main` phase only (`SMOKE_ONLY=main`: 60 passed, 0 failed) |
| Gemini Interactions, stream and non-stream | pass | begin and response with `format: interactions`; thought summary and output text kept |
| Vertex non-stream | pass | Source hashed |
| Responses WebSocket merged / native / duplex | pass | 2 / 2 turns (the native turn used a prefix-delta base); duplex `input_seq` 0, 1, 2 on one RequestID. Each WebSocket completion is also checked to be `succeeded`, although the client closes the socket right after the last `response.completed` (the duplex RequestID's three responses `completed`). That check was added later and passed in a rerun of the `main` and `duplex` phases (`SMOKE_ONLY=main,duplex`: 61 passed, 0 failed); with the plugin's correction removed, the duplex completion was `canceled` |
| Native Codex fixture, empty `response.completed.output` | pass | output rebuilt from `output_item.done` |
| Upstream 500 then a second auth | pass | 2 attempts and 2 usage records, one failed with a masked failure body |
| Mid-stream error | pass | completion `failed` |
| Canceled before and after the first chunk | pass | both completions `canceled` |
| Bootstrap retry with two header-inits | pass | 2 upstream attempts; the one stored response has `bootstrap_resets` 1, terminal status `completed`; completion `succeeded`. How the case makes CPA call header-init twice is in [development.md](development.md#native-acceptance) |
| Rejected by a higher-priority test plugin | pass | completion `rejected`, `begin_written=false` |
| Model-list GETs | pass | `ignored_model_list` 4, no envelope |
| 10 MB and 50 MB prompts, text and base64 image | pass | no drops |
| 8 concurrent 8.5 MB + one 50 MB | pass | no drop (`body_budget` 0) |
| Long multi-response WebSocket stream | pass | 40 responses on one socket |
| Peak plugin `runtime.sys` strictly under the configured limit | pass | limit 768 MiB (as the status route reported it); peak 530 MiB over 72 status polls, heartbeat peak 48 MiB. 10 MB prompts peaked at 153 MiB, the 50 MB prompts at 477 MiB. `runtime.sys` is `/memory/classes/total:bytes`, which also counts heap released to the OS |
| Blackholed archive | pass | heartbeats reached the fake archive, then every archive route was held for 70 s while 36 requests were served: 0 heartbeats during the hold, more after the release, outbox pending 180. Request p50 was 44.5 ms with the archive reachable and 44.5 ms with it held; `before_auth` hook p50 stayed at 128 µs (p99 4096 µs both ways, recorded). An unroutable address was also served |
| Per-chunk latency with the writer saturated | recorded | client chunk gap p99 idle: 3.51 ms without the plugin, 4.25 ms with it; under the same load (three 4 MiB bodies in flight): 39.86 ms without, 46.94 ms with; the plugin's `stream_chunk` hook p99 was 128 µs |
| Time to first byte, 1 MB and 8.5 MB prompts, writer saturated (A/B) | recorded | added later and run in a rerun of the `latency` phase only (`SMOKE_ONLY=latency`: 3 passed, 0 failed). 20 streaming requests per size while three 4 MiB bodies were in flight, p50/p99: 1 MB 53.2/144.3 ms without the plugin, 107.3/169.3 ms with it; 8.5 MB 309.5/468.5 ms without, 497.2/674.6 ms with. The plugin's `hook_micros` per completion (all sync hooks of one request) was 4,665 µs p50 and 36,206 µs p99 over 50 completions; its `before_auth` hook p99 was 32,768 µs. The same rerun's chunk gap p99 under load was 42.60 ms without the plugin and 57.14 ms with it |
| Image generation, video create and poll (media fetch), speech, PDF | pass | 2 media fetches reached the fake archive; the stored response holds only the stripped URL |
| Thinking per format: Claude, OpenAI chat, Responses (HTTP and WebSocket), Gemini, Interactions | pass | 8 signature fields absent with their sha256 present; thinking text kept |
| Outbox while CPA runs (live copy) | pass | the copy's `-wal` held 1,062,992 bytes; secrets absent raw and decoded (WAL frames included), markers present |
| Reload: new version file while a stream is open | pass | the stream finished; the 9.9.9 instance is serving; no panic |
| Restart, no duplicate envelope | pass | 144 to 154 envelopes, no key received twice |
| Clean exit 0, `integrity_check`, modes 0700/0600 | pass | |
| nativefixture status | pass | 23 records holding hook names, counts and reason codes only |
| Fuse smoke | pass | run 1 counted 3 before-auth calls; with the fuse set, CPA served 5+3 requests, logged the fuse, and 2 heartbeats arrived with `calls.before_auth` frozen at 0 |

## Canaries

The canary ids below are the labels defined in `scripts/native/canaries.py`: the letter says where a canary is planted (H header, U usage record or upstream credential, R response body, S signature, M media URL, P body secret, E exact-registered value).

Secrets were absent from uploads, stored pieces, heartbeats, the outbox (live copy and after the stop, raw and decoded), management and public responses and the nativefixture status, in every encoding. The canaries covered: H1 to H8, U1, U2, U3, U7, U8, U10, U11, U12 (upstream keys), R1 to R3, S1, S2 (the two Interactions signatures included), M1 (allowed only in the media-fetch body), cookies, 22 body patterns and E1 to E5. That is 52 secrets in total. Ordinary markers were present in uploads and the decoded outbox, and absent from management output, the public shell, status and the CPA log. Placeholders were present for all 13 rule ids that fired. `X-Archive-*` never reached the mock upstream. CPA v8.0.60 itself writes upstream error text to its log (R3, U8 and the failure marker), with or without the plugin, so those hits are attributed to CPA.

U4 (plugin-executor Source), U5 (OAuth without email), U6 (Source equal to the runtime email or project, kept clear) and U9 (an AuthID path that holds an email) are not run natively, because they need OAuth credential files the mock cannot back. `internal/hooks/usage_test.go` (`TestUsageSourceRules`) covers each of them, plus a mismatched Source and an uncached AuthIndex (hashed, and queued for lookup).
