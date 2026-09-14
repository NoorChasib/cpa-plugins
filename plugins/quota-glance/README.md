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

**Enable Quota Cache first, and let it poll once.** Quota Glance refuses to
start when `cache-path` does not yet exist, so enabling both in the same edit
can leave Quota Glance inactive until the next restart — by which time Quota
Cache has written its snapshot and it starts normally. The CPA log names the
cause.

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
      stale-after: 45m
```

`cache-path` must match Quota Cache's own. Quota Glance refuses to start if it
cannot read that file, rather than serving an empty page that looks like a
working install with no quota anywhere.

**There is nothing else to configure and no password to set.** The dashboard
authenticates as whoever is signed in to the CPA management console, so once the
plugin is enabled the **Quota Glance** entry in the console sidebar opens it.

## Routes

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/quota-glance/app` | none | The page. Contains no data. |
| `GET /v0/management/plugins/quota-glance/summary` | CPA management key | The document the page renders. Supports `If-None-Match`. |
| `GET /v0/management/plugins/quota-glance/health` | CPA management key | Snapshot time, watcher state, last error. |
| `GET /v0/management/plugins/quota-glance/windows` | CPA management key | Observed window keys and which credentials report them. |

**This plugin holds no credential of its own.** Only the page is public, and it
carries no data; every byte of quota sits behind the management key CPA already
checks. The page recovers that key from the console's own browser storage — the
documented arrangement for a plugin page served from the console's origin, and
the same one Quota Cache's status view uses — so there is no second secret to
mint, log, rotate, or leak, and nothing to brute force.

Resource routes return 404 while CPA's built-in home page is enabled. The plugin
cannot read that setting, so if `/app` 404s, that is the first thing to check.

**There is deliberately no refresh route, and no refresh button.** Nothing here
can make data fresher; Quota Cache owns the schedule. The document reports
`observedAtEpoch` and `nextAttemptEpoch`, and the dashboard prints them as
"observed 4m ago · next attempt in 11m", instead of a control that would lie.

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

## The dashboard

`GET /v0/resource/plugins/quota-glance/app` serves one self-contained HTML
document — the React app under `web/`, built with JS and CSS inlined. It is one
file because CPA resource routes are matched on the exact path and accept only
GET: a directory of hashed assets would need a route registered per file.

It makes **no external request of any kind**. No CDN fonts, no icon service, no
analytics — system font stacks and inline SVG. That is not only about weight:
the token arrives in the page's own URL on first visit, so a single third-party
subresource would carry that URL out in a `Referer` header. Two Go tests hold
the line, failing the build on a subresource, a `url()`, an `@import`, or any
address or token that finds its way into the document.

How it signs in, in full: the CPA management console persists its management key
in browser storage, behind a documented reversible obfuscation. This page is
served from the console's own origin, so it recovers that key and presents it to
the management route above — which CPA authenticates before this plugin sees the
request. There is no host to configure either, because the plugin serves the
page and the page calls its own origin.

So there is no sign-in, from the sidebar or from the direct URL
`https://<your-host>/v0/resource/plugins/quota-glance/app`. The one case that
needs anything is a browser that has never signed in to the console — a phone,
say — which sees "Sign in to CPA first" and a link to the console, rather than a
password prompt this plugin could not honour anyway.

Everything the page shows is precomputed here: percentages, levels, ordering,
trend, and the wording of each card's subtitle. The only arithmetic in the
browser is `resetAtEpoch − now`, to tick the countdowns.

## Development

```sh
cd plugins/quota-glance
make ci       # gofmt, vet, bundle freshness, race tests, and the c-shared build
make golden   # regenerate both contracts under testdata/golden/
make smoke    # load the built library into a disposable CPA container (needs docker)

make web      # rebuild web/dist/index.html — commit the result
make web-dev  # dev server on :5173 against the golden fixtures, no CPA needed
```

`web/dist/index.html` is a build artifact that lives in git, because `go build`
embeds it and must never need Node. That arrangement is exactly how a stale
bundle ships silently, so `make ci` rebuilds the app into a scratch directory
and fails if the result differs from the committed file. Edit `web/src/`, run
`make web`, commit both.

`make web-dev` serves the committed golden documents through a stand-in for the
summary route, so the dashboard can be worked on with nothing else running.
There is no console in front of it, so seed a key once in the browser console:

```js
localStorage.setItem('cli-proxy-auth', JSON.stringify({state: {managementKey: 'dev'}}))
```

`?scenario=` then selects a state to look at —
`degraded`, `stale-cache`, `stale-schema`, `never-observed`, `empty`,
`future-schema`, `down`, `unauthorized`. Point `QUOTA_GLANCE_PROXY` at a real
CPA host to develop against live data instead. `web/design/mockup.html` is the
approved design the app is built to match.

`make smoke` is the only check that leaves the process. Everything else calls
the plugin in-process, which cannot tell you the shared library loads, that CPA
accepts the ABI handshake, that the routes are registered where you expect, or
that the plugin unloads without wedging the proxy. It drives the real routes
against a pinned CPA image with synthetic credentials and no provider requests.

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
