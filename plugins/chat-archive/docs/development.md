# Development

Run everything from `plugins/chat-archive`. On a shared host, keep the footprint small: `GOMAXPROCS=4`, `go test -p 4`, `nice -n 10`.

## Build and unit gates

| Target | What it does |
|---|---|
| `make ci` | `fmt-check vet test test-race build c-shared glibc-check pure-check` |
| `make c-shared` | `dist/chat-archive.so` (`CGO_ENABLED=1`, `-trimpath`, `-s -w`, version stamped) |
| `make glibc-check` | fails if the library needs a glibc symbol newer than 2.36 (CPA's image is Debian 12) |
| `make pure-check` | `pkg/` and the importer build without cgo for darwin/arm64 and linux/amd64; `pkg/` imports neither mattn/go-sqlite3 nor `internal/` |
| `make importer-dist` | `dist/chat-archive-import_linux_amd64` and `_darwin_arm64` |
| `make test` | Go tests plus `python3 -m unittest discover -s scripts/tests` |

The sidebar tests in `internal/plugin/status_page_test.go` run the served script in Node's `vm` (console session codecs, 401/403 stop, counters-only rendering); they need `node` and skip without it outside CI.

## Browser gate

`make browser-tools` installs the pinned test tools under `scripts/browser/` (Node 26.8.1 from `install-node.sh`, npm 11.19.0, agent-browser 0.37.1, Chrome for Testing 153.0.8010.36, all checksum-verified). `make browser` runs `TestSidebarBrowser`: the shipped page in a real browser against a synthetic loopback console (remembered session renders counters only; a 401 stops after one request; no session makes no request).

## Native acceptance

All native runs use synthetic data, loopback networking and throwaway containers. None reads an operator config, auth file or log, and none calls a production CPA.

| Target | What it runs |
|---|---|
| `make smoke` | `scripts/native/run-smoke.sh`: builds CPA v8.0.60 (commit `91e0dee827abf476f9f86246f68c500a112f7155`, codeload archive sha256 `21e2a431...5e23`), the production library, a 9.9.9 reload library, the nativefixture library, the reject-fixture test plugin (`rejectplugin.c`) and the fake archive (`scripts/native/fakearchive`, wrapping `pkg/client/clienttest`); then, in a `--network none --read-only --cap-drop ALL` Python container: `abi_probe.py` on both libraries and `acceptance.py` (all phases) |
| `make native-spike` | the ABI probes and the nativefixture phase only |
| `make fuse-smoke` | `scripts/native/fuse-smoke.sh`: applies `fuse-v8.0.60.patch` to a copy of the pinned source (fails loudly if it no longer applies), then `fuse.py` |
| `make store-smoke CPA_SMOKE_WORK=<smoke dir>` | `scripts/native/store_smoke.py`: the official `eceasy/cli-proxy-api@sha256:873ec3df...dc0d` image on an `--internal` network, Plugin Store install of the smoke-tested library beside every published peer (downloaded with catalog sha256 checks), configure, capture, default data path and modes |

Useful variables: `CPA_SOURCE_DIR` (a local checkout of the pinned commit, copied with `git archive`, never modified), `CPA_SOURCE_ARCHIVE` (a downloaded codeload archive), `SMOKE_ONLY=main,duplex,fixture,memory,latency,blackhole` (phases), `SMOKE_CPUS` and `SMOKE_MEMORY` (container limits, default 2 and 3g), `SMOKE_LABEL` (container name `ca-test-<label>-…`, default `smoke`).

The acceptance phases:

- `main`: every format stream and non-stream (Claude, OpenAI chat, Responses via Codex, Gemini, Vertex, Gemini Interactions), the native Codex fixture with an empty `response.completed.output`, failover to a second auth, mid-stream error, cancellation before and after the first chunk, bootstrap retry (see below), rejection by a higher-priority plugin, Responses WebSocket merged and native (every turn's completion `succeeded`, although the client closes the socket right after the last `response.completed`), image, video (media fetch), speech and PDF, model-list GETs, the request, response and usage canaries in every encoding, the CPA log and management responses, reload with a new version file while a stream is open, a live copy of the outbox (with its non-empty `-wal`) scanned raw and decoded while CPA runs, clean exit, outbox scan and modes, restart without duplicates.
- `duplex`: `upstream.codex.response-steering: true`, three turns on one socket, `input_seq` in call order, every response `completed` and the RequestID's completion `succeeded` (CPA reports the client's close as `canceled`).
- `fixture`: the nativefixture library's status trace holds hook names, counts and reason codes only.
- `memory`: 10 MB and 50 MB prompts (text and base64 image), 8 concurrent 8.5 MB plus one 50 MB, a 40-response WebSocket stream. The plugin runs with the default `memory-limit-mib` (768), and its peak `runtime.sys` must stay strictly below the limit the status route reports.
- `latency`: client chunk gaps idle and under the same load, with and without the plugin; time to first byte of streaming requests with 1 MB and 8.5 MB prompts under that load (three 4 MiB bodies in flight, which keeps the plugin's writer saturated), with and without the plugin, as p50/p99; and the plugin's `hook_micros` per completion (all sync hooks of one request) as p50/p99.
- `blackhole`: heartbeats reach the fake archive, then every archive route is held for longer than one heartbeat interval while CPA serves requests (no heartbeat arrives, the outbox grows, request and hook latency stay within the stated bound of the reachable baseline, and heartbeats resume after the release); plus an unroutable archive address.

The bootstrap retry case needs CPA to call the stream header-init hook twice for one request. CPA calls header-init lazily, with the first payload chunk, so an upstream that fails before its first byte gives only one header-init: CPA turns that failure into a stream whose first chunk is the error and retries before any payload. The case therefore uses CPA's own Responses SSE validator. With `requests.streaming.bootstrap-retries: 1`, the mock Codex upstream's first attempt sends an event whose JSON is cut short and then a `response.failed` event. CPA passes the cut event to the plugins (header-init, then chunk 0), holds it back from the client because it is not valid JSON, sees the failure before any deliverable byte, and runs the stream again, which calls header-init a second time. The check asserts two upstream attempts, `bootstrap_resets` 1 on the one stored response, a completed response and a succeeded completion.

Canary values are generated per run (`scripts/native/canaries.py`); `fakearchive maskcheck` confirms each body canary is one mask/v1 catches before the run plants it. Output holds canary ids, counts, sizes and timings only.

## Release helpers

`scripts/release.py package|checksums|verify|validate-library` (local only; rejects nativefixture builds), `scripts/check-candidate.py` (every version binding equals `registry.json`), `scripts/verify-cpa-package.py --cpa-source <dir>` (both registry contracts through CPA v8's `sdk/pluginstore`, offline), `scripts/native/release_evidence.py` (writes `dist/chat-archive-evidence.json` for the root packager).
