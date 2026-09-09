# CPA native compatibility and telemetry boundary

Verified 2026-09-09. This is the maintained compatibility contract for Token Usage, not a deployment verification or a promise of billing-grade accuracy.

## Target and provenance

| Item | Verified target |
|---|---|
| CPA compatibility floor | **v7.2.155**, commit **`7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`** |
| Upstream check | Official `HEAD` and `refs/tags/v7.2.155` both resolved to that commit; official latest-release metadata reported publication `2026-09-08T17:37:47Z` |
| Deployed CPA | **Unknown; not inspected.** Older/newer releases are not implied to behave identically |
| Native contract | ABI **1**, RPC schema **6**, `cliproxy_plugin_init`, Go `c-shared` |
| Tested platform | **Linux amd64 only**, CGO enabled, GCC 15.2.0, Go 1.27.1; module language floor Go 1.26.0 |
| Native baseline | `cpa-plugin-account-health-pushover` release 0.4.0, commit `870456ecdbf3a86c76c6274f1d02e14dadddabf4` |
| Runtime isolation | Official Python container `python@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285`, `--platform linux/amd64 --network none`; **not** a prebuilt CPA image |

Sources: [official release][release], [official release metadata](https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest), [pinned CPA tree][cpa], [baseline tree][baseline]. The checks used `git ls-remote` and the official GitHub API. All CPA code used for execution was compiled, unmodified, from the pinned archive outside this repository. No provider credentials, existing CPA configuration, auth files, or production state were read.

The baseline native ABI structure was adapted selectively. Health classification, auth access, quota polling, notifications, and Pushover code were not copied. `LICENSE` retains `Copyright (c) 2026 NoorChasib` and CPA's published MIT notice for the mirrored protocol declarations, including its original date text. See [baseline license](https://github.com/NoorChasib/cpa-plugin-account-health-pushover/blob/870456ecdbf3a86c76c6274f1d02e14dadddabf4/LICENSE) and [CPA license](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/LICENSE).

CPA requires nonempty name, version, author **and GitHubRepository** in metadata. The intended project URL is therefore an identity field only: its presence is not a claim that a remote, store entry, or release exists. See [`validPlugin`](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/host.go#L1025-L1037).

## ABI and schema 6 decision

- Capabilities are only `usage_plugin` and `management_api`.
- `plugin.register`/`plugin.reconfigure` receive a base64-encoded `config_yaml` byte slice and `schema_version`. This implementation refuses a host schema below 6 rather than claiming an untested legacy path.
- Usage method is `usage.handle`; usage fields are **PascalCase**. It has no request-scoped `host_callback_id`.
- Management `Body` is still a **base64-encoded byte slice on the RPC wire**. Schema 6 changes what CPA does after decoding: JSON strings are no longer rewritten into HTML entities. It does not mean the plugin should change `Body` to an embedded JSON object.
- Schemas 3 and 5 change request-body/history inclusion on stream interceptor chunks; schema 4 adds WebSocket observation. Those changes do not affect these two capabilities. No interceptor or WebSocket capability is advertised.
- Schema 6 returns JSON, not HTML-safe text. Any future renderer must escape untrusted strings itself. The native fixture verifies the parsed JSON string `<model>&literal` survives unchanged through CPA.
- No host callback bridge is retained. Token collection does not need auth lookup, provider HTTP access, host logging, or a host pointer. The inspected ABI has **no `host.usage.*` snapshot/replay method**.
- C request size is bounded before conversion to `C.int`/`C.GoBytes`; response buffers belong to the plugin and are freed with its exported free function. Shutdown excludes/drains admitted plugin calls and joins plugin-owned collection work before returning. The host remains responsible for not invoking freed native function pointers.

Sources: [ABI/schema constants and host methods][abi], [schema negotiation][negotiation], [usage and management RPC dispatch][rpc], [management response contract][management-types], [schema-dependent body handling][management].

### Private management routes

The route namespace is `/v0/management/plugins/token-usage`. The current implementation registers `/plugins/token-usage/status`, `/plugins/token-usage/summary` and `/plugins/token-usage/models`. **Leave every private GET's `Menu` empty and register no public resources.** A nonempty menu on a management GET makes CPA treat it as a legacy **public resource**.

CPA authenticates management fallback routes before dispatch, but does not authenticate resource routes. The native fixtures confirmed missing and invalid management credentials are rejected (401/403), both tested resource paths return 404, and the authenticated status body is readable. This is real HTTP verification, not only a registration assertion.

Sources: [management auth/resource split][auth], [Menu conversion][menu].

## Narrow usage contract and accounting interpretation

Retain only provider, executor type, execution model, alias, requested time, generation flag, failure flag/status, and these seven signed 64-bit raw counters:

`InputTokens`, `OutputTokens`, `TotalTokens`, `ReasoningTokens`, `CachedTokens`, `CacheReadTokens`, `CacheCreationTokens`.

Never persist the full native JSON. `APIKey` can be the actual client key; `Source` can be an email or upstream key. Auth IDs/indexes, sessions, headers, failure bodies and other unused metadata are excluded. The successful authenticated fixture response was checked for planted client/upstream/management/header/failure-body canaries. This is a plugin-output check, not a promise that CPA's own error logging redacts every upstream payload.

The native type/adapter **do not expose** the internal `TokenBreakdown`, its version or quality, a stable request/event ID, `Stream`, response service tier, or original provider token-field presence. Missing provider usage commonly becomes explicit zeros before the native callback, and sometimes no callback is emitted at all. Internal accounting schema 2 is separate from native RPC schema 6 and must not be claimed as native accounting quality.

Sources: [native usage types][usage-types], [internal-to-native adapter][adapter], [sensitive source attribution][source-attribution], [internal canonical accounting][accounting].

At the tested pin:

- `Provider` is runtime data. The fixture configured `fixture-openai`, but native attribution was **`openai-compatible-fixture-openai`**, with executor `OpenAICompatExecutor`. Claude attribution was `claude` / `ClaudeExecutor`. Do not strip provider prefixes silently.
- `Model` came from CPA's execution request, not the mock response's `physical-revision-not-attribution` string. `Alias` preserved the client's requested `alias-*` name.
- `RequestedAt` is set when CPA constructs the executor usage reporter, not a proven client-arrival timestamp. Persistent events also retain a separate local receipt time.
- `Generate` was true for every fixture. The adapter normalizes an omitted internal generation flag to true; this does not justify inventing original provider metadata.
- Latency and TTFT, if added later, are Go duration numbers rather than milliseconds.

Sources: [reporter construction][reporter], [provider prefix](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/util/provider.go#L15), [adapter][adapter], [native types][usage-types].

### Counter semantics are not additive

| Parser family | Input/cache interpretation | Output/reasoning interpretation |
|---|---|---|
| OpenAI-style/shared Codex parser | Cache read/write counters are subsets of reported input | Reasoning is a subset of output |
| Claude | Cache read/write are independent of ordinary input; legacy cached falls back to creation if reads are zero | Flat output already includes reported thinking/reasoning |
| Gemini family | Input includes cache and the parser adds tool-use prompt tokens | Flat output excludes separate reasoning |

These are **specific CPA parser semantics**, not a universal guarantee based on provider names. The spike runs OpenAI-compatible and Claude executors; Codex/Gemini statements are source findings only. Preserve raw values and provenance. Do not add all counters, normalize unknown executors, repair contradictions, or label totals as invoices.

Source: [pinned parser implementations][parsers].

## Real executor-to-native synthetic results

### Method

`bash scripts/native/run-spike.sh` obtains the immutable CPA archive (an explicit `CPA_SOURCE_ARCHIVE` copy or the pinned download), verifies its SHA-256, compiles the actual CPA server and both production/`nativefixture` shared libraries, runs native ABI checks, and launches CPA with fresh synthetic configuration. All runtime connections occur inside a **network-disabled container**: HTTP client → real CPA API/alias routing → built-in executor → local mock upstream → core usage manager → native usage adapter/RPC/C ABI → plugin → authenticated CPA status HTTP response.

The test-only `nativefixture` build exposes at most 64 narrowly decoded records in authenticated `/status`. It is **not a production artifact**. Its numeric raw fixture fields are for Python assertions; the persistent API uses decimal strings for large counts. Ordinary builds do not expose or retain the fixture record buffer.

The **original Phase A bootstrap on 2026-09-09** reported `storage: none_spike_only` and did not implement persistence. That is historical evidence, not the current runtime: rerunning the retained harness now uses the SQLite-backed plugin, supplies the required `database-path` in a fresh private directory, and reports `storage: sqlite`. The raw-record probe still tests the upstream contract, not durability; [separate production acceptance](#current-persistent-acceptance) verifies committed history and restart behavior.

Each row below originally ran twice on 2026-09-09: `usage-statistics-enabled: false` and `true`. **All 30 assertions passed.** In each run, 15 upstream executions produced 14 native observations: the Claude missing-top-level-usage case produced no native event. The built-in destructive usage queue had **0** items with the setting false and **14** with it true, while native observations were identical. The queue check reads only this disposable host's data.

Tuple order below is **(input, output, total, reasoning, legacy cached, cache read, cache creation)**. Every numeric expectation was asserted against the observation; upstream input and expected native output are kept distinct so known loss does not masquerade as accuracy.

| Case | Mock upstream usage | Expected and observed native result |
|---|---|---|
| OpenAI nonstream | `(100,20,120,3,30,30,5)` | One success, same tuple |
| OpenAI final stream usage | Same complete usage then `[DONE]` | One success, same tuple |
| OpenAI cumulative stream | Output 2 followed by final output 20 | One success with latest `(100,20,120,3,30,30,5)`; not a sum |
| OpenAI missing usage | Content and `[DONE]`, no usage object | One success, all seven counters **0** |
| OpenAI terminal stream error | Complete 100/20 usage buffered, then error event | One **failure**, status 502, all seven counters **0**; buffered tokens lost |
| OpenAI truncated upstream body | Complete usage followed by unexpected HTTP EOF | One **failure**, status 0, all counters **0**; buffered tokens lost |
| OpenAI clean EOF without `[DONE]` | Complete usage, correctly framed HTTP EOF | Chat-completions path reports success with complete tuple |
| OpenAI HTTP 400 | Error body, no usage | One failure, status 400, all counters 0 |
| Claude nonstream | Ordinary input 100, output 20 including thinking 3, read 30, write 5 | One success `(100,20,155,3,30,30,5)` |
| Claude creation-only nonstream | Same, but cache reads 0 | One success `(100,20,125,3,5,0,5)`; legacy cached equals writes |
| Claude full final top-level usage | Complete usage on `message_delta` | One success `(100,20,155,3,30,30,5)` |
| **Claude split usage** | Nested `message_start.message.usage`: input 100/read 30/write 5; top-level final usage: output 20 | **One success `(0,20,20,0,0,0,0)`; input and both cache counters absent** |
| **Claude cumulative top-level usage** | First complete usage output 2, later final output 20 | **One success `(100,2,137,0,30,30,5)`; later cumulative update ignored** |
| **Claude no top-level usage** | Nested initial usage, content and `message_stop`, no top-level usage | **No event**, not a zero-token event; checked again by later FIFO delivery and final event count |
| **Claude truncation after first usage** | First complete usage output 2, then unexpected HTTP EOF | **One success `(100,2,137,0,30,30,5)` despite stream failure**; earlier once-only publication prevents outcome correction |

The Claude parser checks only top-level `usage`, and the executor publishes immediately through a once-only reporter. OpenAI-compatible streaming buffers usage, but failure publication can win that same once-only reporter before the buffer is finalized. These mechanisms explain the observations; the plugin cannot reconstruct information that never reaches it.

Sources: [Claude stream parser][parsers], [Claude stream execution][claude-stream], [OpenAI-compatible stream execution][openai-stream], [once-only reporter/publication][publication].

### Reproduction and validation commands

```bash
# Run from this repository. Requires Linux amd64, Go/CGO, GCC, curl, tar,
# sha256sum and Docker. Downloads source, module dependencies and a pinned image.
go test ./...
go test -race ./...
go test -race -tags nativefixture ./...
go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/token-usage.so .
python3 scripts/native/abi_probe.py /tmp/token-usage.so
bash scripts/native/run-spike.sh
```

Validation completed for normal and fixture builds: unit tests, race tests, vet, production shared-library build, native init/ABI version/duplicate-init/buffer bounds/free/error sanitization/shutdown/reinit checks, real native loading, authenticated status, schema 6 preservation, and the 30 executor fixtures.

Source archive SHA-256: `3f829041f62175a546750520d1c160cba1a7618d30f17618731e250c048d77e5`. The runner fails if this changes; an upstream archive regeneration must be deliberately reverified, not silently accepted. The original Phase A runs used `/tmp/token-usage-spike.*`; the current shared runner prints a fresh `/tmp/token-usage-smoke.*` directory and retains synthetic `results.json` and CPA logs. Source, binaries and results are not vendored into this repository.

The first host-user launch failed because the machine's inotify watch quota was exhausted (`no space left on device`, despite over 850 GiB free). No sysctl or unrelated process was changed. A disposable container with a separate UID quota solved the issue. An initial hardened runner lacked write ownership of its temporary bind mount; the final runner explicitly owns only that mount during execution and restores host ownership on exit. A first fixture expectation assumed the configured OpenAI provider name was passed through; real execution and source inspection established the `openai-compatible-` prefix, which is now asserted.

### Current persistent acceptance

The subsequent production acceptance run on **2026-09-09** used the ordinary library, not the raw-record probe. [`scripts/native/production.py`](../scripts/native/production.py) exercised real pinned CPA executors through committed SQLite rows and authenticated `/status`, `/summary` and `/models`, then cleanly restarted against the same database. Both statistics settings passed, including provider/model separation, equal-looking repeat observations, exact counters above 2^53, private-route authentication and sensitive-canary checks. The direct ABI probe separately verified failed nonzero counters and exact history after quiesce/reopen/native reinitialization.

Persistence, configuration, retention, storage-failure recovery and local packaging are implemented; see the [runtime contract](runtime-contract.md) and [native acceptance/packaging guide](development.md). `make smoke` runs both the retained Phase A expectations and this separate production suite. The original telemetry result alone was not persistence evidence, and a fresh full native rerun is still required after runtime changes.

## Delivery limits and work not claimed

The core usage manager uses an unbounded in-memory queue and serial sink dispatch. Native callback errors are debug-logged without replay/acknowledgement. Service shutdown disables/shuts down native plugins before stopping the usage manager; the manager's stop does not join the dispatcher. Correct local persistence cannot establish complete host delivery.

Sources: [usage manager][manager], [native usage RPC error handling][rpc], [service shutdown order][shutdown].

`usage-statistics-enabled` gates the built-in queue sink, **not** native plugin dispatch at this pin; both were tested. `/v0/management/usage-queue` destructively pops short-lived RAM items and is not history or replay. Sources: [queue sink][queue-sink], [queue handler][queue-handler], [usage plugin registration][adapter].

Still not tested or claimed by the synthetic native acceptance:

- Real providers, billable calls, deployed CPA version, production credentials/configuration, or provider billing reconciliation.
- Codex, Gemini, Antigravity, OAuth, WebSockets, Responses streaming, translation into a different client protocol, downstream client cancellation, retry/failover orchestration, or additional image-model events. Retry behavior and [Codex additional-model publication](https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/codex_executor_request.go#L482-L488) were inspected as source-only limitations. Retries can create multiple observations; no uniqueness claim is justified.
- Other platforms or older Go runtime validation. Local persistence/recovery tests and prepared packaging are separate from the original Phase A telemetry result and are documented in the [runtime contract](runtime-contract.md#completed-runtime-validation) and [development guide](development.md).
- A native executor case with nonzero *failed* tokens: these pinned executors emitted zero on the tested OpenAI failures and earlier success on the tested Claude truncation. The narrow decoder and direct native ABI tests separately verify failed nonzero counters, without pretending they came from the executor fixture.
- Missing-account delivery through configured CPA auth: unit and direct ABI tests accept events without account attribution, including identical repeated events; CPA's configured executor fixtures naturally used synthetic auth records.

## Persistent implementation rules

1. Preserve provider, executor type, model and alias separately. Group at least by provider/model. Label counts **CPA-reported** and **observed_events**, never unique requests, physical attempts or invoices.
2. The event decoder/collector performs narrow validation/admission and adds UTC receipt time, a local event ID and bounded anomaly flags under the documented identity/time policies. Keep signed 64-bit counters exact, reject invalid numeric values visibly, and preserve failed nonzero usage. Do not deduplicate equal-looking native callbacks.
3. Preserve every raw counter; no normalization engine is required. Track upstream completeness as **unknown even when local storage is healthy**, and surface the Claude streaming limitations prominently in status/docs.
4. The bounded queue and raw-only SQLite writer replace the original spike-only memory counter. Keep IDs stable through local write retries; perform exact aggregation and emit decimal strings in the authenticated API. No host calls are needed.
5. Keep the `nativefixture` probe isolated from production artifacts. Retain both the upstream-contract regression harness and the separate **production build executor → persistence → authenticated query → restart** acceptance suite. Passing the fixture build does not establish production durability.
6. Retain empty private `Menu`, no resource registrations, strict route dispatch, no raw error reflection, and lifecycle exclusion. `Shutdown()` must synchronously join every plugin-owned worker before the native ABI returns.

[cpa]: https://github.com/router-for-me/CLIProxyAPI/tree/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974
[baseline]: https://github.com/NoorChasib/cpa-plugin-account-health-pushover/tree/870456ecdbf3a86c76c6274f1d02e14dadddabf4
[release]: https://github.com/router-for-me/CLIProxyAPI/releases/tag/v7.2.155
[abi]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginabi/types.go#L5-L105
[negotiation]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/rpc_client.go#L58-L80
[rpc]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/rpc_client.go#L561-L603
[management-types]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginapi/types.go#L1347-L1357
[management]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/management.go#L255-L357
[menu]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/management.go#L156-L166
[auth]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/api/server_management.go#L236-L289
[usage-types]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/pluginapi/types.go#L1359-L1430
[adapter]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/pluginhost/adapters_usage_translation.go#L19-L198
[source-attribution]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L566-L624
[accounting]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/usage/accounting.go#L5-L78
[reporter]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L58-L104
[parsers]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L724-L978
[claude-stream]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/claude_executor_stream.go#L410-L478
[openai-stream]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/openai_compat_executor.go#L419-L568
[publication]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/runtime/executor/helps/usage_helpers.go#L344-L417
[manager]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/usage/manager.go#L250-L398
[shutdown]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/sdk/cliproxy/service_lifecycle.go#L319-L348
[queue-sink]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/redisqueue/plugin.go#L21-L27
[queue-handler]: https://github.com/router-for-me/CLIProxyAPI/blob/7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974/internal/api/handlers/management/usage.go#L23-L55
