# Quota Glance

One page showing how much capacity is left across every credential and every
rate-limit window, and when more arrives.

Quota Glance makes **no provider requests and no CPA management-API calls**. It
reads the snapshot [Quota Cache](../quota-cache/README.md) already writes and
serves it as one aggregated document. Quota Cache remains the only component
that ever contacts a provider; a second poller would compete for the same rate
limits it exists to protect.

## Requirements

- Quota Cache installed and polling, with a snapshot on disk.
- Quota Cache **0.1.5 or newer** for the full session / weekly / weekly-Fable
  layout. Against an older snapshot, Quota Glance shows a single weekly row per
  credential and fills in the rest on its own once Quota Cache supplies
  canonical windows — no reconfiguration needed.

## Install

Merge [`config.example.yaml`](config.example.yaml) into your plugin
configuration, keeping your existing options:

```yaml
plugins:
  enabled: true
  configs:
    quota-glance:
      enabled: true
      cache-path: /CLIProxyAPI/plugins/data/quota-cache/snapshot.json
      data-dir: /CLIProxyAPI/plugins/data/quota-glance
      web-token: ""
      stale-after: 45m
```

`cache-path` must match Quota Cache's own. Quota Glance refuses to start if it
cannot read that file, rather than serving an empty page that looks like a
working install with no quota anywhere.

Leave `web-token` empty and the plugin generates one, logs it **once**, and
stores it under `data-dir` so restarts keep the same token and a bookmarked
dashboard URL keeps working. Copying it into the configuration is optional.

## Routes

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/quota-glance/app` | none | The page. Contains no data. |
| `GET /v0/resource/plugins/quota-glance/summary` | `Authorization: Bearer <web-token>` | The document the page renders. Supports `If-None-Match`. |
| `GET /v0/management/plugins/quota-glance/health` | CPA management key | Snapshot time, watcher state, last error. |
| `GET /v0/management/plugins/quota-glance/windows` | CPA management key | Observed window keys and which credentials report them. |

CPA runs no authentication on a resource route, so the summary route carries its
own: a bearer token compared in constant time against a stored SHA-256, rate
limited per client address. Exceeding the limit costs a `429` for the rest of
the minute and never a ban — a lockout on your own dashboard is a worse outcome
than a slow attack on a long random token.

Resource routes return 404 while CPA's built-in home page is enabled. The plugin
cannot read that setting, so if `/app` 404s, that is the first thing to check.

**There is deliberately no refresh route.** Nothing here can make data fresher;
Quota Cache owns the schedule. The document reports `observedAt` and
`nextAttempt` instead of a control that would lie.

## What it reports

Percentages in the document are **remaining** capacity, converted once from the
used percentage Quota Cache records. `level` is computed server-side —
`critical` below 20% remaining — so every client agrees without recomputing a
threshold in CSS.

Credentials are emitted sorted by their weekly reset, soonest first, and every
row repeats that order, so the top row of each card is always the credential
that recovers next. Clients do not re-sort.

A credential that did not report a window is **excluded** from that row's mean
rather than counted as full. Rows keep serving their last good figures with
`stale: true` and a reason when the snapshot goes missing or its schema stops
matching, because an empty response is indistinguishable from a broken install.

Plan names arrive display-ready: Claude's `Max` and `Team` pass through, and
Codex's plan enum is resolved to the tier name (`pro` is Pro 20x, `prolite` is
Pro 5x). Set `plan-labels` only if a provider renames a tier.

The full field reference — every status, state, data issue, level, trend, and
stale reason — is in [docs/summary-contract.md](docs/summary-contract.md).

## Development

```sh
cd plugins/quota-glance
make ci       # gofmt, vet, race tests, and the c-shared build
make golden   # regenerate both contracts under testdata/golden/
```

`testdata/golden/` holds the two contracts the web app develops against:
`summary.json` (healthy) and `summary-degraded.json` (every degraded state).
Both are byte-identical to what the summary route serves, and CI fails if a
build stops reproducing them. Regenerating is a deliberate contract change —
review the diff.

To exercise the routes without a CPA instance:

```sh
go run ./tools/fixtureserve -token dev-token
curl -s -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8787/v0/resource/plugins/quota-glance/summary
```
