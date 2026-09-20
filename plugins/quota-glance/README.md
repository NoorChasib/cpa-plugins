# Quota Glance

One page showing how much capacity is left across every credential and every
rate-limit window, and when more arrives.

Quota Glance makes **no scheduled provider requests and no CPA management-API
calls**. It reads the snapshot [Quota Cache](../quota-cache/README.md) already
writes and serves it as one aggregated document. Quota Cache remains the only
component that *polls* a provider; a second poller would compete for the same
rate limits it exists to protect.

There is one exception, and it is a write rather than a poll: **using a banked
Codex rate-limit reset**. That cannot come out of a snapshot, so when you press
the button and confirm, this plugin spends the credit itself. It happens on that
one request and on no timer — see [Banked resets](#banked-resets), or set
`allow-redeem: false` to show the count without the button.

## Requirements

- Quota Cache installed and polling, with a snapshot on disk.
- Quota Cache **0.1.8 or newer** for banked Codex rate-limit resets. The count
  and its expiry are fields 0.1.8 added to the snapshot, so against anything
  older the **Banked resets** block simply never appears — the rest of the
  dashboard is unaffected, and it starts appearing on its own once Quota Cache
  is updated. Nothing to reconfigure.
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
      allow-redeem: true
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
| `POST /v0/management/plugins/quota-glance/redeem` | CPA management key | Spend one banked Codex rate-limit reset. |
| `POST /v0/resource/plugins/quota-glance/redeem` | `Authorization: Bearer <web-token>` | The same action, for a reader with no console session. |

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

## Banked resets

Codex banks **rate-limit resets**: entitlements already granted to your account
that clear its current windows when you spend one. A credential holding at least
one gets a **Banked resets** block above its window cards, with the count and
when the soonest one lapses. A credential holding none shows nothing at all —
no badge, no zero, no empty row.

Both halves cost what they should. The count rides along on the usage response
Quota Cache already fetches, so knowing it costs no request. The expiry needs a
second endpoint, so Quota Cache asks for it **only when the count is non-zero** —
an account with nothing banked is still exactly one request per poll. That
matters because the deadline is the part people lose: a banked reset expires
thirty days after it is granted, and a count with no date beside it is the shape
in which they quietly lapse.

**Using one** opens a confirmation first, and the sentence it leads with is the
one that matters: a banked reset is not extra allowance. Spending it restores
that account's session and weekly Codex windows and moves its weekly reset date,
it brings your existing allowance forward rather than adding to it, and **it
cannot be undone**. Confirm and the plugin reads your credits, spends the one
closest to expiring, and reports what happened.

A few things the design refuses to guess about:

- **The count on the card does not drop straight away.** It comes from Quota
  Cache's snapshot, which owns the schedule, so it corrects at the next poll —
  and the message after a redemption says so rather than letting the page
  disagree with itself.
- **A request that never completed is not a request that did nothing.** If the
  connection drops mid-flight the credit may already be spent, so the dashboard
  says the outcome is unknown and points you at your Codex usage instead of
  offering a retry that could cost a second one.
- **Two presses cannot become two credits.** A second attempt against the same
  credential while the first is in flight is refused outright.

The button is absent — not greyed out — whenever it cannot work: with
`allow-redeem: false`, on a credential CPA has parked or disabled, and on an
API-key login, which has no reset credits at all. The count still shows in every
one of those cases.

**Redeeming needs a CPA console session.** The route is registered on the
`web-token` door too, but CPA dispatches only GET to a resource route — `make
smoke` drives a real CPA and reports a 404 there — so a browser signed in with
only the fallback password shows the count and the expiry, and says to sign in
to the console instead of offering a button that cannot work. If CPA ever
dispatches POST to resource routes, the door opens with no change here, and the
smoke output says which behaviour is live.

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

**Cards fold.** Click any window card's header — the whole header is the
control — and its credential rows collapse away, leaving the title, the trend
mark, the big percentage and the server's subtext. That is deliberate: folding a
card should hide the detail, not the headline, so a folded page still answers
"how much is left and when does more arrive" at a glance.

Which cards you folded is remembered in that browser, under
`quota-glance.collapsed` in local storage. It is a preference about how one
person reads the page rather than a fact about anyone's quota, so it never
enters the document — the plugin still serves the same bytes to every reader.
Cards are keyed by provider and row, so folding Claude's **Session** leaves
Codex's alone.

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
