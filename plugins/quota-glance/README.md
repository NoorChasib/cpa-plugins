# Quota Glance

One page showing how much capacity is left across every credential and every
rate-limit window, and when more arrives.

Quota Glance makes **no scheduled provider requests and no CPA management-API
calls**. It reads the snapshot [Quota Cache](../quota-cache/README.md) already
writes and serves it as one aggregated document. Quota Cache remains the only
component that *polls* a provider; a second poller would compete for the same
rate limits it exists to protect.

There is one exception, and it is a write rather than a poll: **using a banked
Codex or Claude rate-limit reset**. That cannot come out of a snapshot, so when
you press **Use one** and confirm, this plugin spends the reset itself — from
the CPA sidebar, a browser tab, a phone or the menu bar app alike. It happens on
that one request and on no timer — see [Banked resets](#banked-resets), or set
`allow-redeem: false` to show the count without the button.

## Requirements

- Quota Cache installed and polling, with a snapshot on disk.
- Quota Cache **0.1.10 or newer** for banked Claude resets, Claude plan names,
  Claude's estimated renewal date, Grok's subscription display name, and
  Codex's subscription renewal date.
  Against an older snapshot Claude's resets are absent and its plan badge reads
  "—", Grok shows the tier its usage response names, and Codex shows the
  spend-control reset as its renewal; each fills in on its own once Quota Cache
  is updated.
- Quota Cache **0.1.9 or newer**, with an OpenRouter management key, for the
  [OpenRouter balance](#openrouter-balance). Without either, the card is simply
  absent and nothing else changes.
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
for a loaded native library. After an update, reload any open dashboard tab and
use **Reload Page** in the menu bar app: a page already open keeps running the
version it loaded.

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
      openrouter-warn-below: 5
```

`cache-path` must match Quota Cache's own. Quota Glance refuses to start if it
cannot read that file, rather than serving an empty page that looks like a
working install with no quota anywhere.

**From the CPA console there is nothing to set.** The dashboard can use the
console's own session, so once the plugin is enabled the **Quota Glance** entry
in the sidebar opens it — no password, no prompt — as long as you ticked
**Remember password** when signing in to the console.

`web-token` is the dashboard password, for every other browser: a phone, a
bookmark, the menu bar app, a console signed in without Remember password. Set
it to a password of your choosing (at least 20 random characters), or leave it
empty and one is generated and printed **once** in the CPA log at startup, then
persisted under `data-dir` so restarts keep it. Either way it is typed in once
per browser and saved there, and a browser that has both uses the password.

It also spends banked resets (**Use one**) unless `allow-redeem: false`. Treat
it as a password: anyone holding it can view the dashboard and use a reset. If
it was ever shared with someone who should not have it, set `web-token` to a new
value, or set `allow-redeem: false`, which removes the button everywhere.

## Routes

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/quota-glance/app` | none | The page. Contains no data. |
| `GET /v0/management/plugins/quota-glance/summary` | CPA management key | The document the page renders. Supports `If-None-Match`. |
| `GET /v0/resource/plugins/quota-glance/summary` | `Authorization: Bearer <web-token>` | The same document, for a reader signed in with the dashboard password. |
| `GET /v0/management/plugins/quota-glance/health` | CPA management key | Snapshot time, watcher state, last error. |
| `GET /v0/management/plugins/quota-glance/windows` | CPA management key | Observed window keys and which credentials report them. |
| `POST /v0/management/plugins/quota-glance/redeem` | CPA management key | Spend one banked Codex or Claude rate-limit reset. |
| `GET /v0/resource/plugins/quota-glance/spend` | `Authorization: Bearer <web-token>` + `X-Quota-Glance-Spend` | Spend one banked reset, for a reader signed in with the dashboard password. |
| `POST /v0/resource/plugins/quota-glance/redeem` | `Authorization: Bearer <web-token>` | Registered; CPA v8.0.15 does not dispatch it. |

**One document, two doors, and both can spend.** The dashboard password goes to
the resource routes. CPA authenticates nothing there, so the plugin checks it
itself: compared in constant time against a stored SHA-256, with failed attempts
counted globally rather than per caller — the ABI hands the plugin only headers
the caller chose, so any key taken from them is rotated, and forged, trivially.
A correct token is never throttled, so no volume of hostile traffic can lock you
out of your own dashboard; wrong ones get a bare `401`, and past 20 in a minute
a `429` until the minute is up. Because none of this reaches CPA's management
API, a wrong or stale password **never counts toward CPA's IP ban**.

The console door spends the session that is already there: the page recovers
the management key from the console's own browser storage — the documented
arrangement for a plugin page served from the console's origin — and CPA checks
it on the management routes. That door needs no sign-in, and it is the one the
sidebar uses when no password is saved. It is also the one where a refusal
costs something: CPA locks an address out of its whole management API for 30
minutes after 5 failed management sign-ins. So the page presents a console key
only when it is sure the key is meant for this CPA, and stops presenting it,
in every tab, the moment CPA refuses it. [docs/access.md](docs/access.md) has
the rules.

Resource routes return 404 while CPA's built-in home page is enabled. The plugin
cannot read that setting, so if `/app` 404s, that is the first thing to check.

**There is deliberately no refresh route, and no refresh button.** Nothing here
can make data fresher; Quota Cache owns the schedule. The document reports
`observedAtEpoch` and `nextAttemptEpoch`, and the dashboard prints them as
"observed 4m ago · next attempt in 11m", instead of a control that would lie.

## What it reports

Percentages in the document are **remaining** capacity, converted once from the
used percentage Quota Cache records. `level` is computed server-side —
`low` at 40% remaining or less, `critical` at 10% or less — so every client
agrees without recomputing a threshold in CSS.

Credentials are emitted sorted by their weekly reset, soonest first, and every
row repeats that order, so the top row of each card is always the credential
that recovers next. Clients do not re-sort.

A credential that did not report a window is **excluded** from that row's mean
rather than counted as full. On Claude, the session row leaves out an account
whose weekly is spent, and the Fable row counts each account at no more than
its weekly has left, so an account with no weekly left adds nothing to either;
its own figures stay on the expanded card
([how](docs/summary-contract.md#claudes-session-and-fable-bounded-by-the-weekly)).
Rows keep serving their last good figures with `stale: true` and a reason when
the snapshot goes missing or its schema stops matching, because an empty
response is indistinguishable from a broken install.

Each row also arrives taken apart: every credential's slice of the pool, and
what the next reset gives back. The dashboard sizes its pooled bar from the
row's own `remainingFraction` and `projectedGainFraction` rather than working
anything out itself, so the bar and the number under it cannot disagree. The
slices are for a client that draws the pool account by account — see
[The pool, slice by slice](docs/summary-contract.md#the-pool-slice-by-slice).

Plan names arrive display-ready: Claude shows Team, Enterprise, Max 20x, Max 5x,
Pro or Free; Codex's plan enum is resolved to the tier name (`pro` is Pro 200,
`prolite` is Pro 100); and Grok shows its subscription's own name, such as
SuperGrok Heavy. Set `plan-labels` only if a provider renames a tier; prefix a
key with the provider id (`codex:pro`, `claude:pro`, and `xai:` for Grok) to
rename one provider's tier. An unprefixed key applies on every provider, Grok
included, as it always has — except to Claude's `pro`, `team`, `free` and
`enterprise`. Codex sends those same tokens for different tiers, so an
unprefixed key for one of them is taken as written for Codex, and leaves
Claude's alone; rename Claude's with `claude:`.

Where the provider reports them, a credential also carries its **credit
balance** — Codex credits, or Grok's prepaid dollars — and its **renewal date**,
which Codex reports. Anthropic reports no renewal date, so a Claude account
shows an estimate, `renews ~Oct 29 (est.)`, taken from when its subscription
started ([how](docs/summary-contract.md#renewalestimated--whether-that-renewal-is-an-estimate)).

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

Codex and Claude both bank **rate-limit resets**: entitlements already granted
to your account that clear its current windows when you spend one. Each account
holding at least one gets a tile under **Banked resets**, at the top of its
provider's section, with the count and when the soonest one lapses. An account
holding none shows nothing at all — no tile, no zero, no empty row.

Both halves cost what they should. The count rides along on the usage response
Quota Cache already fetches, so knowing it costs no request. A Claude grant's
expiry rides along too: each grant carries its own end date in that same
response. Codex's expiry needs a second endpoint, so Quota Cache asks for it
**only when the count is non-zero** — an account with nothing banked is still
exactly one request per poll. That matters because the deadline is the part
people lose: a Codex reset expires thirty days after it is granted, a Claude
grant on its own end date, and a count with no date beside it is the shape in
which they quietly lapse.

**Every account holding one gets its own Use one button** — the tile itself —
so you choose which account to spend on, including an account CPA has put in a
cooldown, which is usually exactly when you want one. Claude only spends a reset
on an account that is at a limit and not in its own cooldown after the last
one; where the last poll saw either, the reason is printed on the tile, but the
button stays, because the plugin checks with Claude afresh before spending
anything.

**Using one** opens a confirmation first, and the sentence it leads with is the
one that matters: a banked reset is not extra allowance. Spending it restores
that account's session and weekly windows, it brings your existing allowance
forward rather than adding to it, and **it cannot be undone**. Confirm and the
plugin asks the provider what can be spent — for Codex, the credit closest to
expiring; for Claude, the grant Claude recommends — spends it, and reports what
happened. If Claude says no reset can be spent right now, nothing is spent and
the dashboard says why.

A few things the design refuses to guess about:

- **The count on the card does not drop straight away.** It comes from Quota
  Cache's snapshot, which owns the schedule, so it corrects at the next poll —
  and the message after a redemption says so rather than letting the page
  disagree with itself.
- **A request that never completed is not a request that did nothing.** When
  no answer anyone can trust came back — from the provider to the plugin, or
  from the plugin to the page — the reset may already be spent, so the
  dashboard says the outcome is unknown and asks you to check the account's
  usage before pressing again. For ten minutes, pressing again repeats the
  same claim, or asks what the first press came to, rather than spending a
  second reset; restarting or updating CPA or Quota Glance, or reloading the
  page, ends that early.
  [One press, one spend](docs/summary-contract.md#one-press-one-spend) has the
  details.
- **Two presses cannot become two credits.** A second attempt against the same
  credential while the first is in flight is refused outright, and a second
  copy of the same press — a browser or proxy resending it on its own — is
  answered with the first copy's answer rather than spent again.
- **A reset puts the account straight back into CPA's rotation.** Once the
  provider confirms the reset, the plugin clears CPA's own cooldown on that one
  credential, as **Clear cooldown** in the CPA console does; otherwise CPA can
  keep skipping the account for as long as the cooldown it recorded at the limit
  still runs. If that clear fails, or CPA is older than v8.0.12, the message says
  the reset is spent and asks you to use **Clear cooldown** in the console — not
  to press **Use one** again, which would spend another reset.
  [CPA's cooldown after a reset](docs/summary-contract.md#cpas-cooldown-after-a-reset)
  has the details.

The button is absent — not greyed out — whenever it cannot work: with
`allow-redeem: false`, on a credential you have disabled in CPA, and in a
browser whose every way in was refused, where the band says to sign in again
instead. The count still shows in each case. A press takes at most 55 seconds.

**Use one works wherever the page loads** — the CPA sidebar, a browser tab, a
phone, the menu bar app — with the dashboard password or a console session.
On the password's door the press is a GET to `/spend`, because CPA dispatches
nothing else to a resource route, and nothing that can spend is in its URL.
Behind a reverse proxy, pass `Authorization` and `X-Quota-Glance-Spend`
through and allow at least 60 seconds.
[Why a GET spends](docs/access.md#why-a-get-spends) covers how it is fenced
off, and what retires it.

## OpenRouter balance

Quota Glance can show how much money is left on your OpenRouter account, as an
**OpenRouter** card below the quota providers. Setting it up is one step, in
Quota Cache rather than here: give Quota Cache **0.1.9 or newer** an
OpenRouter management key, as described in
[its README](../quota-cache/README.md#openrouter-balance-optional). Read that
section before creating one; a management key is a powerful credential. Quota
Cache then polls the balance on its normal schedule, and the card appears after
its first poll. Without a key there is no card and the page is unchanged.

The card shows the dollar amount left: what you have bought, minus what you
have spent. It turns **amber below `openrouter-warn-below`** (default `$5`) and
**red once nothing is left**, whatever the threshold. Change the threshold in
the plugin's settings in the CPA panel; it applies without a restart. Set it
to `0` to keep the card green until the balance runs out.

It is a figure, not a bar. OpenRouter reports only lifetime totals, so a
percentage would be a share of everything you have ever bought, which says
nothing about whether your next request will be paid for. Like every other
figure here it comes from the snapshot: Quota Glance makes no OpenRouter
request of its own, and the card says when a figure is stale or the last poll
failed. See [docs/summary-contract.md](docs/summary-contract.md#balances--prepaid-accounts)
for the fields.

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

How it signs in, in full: the page uses the dashboard password if one is saved
in this browser, and otherwise a CPA console session remembered on this same
address — one signed in with **Remember password** ticked, and talking to this
CPA. So the sidebar and the direct URL
`https://<your-host>/v0/resource/plugins/quota-glance/app` both open straight
onto the dashboard in any browser signed in to the console that way.

Anywhere else, the sign-in screen asks for the dashboard password, with a link
to the console below it. The password is saved in that browser, so it is asked
once. There is no host to configure either, because the plugin serves the page
and the page calls its own origin.

The key at the top right opens **Access on this browser**: which of the two
this browser holds and which one the page is using, **Sign out** (forgets the
saved password; it does not sign you out of the console), and, after CPA has
refused a console session, **Try it again**. How the page keeps a refused or
unanswered console key from adding up to CPA's 30-minute lockout is in
[docs/access.md](docs/access.md#when-cpa-refuses-a-key).

**Each window is one pooled bar.** What is left is one fill in the level's
colour, followed by a hatched stretch for what the next reset gives back; the
empty track after that is capacity no scheduled reset is about to return.
Under the bar are the pool's "% left", who resets next and when, and when it is
full again if that is later. Each account's own figure is in its row below,
with a note when the pool counts it as less, as a spent or capping weekly does
on Claude.

**Cards fold.** Click the **N accounts** line under a card and its per-account
rows fold away, leaving the bar, the big percentage, the recovery line, when it
is full again, and the **N accounts** line itself, which says how many accounts
the pool leaves out and why (for example "5 accounts · 2 weekly spent · 1
without a reading"). That is deliberate: folding a card should hide the detail,
not the headline, so a folded page still answers "how much is left and when
does more arrive" at a glance.

Which cards you folded is remembered in that browser, under
`quota-glance.collapsed` in local storage. It is a preference about how one
person reads the page rather than a fact about anyone's quota, so it never
enters the document — the plugin still serves the same bytes to every reader.
Cards are keyed by provider and row, so folding Claude's **Session** leaves
Codex's alone.

Everything the page shows is precomputed here: percentages, levels, ordering,
trend, every width in the pooled bar, the ink level of every block in an
activity strip, and the wording of each card's subtitle. The browser's only
arithmetic is subtracting an instant from now — to tick the countdowns, and to
age the last request under an address — and one subtraction of counts: the
fold line takes `heldOutCount` from `excludedCount` to say how many accounts
have no reading.

## Development

```sh
cd plugins/quota-glance
make ci       # gofmt, vet, the page's tests, bundle freshness, race tests, and the c-shared build
make golden   # regenerate both contracts under testdata/golden/
make smoke    # load the built library into a disposable CPA container (needs docker)

make web      # rebuild web/dist/index.html — commit the result
make web-dev  # dev server on :5173 against the golden fixtures, no CPA needed
make web-test # the page's own tests: node >= 22.18, nothing to install
```

`make web-test` runs `web/test/` under `node --test`: which credential the page
may present and when, driven through the same code the page runs, and the dev
fixture below over real HTTP. It is what holds the page to never costing a
failed CPA management sign-in it could have avoided.

`web/dist/index.html` is a build artifact that lives in git, because `go build`
embeds it and must never need Node. That arrangement is exactly how a stale
bundle ships silently, so `make ci` rebuilds the app into a scratch directory
and fails if the result differs from the committed file. Edit `web/src/`, run
`make web`, commit both.

`make web-dev` serves the committed golden documents through a stand-in for the
plugin's routes, so the dashboard can be worked on with nothing else running.
The stand-in answers like CPA, refusals and IP ban included, and spends nothing.
There is no console in front of it, so either type `dev-token` into the password
field, or seed a console session once in the browser console — remembered, and
for this address, or the page will not use it:

```js
localStorage.setItem('cli-proxy-auth', JSON.stringify({version: 0, state: {managementKey: 'dev', rememberPassword: true, apiBase: location.origin}}))
```

With both, the page uses the password; sign out from the key at the top right to
look at the console's door.

`?scenario=` then selects a state to look at, such as `degraded`, `holds` or
`cpa-banned`, and `?redeem=` how a press ends, such as `reset`,
`cooldown-failed`, `outcome-unknown` or `gateway`. An unknown name lists the ones there are: a
scenario in the response, an ending in the dev server's log. The stand-in
counts CPA's failed sign-ins as CPA does; restarting the dev server lifts its
simulated ban.

`QUOTA_GLANCE_PROXY` forwards the rest of `/v0` to a real CPA host, but the
summary and every press path are always answered by the stand-in, whatever it
is set to, so nothing the page sends reaches that CPA.
`web/design/redesign/option-d.html` is the approved design the app is built to
match (`?view=page` and `?view=menubar`).

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

Add `-redeem reset` (or any ending its `-help` lists) to offer the button and
answer every press with a stand-in that contacts no provider, then open
`http://127.0.0.1:8787/v0/resource/plugins/quota-glance/app?token=dev-token` to
press **Use one** through the real handler.
