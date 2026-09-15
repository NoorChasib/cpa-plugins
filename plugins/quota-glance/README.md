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
- Quota Cache **0.1.6 or newer** for the full session / weekly / weekly-Fable
  layout. Anthropic moved model-scoped allowances into a structured `limits[]`
  array and nulled the flat keys that carried them; 0.1.6 reads both, so
  anything older shows no Fable card and an empty plan badge however healthy the
  credential is. Against an older snapshot Quota Glance renders what it is
  given and fills in the rest on its own once Quota Cache supplies canonical
  windows — no reconfiguration needed.

**Enable Quota Cache first, and let it poll once.** Quota Glance refuses to
start when `cache-path` does not yet exist, so enabling both in the same edit
can leave Quota Glance inactive until the next restart — by which time Quota
Cache has written its snapshot and it starts normally. The CPA log names the
cause.

## Install

Add `https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json`
as your store source, then install **Quota Glance**. If you already use
`preview/registry.json`, keep that source and select **Update** for this plugin;
both catalogs advance together. Follow any disable/restart instruction CPA gives
for a loaded native library.

Then merge [`config.example.yaml`](config.example.yaml) into your plugin
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

**From the CPA console there is nothing to set.** The dashboard authenticates as
whoever is signed in to the console, so once the plugin is enabled the **Quota
Glance** entry in the sidebar opens it — no password, no prompt.

`web-token` is the fallback, for a browser with no console session: a phone, a
bookmark, a machine that has never signed in. Set it to a password of your
choosing, or leave it empty and one is generated and printed **once** in the CPA
log at startup, then persisted under `data-dir` so restarts keep it. Either way
it is typed in once per browser and saved there.

## Routes

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/quota-glance/app` | none | The page. Contains no data. |
| `GET /v0/management/plugins/quota-glance/summary` | CPA management key | The document the page renders. Supports `If-None-Match`. |
| `GET /v0/resource/plugins/quota-glance/summary` | `Authorization: Bearer <web-token>` | The same document, for a reader with no console session. |
| `GET /v0/management/plugins/quota-glance/health` | CPA management key | Snapshot time, watcher state, last error. |
| `GET /v0/management/plugins/quota-glance/windows` | CPA management key | Observed window keys and which credentials report them. |

**One document, two doors.** From the console the page spends the session that
is already there: it recovers the management key from the console's own browser
storage — the documented arrangement for a plugin page served from the console's
origin, and the same one Quota Cache's status view uses — and CPA checks it. That
door needs no sign-in and is the one the sidebar uses.

The other door exists because CPA authenticates nothing on a resource route, so
a reader arriving without a console session has no session to spend. That path
carries `web-token`, compared in constant time against a stored SHA-256. Failed
attempts are counted globally rather than per caller — the ABI hands the plugin
only headers the caller chose, so any key taken from them is rotated, and forged,
trivially. A correct token is never throttled, so no volume of hostile traffic
can lock you out of your own dashboard; a wrong one costs a `429` for the rest
of the minute and never a ban.

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

Each credential also carries **which of them CPA is actually routing to**, as a
strip of request counts under its address. A credential sitting at 100% is
either keeping up with the traffic or taking none of it, and those are opposite
facts that look identical on a capacity bar. The counts are CPA's own — 20
buckets of 10 minutes, the last 3h20m, read from the credential roster the
plugin already asks for — so this costs no provider request, and the strip is
absent rather than empty on a CPA that does not report them. Because that
counter moves while the snapshot sits still, the document is rebuilt once a
minute as well as on every Quota Cache write; `.../health` counts those rebuilds
separately as `heartbeats`.

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

How it signs in, in full: the page tries the console's management key first,
recovering it from browser storage on the console's own origin, and falls back
to a saved `web-token` only if that key is absent or refused. So the sidebar and
the direct URL `https://<your-host>/v0/resource/plugins/quota-glance/app` both
open straight onto the dashboard on any browser that has signed in to the
console.

Anywhere else, the sign-in screen offers both doors: a link to the console, and
a password field. Whichever you use is saved in that browser, so it is asked
once. There is no host to configure either, because the plugin serves the page
and the page calls its own origin.

Everything the page shows is precomputed here: percentages, levels, ordering,
trend, the ink level of every block in an activity strip, and the wording of
each card's subtitle. The only arithmetic in the browser is subtracting an
instant from now — to tick the countdowns, and to age the last request under an
address.

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
There is no console in front of it, so either type `dev-token` into the password
field, or seed a console session once in the browser console:

```js
localStorage.setItem('cli-proxy-auth', JSON.stringify({state: {managementKey: 'dev'}}))
```

`?scenario=` then selects a state to look at —
`degraded`, `stale-cache`, `stale-schema`, `never-observed`, `empty`,
`future-schema`, `down`, `unauthorized`, `cpa-expired` (CPA refuses, so the
password fallback takes over). Point `QUOTA_GLANCE_PROXY` at a real
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
