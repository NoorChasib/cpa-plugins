# The summary document

`GET /v0/management/plugins/quota-glance/summary` returns one JSON document.
CPA authenticates it with the management key before this plugin sees the
request. The identical document is served on
`GET /v0/resource/plugins/quota-glance/summary` for a reader signed in with the
dashboard password; CPA authenticates nothing there, so that path carries the
plugin's own `web-token`. Same bytes, same ETag, two gates. Which of the two a
browser uses, and when, is in [access.md](access.md). This
page is its reference: the vocabulary a client has to understand, and the
guarantees it can rely on. Two committed examples live under `testdata/golden/`:

| File | What it shows |
| --- | --- |
| `summary.json` | Seven healthy credentials, an OpenRouter balance, and five Claude API credit organizations estimated from Quota Cache's usage meter (`seven-credentials.meter.json` beside the snapshot) with the dashboard's settings from `testdata/overrides/seven-credentials.json`: on the monthly credit, on a Console reading with a credit set on the dashboard, low, one with a cache-write difference worth showing, and one on Haiku 5.5's long-prompt tier refilled at midnight, plus an organization the meter saw that nothing names. A Claude credential under a CPA-style auth index carries a renewal date set on the dashboard equal to the build's own day, so it reads next month's. The layout the design was drawn against, with banked resets on Codex and Claude (one Claude account holding them with a `notLimited` hold), Codex credits and a renewal date, estimated Claude renewals (a month-end start clamped to a 30-day month, an annual plan, and a plan whose billing period was never read), and Grok prepaid credits. Claude's Fable row counts each account at no more than its weekly has left, so two entries print one figure and count another (`pooledPercent`), and the row reads 36% where the Fable figures alone average 43%. |
| `summary-degraded.json` | Every degraded state a real deployment produces — stale, failed, never-polled, disabled, unavailable, unsupported, entries with no reading, per-model rows, an unmapped window, a Grok product window folded into its credits, a reset in the past, a window with no reset at all, and a low OpenRouter balance whose last poll failed and has gone stale. A Claude account whose weekly is spent: held out of the session mean (`heldOut`, `heldOutCount`) while its idle session still prints 98%, its session reset the soonest on the card yet not the recovery the card announces, and counted as 0% on Fable while its own Fable figure reads 60%. Also the edge shapes of the credential extras: a Claude account in CPA cooldown still offering its reset with a dated `cooldown` hold, a cooldown already over, a disabled account's count without its button, unlimited Codex credits, a renewal already past, an estimated renewal for an annual plan begun on Feb 29, a subscription start still ahead of the build that gives no estimate, and an empty Grok balance. Claude API credits in every account state but `ok`, against a meter that has gone stale after a `shutdown` gap: out after a refusal, a lower bound because counting began after the cycle did, needing settings (one missing only its refill date, one missing its credit with an unpriced model), pending, misconfigured (`organization_id_invalid`), written by Quota Cache 0.1.13, an `item-<n>` for each reason one cannot be edited, an over-limit unlinked organization beside one nothing names, an orphaned API credit setting and an orphaned renewal date, with editing off (`allow-edit: false`). |

Both are byte-identical to what the route serves and are regenerated with
`make golden`. CI fails if a build stops reproducing them, so a change to either
is a deliberate contract change.

## Guarantees

- **All percentages are REMAINING capacity.** quota-cache records used capacity;
  the conversion happens once, here. `used_percent: 76` arrives as
  `remainingFraction: 0.24` / `remainingPercent: 24`.
- **Both a fraction and an integer percent** are emitted so the bar width and
  the printed label cannot disagree. Size bars from the fraction, print the int.
- **Everything is precomputed.** Thresholds, wording, and ordering are decided
  server-side. A client that recomputes them will disagree with another client.
- **Quota comes from the snapshot; routing activity comes from the host.**
  `credentials[].activity` is the one part of this document that is not built
  from the quota-cache snapshot, and the one part that moves between snapshot
  writes. The plugin therefore rebuilds on a short timer as well as on a write.
  That rebuild reads a file and asks CPA in-process for its roster; as
  everywhere else here, **it contacts no provider**.
- **`credentials[]` is pre-sorted** by each credential's `weekly` window reset,
  soonest first; those without one sort last. **Every row's `entries[]` repeats
  that order.** Do not re-sort.
- **`entries[]` lists every one of the provider's credentials**, always, in that
  same order. `entries.length == provider.credentialCount`. A credential that
  reported nothing for the window is present with `hasReading: false` rather
  than omitted, because a credential silently missing from a card cannot be told
  apart from one the operator never added.
- **`hasReading: false` is not zero.** Every numeric field on such an entry is
  zero and none of them means anything: render a dash. An absent reading and an
  exhausted credential are the same bytes and opposite facts.
- **Times are integer Unix epoch seconds.** Nullable ones are `null`, never
  omitted and never zero. `generatedAtEpoch + resetInSeconds == resetAtEpoch`.
- **The header line is precomputed too.** `observedAtEpoch` is the newest
  successful observation across every credential and `nextAttemptEpoch` the
  soonest poll quota-cache has scheduled — the two halves of "observed 4m ago ·
  next attempt in 11m". They are served rather than derived because deriving
  them means a max and a min across every entry. `nextAttemptEpoch` is the
  soonest attempt **still ahead**, because what it answers is when this document
  can next change; one credential stuck in backoff would otherwise hold it in
  the past forever and the header would read "due" while everything else kept
  polling on schedule. Only when nothing at all is scheduled ahead — a poller
  that has genuinely stalled — does it report an instant already passed, so the
  client can say "due" rather than go blank. Both are `null` when there is
  nothing to report.
- **Additive evolution only.** New fields and new enum values may appear without
  a `schemaVersion` bump; handle an unknown value by falling through to a
  neutral rendering rather than failing.

## Vocabulary

### `credentials[].status` — what is the state of this credential?

| Value | Meaning |
| --- | --- |
| `ok` | Polled successfully. |
| `error` | Its last poll failed. Last-known figures are still shown, marked. |
| `pending` | quota-cache knows about it but has not polled it yet. No data, not an error. |
| `unsupported` | quota-cache does not poll this provider. |
| `disabled` | Disabled in CPA. |
| `unavailable` | CPA will not route to it right now — typically a quota cooldown after a 429. |

The last two answer a different question from the rest: whether CPA will route
to the credential, not whether quota-cache can read it. They are reported here
because there is one `status` field and routing state wins it when set — but
they say nothing about the figures. A parked credential is still polled, still
appears on every card, and counts in every mean exactly as it would if CPA were
routing to it. The one kind of account with a reading that a mean leaves out —
on Claude's session row, an account whose weekly is spent — is judged from its
weekly reading and never from this field, so a cooldown neither causes that
nor prevents it
([Claude's session and Fable](#claudes-session-and-fable-bounded-by-the-weekly)).
Render it normally and mark it; `credentials[].status` is the only place that
fact lives.

`counters.observedOK` and `counters.observeError` follow the **poll**, not the
status: a credential polled cleanly is counted as observed whether or not CPA
will route to it. Credentials never polled at all (`pending`, `unsupported`) are
in `counters.credentials` and in neither.

### `entries[].state` — is this particular reading trustworthy?

A different question from `status`, and a different set.

| Value | Meaning |
| --- | --- |
| `ok` | A reading, and a good one. |
| `error` | The last poll failed. The previous figures are still shown. |
| `stale` | The observation is older than the configured `stale-after`. |
| `noData` | No reading: this credential did not report this window. Nothing is wrong with it — a plan with no Fable allowance lands here. |
| `pending` | No reading: quota-cache has not polled this credential yet. |
| `unsupported` | No reading: quota-cache does not poll this provider. |

The last three always accompany `hasReading: false`. `disabled` and
`unavailable` deliberately never appear here: they describe the credential, not
the reading, and a row that called itself `ok` on one card and `disabled` on the
next would be describing one credential two ways in the same column.

A spent weekly is not a state either. `state` says whether a reading can be
trusted, and the session of a Claude account whose weekly is spent is a good
reading: the 100% is true, the account just cannot use it. The two facts are
independent — an entry can be held out and `stale`, or held out and `error` —
so one field cannot carry both, and a state other than `ok` tells a client to
doubt the very figure the expanded card exists to show. `heldOut` and
`pooledFraction` say it instead, on the two rows it bears on.

### `entries[].dataIssues` — always an array, often empty

| Value | Meaning |
| --- | --- |
| `percentOutOfRange` | The provider reported below 0 or above 100. Clamped. |
| `percentInvalid` | Not a finite number. Reported as no remaining capacity, because overstating headroom is the damaging direction. |
| `resetInPast` | The reset instant has passed and the next poll has not landed. Normal; render "resetting…", not a negative countdown. |
| `observeError` | The last poll for this credential failed. |
| `refreshPending` | A poll is in flight. Not an error. |
| `stale` | Older than `stale-after`. |

### `credentials[].id` and `email`

`id` is the credential's CPA auth index. Treat it as an opaque key: CPA derives
a stable index that is **sometimes the file name** (`claude-you@example.com.json`)
and sometimes an opaque hash (`d990722065363974`), depending on how the
credential was loaded. Both shapes were observed against a real CPA instance.

`email` is resolved for you in either case — derived from the file name where
that is what the index is, and taken from CPA's own roster otherwise. Render
`email`; never try to parse `id`.

### `plan` — already display-ready

The string beside a credential is the name its tier is sold under, resolved
server-side, from a table of that provider's own. The same token means
different tiers at different providers — both sell a "Pro" — so no table is
ever applied to another provider's credentials.

| Provider | Reported | Shown |
| --- | --- | --- |
| Codex | `prolite` (also `pro_lite`, `pro-lite`) | Pro 100 |
| Codex | `pro` | Pro 200 |
| Codex | `self_serve_business_prolite` | Business Premium |
| Codex | `free`, `go`, `plus`, `team`, `business`, `enterprise`, `edu` | Free, Go, Plus, Team, Business, Enterprise, Edu |
| Claude | `max_20x`, `max_5x`, `max` | Max 20x, Max 5x, Max (a Max account whose tier the profile did not state) |
| Claude | `team`, `enterprise`, `pro`, `free` | Team, Enterprise, Pro, Free |
| Grok | the subscription's display name (`SuperGrok`, `SuperGrok Heavy`) | unchanged |

The Codex names follow the CPA console. The Claude tokens are what quota-cache
derives from the account's profile, which is where Claude says what it is on;
a snapshot written before quota-cache did that carries `Max` or `Team`, which
lands on the same rows. An unrecognized enum value is rendered readably
(`self_serve_business_usage_based` becomes "Self Serve Business Usage Based")
rather than shown raw, and anything that already reads as a name passes
through. `plan` is `""` when the provider reported none.

**Do not map this in the client.** An operator who needs a different name sets
`plan-labels` in plugin configuration, and every client then agrees.

### `activity` — what CPA has routed here recently, or `null`

A credential at 100% is either being routed to and keeping up, or not being
routed to at all. Those are opposite facts about a pool and they are the same
capacity bar, so the routing half comes from CPA's own rolling request counter
on the roster — not from the quota-cache snapshot, which knows nothing about it.

It hangs off the **credential**, not off a row: a request is made against a
credential and not against one of its windows, so the same block is correct on
every card the credential appears in.

```json
"activity": {
  "bucketSeconds": 600,
  "windowSeconds": 12000,
  "buckets": [{ "success": 0, "failed": 0, "intensity": 0 }],
  "success": 117,
  "failed": 0,
  "lastRequestAtEpoch": 1789012800,
  "live": true
}
```

| Field | Meaning |
| --- | --- |
| `bucketSeconds` / `windowSeconds` | The ring's shape, read off the host rather than assumed. CPA reports 20 buckets of 10 minutes today — the last 3h20m — and nothing here may hardcode that. |
| `buckets` | **Oldest first**, newest last; the last bucket is the one in progress. Every bucket is present, including empty ones. |
| `success` / `failed` | Totals over the window, so no client sums the ring to label it. |
| `lastRequestAtEpoch` | The end of the newest non-empty bucket, clamped to the build instant. An **upper bound**, not an exact instant: the ring counts per bucket and does not record where inside one a request landed. `null` when the window is empty. |
| `live` | Traffic in the bucket in progress. |

**`null` is not "no traffic".** `activity: null` means the host reports no such
counter — an older CPA — and the correct rendering is nothing at all. A
credential nothing has routed to has a full ring of empty buckets, `success` and
`failed` at 0, and `lastRequestAtEpoch: null`. One says unknown, the other says
idle, and drawing them the same way is the one mistake this shape exists to
prevent.

**Use `live` rather than comparing `lastRequestAtEpoch` to your clock.** Only the
server knows which bucket is in progress, and a request 30 seconds old and one 9
minutes old are both inside it.

#### `buckets[].intensity` — 0 to 3

Where the bucket sits on the ink ramp: `0` no traffic, `3` as busy as the
busiest bucket **anywhere in that provider**. Scaled provider-wide on purpose —
a strip scaled to its own row makes the credential taking a trickle look exactly
like the one carrying the pool, which is the single question the strip answers.

Like every other threshold here it is decided server-side, so two clients cannot
ink the same bucket differently. Clamp an unknown value to the top of the ramp
you implement. A bucket with any `failed` in it should read as a failure at any
volume: one failure inside a busy ten minutes is the thing most worth not
losing.

### `resetCredits` — banked resets this account holds, or `null`

Codex and Claude both bank **rate-limit resets**: entitlements already granted
to the account that clear its current windows when one is spent — Codex calls
them rate-limit reset credits, Claude calls them reset grants. It is not extra
allowance. Spending one restores that account's session and weekly windows,
which is why it hangs off the **credential** rather than off any of the windows
it would reset — showing it once per card would put three controls on screen
for one irreversible action.

`null` means there is nothing to show, and the correct rendering is **nothing at
all** — no badge, no zero. That covers every credential on a provider with no
such concept and every Codex or Claude account that has not been granted one,
which is the ordinary case.

```json
"resetCredits": {
  "availableCount": 2,
  "expiresAtEpoch": 1790251200,
  "expiresInSeconds": 1238400,
  "redeemable": true,
  "hold": "cooldown",
  "holdUntilEpoch": 1789020000
}
```

| Field | Meaning |
| --- | --- |
| `availableCount` | At least 1 whenever this object exists. |
| `expiresAtEpoch` / `expiresInSeconds` | The soonest credit that can still be spent. **`null` when the provider did not date it** — render that differently from a distant deadline rather than implying safety. |
| `redeemable` | Whether this dashboard may spend it. |
| `hold` | The provider's reason, at the last poll, that none can be spent right now: `notLimited` (Claude only resets an account that is at a limit), `cooldown` (one was spent recently), `paused`, `ineligible`. `""` when one can be, and always `""` for Codex, which gives no such reason. |
| `holdUntilEpoch` | When a `cooldown` hold lifts, and `null` otherwise. A cooldown whose end has already passed arrives as `hold: ""` with this `null`. |

**A banked reset lapses** — a Codex credit thirty days after it is granted, a
Claude grant on its own end date — which is the documented way operators lose
them, so the deadline is carried beside the count rather than left to be
discovered. For Codex, quota-cache reads it from a second endpoint and only
when the count is non-zero. A Claude grant carries its end date in the same
usage response as the count, so dating it costs no request. A credit or grant
it could not date still reports its count.

**Every account holding a reset gets its own button**, so the operator chooses
which account to spend on. **Never offer redemption without `redeemable`.** It
is false when redeeming is switched off in configuration
(`allow-redeem: false`), when the credential is disabled in CPA, and on any
provider other than Codex and Claude. The count still shows in every one of
those cases; only the control goes.

`redeemable` says the plugin would accept a press. Whether this client can send
one is its own question: a dashboard left with no way in — no password saved
and no usable console session, or each of them refused — shows the count
without the button, because a press could only be refused, and on the console
door each refusal is a failed CPA management sign-in. See [access.md](access.md).

`redeemable` is deliberately **not** false for a credential CPA reports as
`unavailable`. That status is CPA's cooldown on routing to it, and it says
nothing about the account: CPA still hands its token to the plugin, quota-cache
still polls with it, and a cooldown is exactly when an operator reaches for a
reset.

**`hold` is a hint, never a gate.** Print it beside the button — the
dashboard says "usable once at a limit", "cooling down · 1h 45m", "paused" and
"not eligible right now" — but do not hide or disable the button over it:
the reading is a poll old, and the plugin asks the provider afresh before it
spends anything. A press against a held account comes back as the matching
outcome below, with nothing spent.

### `renewalAtEpoch` — when the subscription renews, or `null`

The instant the account's subscription renews or ends. Codex reports its own;
Claude's is a date set on the dashboard, or else an estimate, as
`renewalSource` below says. `null` for every other provider, when there is
none of the three, and once a reported instant has passed: a renewal behind us
is a poll that has not yet seen the next one, and counting down past zero to it
would be wrong. It is never in the past. Render it as a date with a countdown,
ticking against your own clock.

### `renewalSource` — where that renewal comes from

`"reported"` (the provider's own date), `"dashboard"` (a date set on the
dashboard), `"estimated"` (from when the subscription began, below), or `null`
when `renewalAtEpoch` is `null`. The first that exists wins: **reported, then
dashboard, then estimated**. Codex reports its own date and never takes one
from the dashboard.

A dashboard date `d` is 00:00 UTC of that day while it is still ahead. Once it
has come, the renewal is the next one on the same day of the month, or of the
year when the plan's `billing_period` is `annual`, strictly after the build,
with the day clamped to a shorter month as an estimate's is. On the renewal day
itself that is next month's, so a countdown never goes below zero.

### `renewalEditable` and `renewalSetting`

`renewalEditable` is `true` for a Claude credential whose renewal date the page
may set, while editing is available (`apiCredits.editing`, below; the same rule
applies when `apiCredits` is `null`). It is `false` for every other provider,
when `allow-edit` is off or `settings.json` cannot be read, and for a credential
whose id is not a CPA auth index of 16 hexadecimal digits, which is what
`settings.json` keys a date by.

`renewalSetting` is the date stored on the dashboard, `{"date": "2026-10-29",
"revision": "5", "updatedAtEpoch": 1791547200}`, used or not, and `null` when
there is none. `revision` is what a save sends back as `baseRevision`. Saving
`date: null` ("Use estimate") deletes it.

### `renewalEstimateAtEpoch` — the estimate beside whatever wins

A Claude credential's estimated renewal, the next billing anniversary of when
the subscription began (`renewalEstimated`, below), whatever `renewalSource`
says. Beside a date set on the dashboard it is the date **Use estimate** returns
to, which the editor names: `set here · estimate ~Oct 21 · Use estimate`.
`null` for every other provider, and when Quota Cache has not read a start. A
plugin older than this field omits it; read a missing field as `null`.

### `renewalOrphans` — dates stored for credentials no longer listed

A top-level array, always present: each renewal date stored on the dashboard
whose credential is not in `credentials[]`, `{id, date, revision,
updatedAtEpoch}`, oldest `updatedAtEpoch` first. The page lists them for
removal; a save may only clear one (`date: null`). It is empty when the roster
could not be read, since there is then nothing to tell an orphan from a
credential the host failed to report.

### `renewalEstimated` — whether that renewal is an estimate

Always present. `true` only when `renewalSource` is `"estimated"`, and `false`
otherwise, including when `renewalAtEpoch` is `null`. A plugin older than this
field omits it, so read a missing field as `false`.

Anthropic does not report when a Claude subscription renews: Claude Code and
the CPA management centre show no renewal date either. It does report when the
subscription was created, and Quota Cache 0.1.10 keeps that as
`account_details.subscription_started_at` with the plan's `billing_period`
(`monthly` or `annual`) beside it. When a credential has no reported renewal
still ahead, no date set on the dashboard, and has a start, `renewalAtEpoch` is
the start's next anniversary strictly after the build, and `renewalEstimated`
is `true`:

- **Cadence**: yearly when `billing_period` is `annual`, and monthly otherwise,
  including when it is missing or `unknown`.
- **Anchor**: the start's day of the month and time of day, in UTC. In a month
  too short for that day it falls on the month's last day: a start on Jan 31
  renews Feb 28 (Feb 29 in a leap year), Mar 31, Apr 30, and so on. Each
  anniversary is counted from the start itself, never from the previous one,
  so the day does not drift down after February. An annual plan begun on Feb 29
  renews on Feb 28 outside leap years.
- **Boundary**: at the anniversary's own second, the next one is reported.
- A start still in the future produces no estimate.

A reported renewal and a dashboard date always take precedence and are never
marked estimated. Codex reports its own date and has no start, so a Codex
renewal is always `false`.

**Say that it is an estimate wherever you print it.** The anniversary is correct
for an account whose billing date has never moved. It is wrong by however far
the date has moved for one that was paused, changed plan mid-cycle, or
re-subscribed, and nothing in the snapshot can show that. The dashboard prints
`renews ~Oct 29 (est.) · in 24d`, counts the distance in whole days, and
explains the estimate in a tooltip. An estimate costs no request: the start
comes from the profile Quota Cache already reads for the plan, and the cadence
from the usage response it already polls.

### `credits` — money or credit the account can spend, or `null`

The account's spendable balance beyond its windows, already written out.

```json
"credits": { "display": "57,706.15", "unlimited": false, "amount": "57706.149", "unit": "credits" }
```

| Field | Meaning |
| --- | --- |
| `display` | Formatted for direct rendering. Codex credits: grouped, at most two decimals, trailing zeros dropped, `"0"` when none (`"57,706.15"`, `"120.5"`). Grok's prepaid balance: dollars, `"$12.40"`. `"Unlimited"` when the provider says there is no limit. |
| `unlimited` | `true` when the provider reports no limit; `display` is then `"Unlimited"`. |
| `amount` | The exact decimal in `unit`, unrounded and ungrouped — `"57706.149"`, `"12.40"` — and `""` when `unlimited`. For comparing or sorting, never for printing. |
| `unit` | `credits` or `usd`. |

Codex's comes from its credit balance and Grok's from its prepaid balance.
Grok's monthly allowance and on-demand cap are limits rather than money held,
and are not reported here. `null` for every other provider and whenever the
balance was not in the last successful poll. Like `resetCredits` it is the last
reading, stale or not; `status` says how old that is. It is a figure, never a
bar: there is no ceiling to take a fraction of.

### Spending one — `POST .../redeem` and `GET .../spend`

The one action on this plugin that changes anything, and the one place it
contacts a provider — Codex or Claude, whichever the credential is on. A press
can arrive by three routes, each authenticated as `/summary` is on its tree:

| Route | Auth | The press travels |
| --- | --- | --- |
| `POST /v0/management/plugins/quota-glance/redeem` | CPA management key | as the JSON body |
| `GET /v0/resource/plugins/quota-glance/spend` | `Authorization: Bearer <web-token>` | in the `X-Quota-Glance-Spend` header |
| `POST /v0/resource/plugins/quota-glance/redeem` | `Authorization: Bearer <web-token>` | as the JSON body |

A client holding the management key uses the first, and one holding
`web-token` uses `/spend`. The third is registered but unreachable today:
current CPA dispatches only GET to resource routes, so a POST there returns 404
before the plugin sees it, and `/spend` exists because of that. `make smoke`
reports which behaviour the running CPA has. All three end in the same code and
answer with the same bytes.

The press is the same JSON object on every route:

```json
{ "credentialId": "codex-noor@example.com.json", "confirmed": true, "pressId": "dGhpcyBpcyBhIHByZXNzIQ" }
```

`confirmed` must be `true`. The dialog belongs in the client, but a request that
arrives without an answer to it is refused rather than assumed — spending a
reset cannot be undone. On the POST routes the body must be JSON; a
form-encoded content type is rejected, because a form post is the one a
cross-site page can make without script. The credential must be one the served
document already reports as `redeemable`, so a caller cannot nominate a
credential the dashboard is not offering.

`pressId` names this one press: 16 to 64 characters from `A-Z`, `a-z`, `0-9`,
`_` and `-`. The dashboard draws 16 random bytes for each new press and sends
them as 22 base64url characters. It is required on `/spend` and optional on the
POST routes, where a body without one behaves exactly as it did before press
ids existed, so a page cached from an older release keeps working. A malformed
one is `invalid_request` wherever it is sent. What it buys is in
[One press, one spend](#one-press-one-spend).

#### `GET .../spend` — the press as a GET

```
GET /v0/resource/plugins/quota-glance/spend
Authorization: Bearer <web-token>
X-Quota-Glance-Spend: <the press object as UTF-8 JSON, base64url-encoded without padding>
Accept: application/json
```

There is no query string, and nothing in the URL selects the action, so a link,
a prefetch, a crawler or a cache that holds the URL holds nothing that can
spend. Send exactly one `X-Quota-Glance-Spend`, at most 4096 characters.

A GET is the request every layer between a page and this plugin feels free to
make or repeat on its own, so the route turns away anything that does not look
like a script on the dashboard asking, before it looks at the token. The checks
run in this order, and the first that fails answers:

1. **Switched off.** The plugin is disabled: `503 disabled`.
2. **Fetch metadata.** A `Sec-Fetch-Site` other than `same-origin`, a
   `Sec-Fetch-Mode` other than `cors` or `same-origin`, a `Sec-Fetch-Dest`
   other than `empty`, or any `Sec-Purpose` or `Purpose` header at all:
   `403 cross_site`. A header that is absent is allowed — a browser on a
   plain-HTTP origin sends none of them, and neither does anything that is not
   a browser. This refusal costs the token limiter nothing.
3. **Early data.** Any `Early-Data` header: `425 too_early`. The request may
   have been replayed by anyone on the path before the TLS handshake proved who
   sent it. Send it again on the established connection; nothing was spent.
4. **The token.** Missing or wrong: a bare `401`, or `429` with
   `Retry-After: 60` past 20 failures in a minute, from the same limiter as
   `/summary`. The right token is never throttled.
5. **Redeeming switched off** (`allow-redeem: false`): `404 not_found`.
6. **The header.** Missing, repeated, longer than 4096 characters, not unpadded
   base64url, not a JSON object, or a field of the wrong type:
   `400 invalid_request`. `confirmed` not `true`:
   `400 confirmation_required`. Then a `pressId` missing or malformed:
   `400 invalid_request`.
7. **The press**, exactly as on the POST routes, starting with the ledger
   below.

#### One press, one spend

A press that carries a `pressId`, on any route, is looked up in the plugin's
press ledger before anything else — the `redeemable` check included:

- The first copy of an id spends, and its answer — success or error, status,
  headers and body — is recorded.
- Every later copy of that id is handed the recorded answer byte for byte, plus
  `X-Quota-Glance-Replayed: 1`, and spends nothing. A copy that arrives while
  the first is still under way waits for it, up to 60 seconds — the press's own
  bound and five more — and past that gets `409 already_in_flight`, still
  spending nothing.
- The lookup comes first because a second copy is owed the first copy's answer
  even after the document stops offering the account, which is exactly what
  happens after a press that spent its last reset.
- The same id naming a different credential is `400 invalid_request`: one press
  is one decision about one account.
- An id is remembered for ten minutes after its answer, the same window as the
  claim journal under `outcome_unknown` below. The ledger holds 256; when every
  one is still under way or inside its ten minutes, a new press gets
  `409 already_in_flight` rather than an old one being forgotten early, because
  forgetting one early is what would let a copy of it spend again.
- It is in memory. A CPA restart, an update of the plugin or switching it off
  and on forgets every id, and the next copy of an old press is then a new
  press.

That is what makes a GET safe to repeat: a browser resending after a dropped
connection, a proxy retrying an idempotent method against another upstream, or
an edge replaying early data all repeat the id, and get an answer instead of a
second spend.

The ledger and the claim journal stack without overlapping. The ledger repeats
this plugin's answer, for the same press; the journal repeats a provider claim,
for a new press. An `outcome_unknown` answer is recorded like any other, so the
same `pressId` is told `outcome_unknown` again with no second provider call,
while a press with a new id goes through the journal and repeats the same
claim.

A client should keep a press's id while it does not know what the plugin did
with it, and send that id again if the reader presses again: after an answer
that was not the plugin's — the request failed, a response at 500 or above
without one of the codes in the table below (a gateway's page, or a gateway's
own JSON), a 2xx it could not read — and after a repeat of such a press that
was turned away before the plugin ran, which says nothing about the press it
repeated. The plugin's own answer settles the id, and so does a new press
turned away; the next press then gets a new one. Word a replayed answer as
what the earlier press came to (the dashboard prefixes "From your earlier
press:"), never as something this press did.

#### What a press does, and what comes back

What the plugin does with a press depends on the provider, and in both cases
it asks the provider what is spendable before it spends anything:

- **Codex** reads the account's credits and consumes the one closest to
  lapsing.
- **Claude** reads the account's organization and its grant status, and claims
  the grant the provider recommends — or, without a usable recommendation, the
  one closest to lapsing. When the status already says no grant can be spent
  (not at a limit, in a cooldown, paused, ineligible) nothing is claimed, and
  that reason is the outcome.

```json
{ "provider": "claude", "outcome": "reset", "windowsReset": 2, "remainingCount": 1, "snapshotPending": true }
```

`provider` is `codex` or `claude`, so the client can word the outcome in that
provider's terms. `remainingCount` is what the account holds now, counted from
the inventory read during this press — less one when one was spent. It is a
floor rather than a fresh reading, and the next poll replaces it.

| `outcome` | Spent? | Meaning |
| --- | --- | --- |
| `reset` | yes | A reset was spent and windows were cleared. |
| `nothingToReset` | yes | Codex accepted it but no window needed clearing. **The credit is still gone** — Codex consumes it on acceptance, and saying otherwise invites a second press. |
| `alreadyUsed` | earlier | The provider says the reset this claim names was already consumed. On the press after an `outcome_unknown`, this means **the earlier attempt went through**. |
| `noCredit` | no | Nothing could be spent: the count on the card was out of date, or Claude reports a grant that cannot be used right now without naming why. |
| `notLimited` | no | Claude only resets an account that is at a limit, and this one is not. |
| `cooldown` | no | Claude spent a reset on this account recently and will not spend another yet. |
| `paused` | no | Claude has paused the account's grant. |
| `ineligible` | no | Claude says the account is not eligible for reset grants. |
| `failed` | no | The provider refused for a reason this vocabulary has no better word for — today, Claude saying resets are unavailable. |

`snapshotPending` is `true` because the count on the card comes from
quota-cache's snapshot and will not move until its next poll. Say so rather than
letting the dashboard silently disagree with itself.

##### CPA's cooldown after a reset

CPA records a routing cooldown on a credential that hits its limit, and keeps
skipping that credential until the cooldown ends, however the limit was lifted.
So a reset the provider confirms is followed, in the same press, by clearing
that one credential's cooldown through CPA's `host.routing.reset_cooldown`
callback: the in-process equivalent of the console's **Clear cooldown**
(`POST /v8/management/routing/cooldown/reset`). It contacts no provider, needs
no management key, and leaves the token file alone. The result rides beside the
outcome as `cooldown`:

```json
{ "provider": "claude", "outcome": "reset", "windowsReset": 2, "remainingCount": 1, "snapshotPending": true, "cooldown": "cleared" }
```

| `cooldown` | Meaning |
| --- | --- |
| absent | The outcome is not a confirmed reset, so CPA's cooldown was not touched. Every outcome but `reset` and `alreadyUsed`, and `alreadyUsed` on a fresh claim. |
| `cleared` | CPA cleared the credential's cooldown, and its answer named that same credential. |
| `failed` | **Partial success.** The reset is spent, but CPA did not confirm the clear: it refused, answered for another credential, or did not answer within five seconds. CPA may keep skipping the account until its cooldown ends. |
| `unsupported` | **Partial success.** As `failed`, on a CPA older than v8.0.12, which has no such callback. |
| `unconfirmed` | Codex accepted the spend but its answer did not say `reset` — a code this build does not know, or a body it could not read. The credit is gone, so the outcome is still `reset`, but that is not evidence enough to clear CPA's cooldown, so it was left alone. |

Only two answers are authority to clear: the provider's own `reset`, and, on the
press that repeats a claim left unknown, the provider saying that same claim
(same credit or grant, same request id) was already spent. An unknown outcome,
a refusal, a hold, `noCredit`, `nothingToReset`, and a fresh claim finding its
grant already used clear nothing.

The clear runs after the claim is settled and taken off the journal, so it can
never turn into another spend. The press waits at most five seconds for it, with
the credential still reserved: a press arriving in that time is
`already_in_flight`, and a copy of the same press is handed the same answer from
the ledger without clearing again. CPA serves the clear with no deadline of its
own, so past five seconds the press answers `failed` rather than holding a spent
reset's answer until the browser gives up; the credential is then free, even if
CPA is still finishing the clear, and whatever CPA says later is discarded. The
next press is a fresh claim either way, which checks the provider's inventory
before it spends. A clear that did not happen is never retried by the plugin.
The remedy for `failed` and `unsupported` is **Clear cooldown** on the credential
in the CPA console; pressing **Use one** again would spend another reset and is
not one.

Failures carry a fixed code and never provider text:

| Status | `error` | Meaning |
| --- | --- | --- |
| 400 | `confirmation_required`, `invalid_request` | The press was refused before anything else happened: no confirmation, a malformed body or header, a malformed `pressId`, or a `pressId` first used for another credential. |
| 401 | — (no body) | Token routes only: the token was missing or wrong. Nothing was spent. |
| 403 | `cross_site` | `/spend` only: the browser marked the request as not made by a script on this page. Nothing was spent, and the token limiter was not charged. |
| 425 | `too_early` | `/spend` only: the request may have arrived as TLS early data. Nothing was spent; send it again. |
| 429 | — (no body) | Token routes only: more than 20 failed token attempts this minute, with `Retry-After: 60`. Never sent to the right token. |
| 409 | `not_redeemable` | The document does not offer this credential, or its provider has no banked resets. |
| 409 | `already_in_flight` | A press on this credential is still under way; or a second copy of a press outwaited the first; or the press ledger is full. Nothing new was started, but the earlier press may still spend a reset, and the card will not show it until quota-cache polls — so do not invite another press: one made once the earlier finishes is a new press, and spends a second reset if the account holds one. |
| 409 | `credential_unusable` | The credential cannot authenticate the request — an API-key login has none of this — or, on the press after an unknown outcome, it now signs in as a different account than the claim was made for. Nothing was sent. |
| 415 | `unsupported_media_type` | Not JSON. |
| 502 | `provider_unavailable` | The provider could not be reached before anything was spent. Try again later. |
| 502 | `provider_refused` | The provider turned the request away — a read refused or answered in a shape the plugin will not act on, or a spend refused on authentication. Nothing was spent. |
| 502 | `provider_rate_limited` | The provider is throttling the account. Nothing was spent. Try again later. |
| 502 | `outcome_unknown` | A spend was sent and no answer the plugin can trust came back. **A reset may have been spent.** Carries `retryUntilEpoch`, below. |
| 502 | `retry_window_closed` | An earlier press left its claim unresolved, and that claim's retry window closed before this press could settle it — during this press's reads, or while its repeat was in flight. **Whether the earlier claim spent a reset is still unknown**, and the claim is no longer on record: the next press is a new claim. |
| 404 | `not_found` | Redeeming is switched off (`allow-redeem: false`). |

**A 404 says who sent it by its body.** Every 404 from the plugin carries
`{"error":"not_found"}`: redeeming is switched off, or the plugin answers no
such path and method. A 404 without that body did not come from the plugin:
it is CPA's own, for a route it does not dispatch — a POST to a resource route,
any resource route while CPA's home page is enabled — or for a route the loaded
plugin does not register, such as `/spend` on a release older than 0.5.0.
Neither kind spent anything; the dashboard says redeeming is switched off for
the first, and that the server did not offer the reset route for the second.

**A 401 or 403 on the management route is CPA's, never the plugin's.** The
plugin answers neither there, so the press was turned away before the plugin
ran and nothing was spent. CPA's body names the reason — a missing or invalid
key, remote management disabled or its key not set, an IP ban — and each
refused key counts toward CPA's ban of the address, so a client must not send
the same key again on its own. [access.md](access.md#how-a-refusal-is-read)
lists the strings.

```json
{ "error": "outcome_unknown", "retryUntilEpoch": 1789013400 }
```

**`outcome_unknown` is the one failure where pressing again is a different
act.** The plugin keeps the unanswered claim in memory for ten minutes from
when the claim was first made, per credential, and `retryUntilEpoch` is the
instant that window closes (null if the plugin cannot say, which promises
nothing). A repeat does not extend it — the provider matches a repeated request
id against the original — so every `outcome_unknown` for the same claim names
the same instant. Print it as a time, never as "ten minutes": the reader of the
second answer has less of the window left than the reader of the first. A
press that starts before it repeats the **same claim** — the same
credit or grant and the same request id, which the provider matches against the
original — so it reports what happened to the first attempt or makes it once,
and can never spend a second reset. It skips the inventory read, which would see
the reset the first attempt may have spent as gone and pick another. It does
re-read who the credential signs in as — Claude's organization, Codex's ChatGPT
account — and refuses (`credential_unusable`) if that has changed. A refusal on
a repeat proves only that the repeat spent nothing — Claude's cooldown refusal
is exactly what it would say had the first attempt worked — so it comes back as
`outcome_unknown` again rather than as the refusal. A repeat is still a press on
the card, so it needs the card to still offer the button: once quota-cache has
polled and seen the account's last reset gone, `resetCredits` is `null`, the
route answers `not_redeemable`, and that is itself the answer. Once the window
closes the claim is dropped, and the next press starts fresh from the
provider's inventory, which finds the reset gone if the first attempt did spend
it — and on an account that holds another, spends that one. The window does not
survive a CPA restart, nor an update of the plugin or switching it off and on:
CPA does both of those by loading a fresh copy of the plugin without
restarting, and the claim lived in the old copy's memory. A client that prints
the promise must name all of these, not CPA's restart alone. A press whose
repeat cannot be sent before the window closes sends nothing and answers
`retry_window_closed`, not `outcome_unknown`, because there is no longer a
repeat to promise.

A client must not print "nothing was spent" for an answer it cannot read. A
code it does not recognise, or no readable code at all, proves nothing was
spent only on a 4xx, which is a request turned away before the plugin acted —
CPA's sign-in gate, or the plugin's own checks. On any other status — a reverse
proxy's 502 or 504 page, CPA's plain-text 502, a 200 whose body does not parse
— the outcome is unknown, worded like `outcome_unknown` without its promise.

Which provider statuses count as which is fixed: a 2xx with a result the plugin
recognises is an answer; 401, 403 and 429 turn a request away before it is
acted on and spend nothing; **everything else on the spend request is
`outcome_unknown`**, a 5xx above all — a 502 or 504 is what a gateway returns
when the service behind it was slow, not idle. Codex's 2xx is read more
generously than Claude's, because Codex consumes the credit on acceptance: a
Codex 2xx the plugin cannot read, or whose code it does not know, is reported
as `reset` rather than as unknown. A Claude 2xx it cannot read is unknown.

**Timing.** A press takes at most 55 seconds: each read is bounded at 12, and
the spend at 25 — Claude Code's own bound for the claim — and the spend is only
sent with that much time left, so it is never cut short by the press around it.
CPA puts no deadline of its own on any of the three routes. Give the request at
least 65 seconds before abandoning it, and treat an abandoned request as
`outcome_unknown`: the plugin may still have sent the spend. A reverse proxy in
front of CPA must allow at least 60 seconds and pass `Authorization` and
`X-Quota-Glance-Spend` through; one that drops the header gets
`400 invalid_request`, and nothing is spent.

### Saving settings — `POST .../settings` and `GET .../save-settings`

The dashboard's editor saves an API credit's monthly amount, refill date and
Console reading, and a Claude credential's renewal date, to `settings.json` in
`data-dir`. Nothing else is written and nothing is contacted. Two doors, each
authenticated as `/summary` is on its tree, and both behind `allow-edit`
(default `true`):

| Route | Auth | The batch travels |
| --- | --- | --- |
| `POST /v0/management/plugins/quota-glance/settings` | CPA management key | as the JSON body |
| `GET /v0/resource/plugins/quota-glance/save-settings` | `Authorization: Bearer <web-token>` | in one `X-Quota-Glance-Settings` header, base64url without padding |

One **Save** sends one batch: every changed row of one card, applied all or
nothing. A failure never leaves some rows saved.

```json
{"kind": "apiCredits", "items": [
  {"id": "org-3f2a9c1d0b7e", "baseRevision": "7", "monthlyUsd": "260.50", "renews": null,
   "reading": {"remainingUsd": "143.20", "at": "2026-10-09T13:20:00Z"}}
]}
{"kind": "renewals", "items": [{"id": "0123456789abcdef", "baseRevision": "", "date": "2026-10-29"}]}
```

Every value key is required, as a value or `null`; `null` is "not set", **Use
config**, **Use estimate** or **Clear**. A row sends its full desired state and
`baseRevision`, the `settings.revision` (or `renewalSetting.revision`) it was
opened with, `""` when nothing was stored. At most 16 `apiCredits` rows or 32
`renewals` rows, at most 4096 bytes of JSON.

The checks run in this order, both doors unless marked, and the first that
fails answers:

1. **Switched off.** `503 disabled`.
2. **GET only: fetch metadata and early data**, exactly as `/spend`:
   `403 cross_site`, `425 too_early`, costing the limiter nothing.
3. **GET only: the token.** A bare `401`, or `429` with `Retry-After: 60` past
   20 failures a minute, from the shared limiter. The right token is never
   throttled.
4. **Editing.** `allow-edit: false`: `404 not_found`, so an unauthenticated
   caller learns nothing about it. `settings.json` unreadable:
   `503 settings_unavailable`.
5. **POST only:** a `Content-Type` other than `application/json`:
   `415 unsupported_media_type`.
6. **Shape.** Over 4096 bytes, not exactly one header, not unpadded base64url,
   not an object, an unknown or missing key, an unknown `kind`, no rows or too
   many, a malformed `baseRevision`, or an id twice: `400 invalid_request`.
7. **Values**, first failure only: `400 {"error": <code>, "id": <id>, "field":
   <field>}`, `field` being `monthlyUsd`, `renews`, `reading.remainingUsd`,
   `reading.at` or `date`.

   | Value | Rule | Code |
   | --- | --- | --- |
   | `monthlyUsd` | 1 to 7 whole digits, at most 2 decimals, `0` allowed | `invalid_monthly_usd` |
   | `renews`, `date` | `YYYY-MM-DD`, a real date in 2000 to 2099 | `invalid_renews`, `invalid_date` |
   | `reading.remainingUsd` | as `monthlyUsd` | `invalid_reading_amount` |
   | `reading.at`, new or changed reading | RFC 3339 in UTC with `Z`, whole seconds, from 48 hours before the server's clock to 5 minutes after | `invalid_reading_time` |
   | `reading.at`, new or changed reading | not before the start of the cycle the batch's refill date (or, with `renews: null`, the configured one) gives | `reading_before_refill` |

   A reading resent unchanged, the same `remainingUsd` and `at`, is not checked
   against the clock again, so a credit can be changed on a row whose reading is
   a week old.
8. **Editable.** Every id must be offered by the served document:
   `apiCredits.accounts[].settings.editable`, or
   `credentials[].renewalEditable`. An id in `apiCredits.orphans` or
   `renewalOrphans` may be cleared, every value `null`, and never set. Otherwise
   `409 {"error": "not_editable", "ids": [...]}`.
9. **Conflicts.** A row whose `baseRevision` is not the stored entry's revision,
   asking for anything other than what is stored now:
   `409 {"error": "conflict", "revision": "<file revision>", "conflicts": [ids],
   "current": {id: <settings block or renewal setting>}}`, nothing written. A
   batch whose every row already holds what it asks for is
   `200 {"ok": true, "unchanged": true, "revision": "<file revision>"}`, with no
   write. A resent request is therefore a no-op or a conflict, never a second
   change, which is why no press id is needed.
10. **Rate.** Past 30 committed saves in the last minute, both doors together:
    `429 too_many_writes` with `Retry-After: 60`.
11. **Commit.** A new reading takes its baseline from the meter: its usage from
    00:00 UTC of the reading's day to the start of the reading's hour, from the
    meter the last rebuild read. A meter that has not saved since, because
    Quota Cache is stopped or behind, gives the hours it has: it counted
    nothing after its last save, so the baseline is complete, or short, which
    overstates the spend since the reading, the safe direction. A reading is
    therefore saved while the card reads "incomplete"; the row stays a bound
    until a reading taken after counting resumes. Only a meter that no longer
    keeps 00:00 UTC of the reading's day, which within the 48 hours allowed
    means a meter whose clock runs ahead, refuses it, with
    `400 invalid_reading_time`. Usage under a model name or prompt class this
    build does not know, as a newer Quota Cache may add, goes into the
    baseline as `(other)`, which is never priced. A new reading for an account
    whose `organizationId` is not in the form a stored reading holds is
    `409 not_editable` with its id. Past a bound (64 API credit rows, 256
    renewal dates, 1 MiB), renewal dates for credentials the document no
    longer lists are evicted, oldest first; when that is not enough,
    `409 settings_full`. A file this build could not load back is never
    written: that, and a failed write, are `503 settings_unwritable`, and
    nothing changed.
12. **Answer.** The document is rebuilt before the answer:
    `200 {"ok": true, "unchanged": false, "revision": "8", "settings": {id:
    <the row's settings block, or the credential's renewalSetting, from the
    rebuilt document; null for a row it no longer lists>}}`.

Each committed batch logs one `info` line, `quota-glance settings changed`, with
the batch's `kind`, the `door` (`console` or `password`), the `ids` and the
names of the `fields` that changed. Never a value, never the token.

`settings.json` is checked for shape when it is loaded; the time rules above
apply only when saving, so a stored reading that has since aged past its use
still loads, and the document reports it as unused. A file that fails a shape
rule applies nothing, is never written over, and makes editing unavailable
(`settingsUnreadable`) until it is moved aside and the configuration is saved
again. `/health` reports `settings: {path, revision, api_credit_entries,
renewal_entries, last_error}` and, for Quota Cache's meter file, `meter: {path,
flushed_at, last_error}` (`last_error` is `""`, `"missing"` or `"unreadable"`),
in the snake_case the rest of `/health` uses.

### `level` — computed server-side, on both rows and entries

Judged on the whole percent the document prints (`remainingPercent`), so the
level always agrees with the figure: `ok` at 41% and above, `low` from 40% down
to 11%, `critical` at 10% and below. The page draws bars blue, amber and red,
and the big number green, amber and red. **Do not recompute this threshold in CSS**; if
the rule changes it changes here, once.

### `trend` — `up`, `down`, `flat`, `unknown`

Compares the row now against the same members an hour ago; more than ±2 points
of movement is `up`/`down`. `unknown` when there is too little history — fewer
than two samples spanning half an hour, which is the normal state for the first
half hour after a restart. Hide the arrow entirely on `unknown`.

The arrow describes the headline beside it, so it compares what each member
counts as in the mean (`pooledFraction`), over the members the mean is taken
over. A held-out account is not compared: its idle session would read as a
pool refilling. On Claude's Fable row the capped figures are compared, not the
raw ones.

The history is recorded from served documents, so after upgrading from a
release without the Fable cap it still holds raw Fable figures. For the first
hour, Claude's Fable trend compares capped figures with those raw ones and can
read `down` where nothing moved. That is accepted: once the sample nearest an
hour ago was recorded capped, it compares like with like.

### `resetDisplayHint` — `countdown` or `none`

`countdown` means `resetAtEpoch` is in the future and safe to tick against.
`none` means either there is no reset instant (`resetAtEpoch` is `null` — a
consumable balance, for instance) or it has already passed (`resetAtEpoch` is
set and `resetInSeconds` is 0, alongside a `resetInPast` issue). Those two cases
are distinguished by whether `resetAtEpoch` is null.

### `staleReason` — `null` unless `stale` is true

| Value | Meaning |
| --- | --- |
| `neverObserved` | No credential has ever been polled successfully. |
| `cacheStale` | The newest observation is older than `stale-after`. |
| `cacheMissing` | The quota-cache snapshot could not be read. |
| `snapshotSchemaUnsupported` | quota-cache wrote a schema this build does not understand. |
| `rosterUnavailable` | CPA could not list credentials. |

On any of the last three the **last good document keeps being served**, marked
stale. An empty response is indistinguishable from a broken install, so it is
never sent in place of data that was valid a moment ago. What the dashboard's
settings decide in it, `apiCredits`, `renewalOrphans` and each credential's
renewal fields, is rebuilt from the same snapshot and roster with the settings
and Quota Cache's meter as they are now, so a value saved meanwhile shows at
once.

## Rows

`rowId` is the canonical window key, plus `:<model>` for a per-model window
(`model_weekly:spark`). `order` is the sort key; the client sorts by it and
holds no opinion about what the values mean.

| Key | Row | `order` |
| --- | --- | --- |
| `session` | Short rolling window; Claude's 5-hour limit | 10 |
| `weekly` | The regular multi-day window | 20 |
| `weekly_fable` | Claude's separate 7-day Fable/Opus allowance | 30 |
| `model_session` | Per-model short window; `sourceModel` is set | 40 |
| `model_weekly` | Per-model multi-day window; `sourceModel` is set | 50 |
| `credits` | A consumable balance rather than a rate window | 60 |
| `monthly` | Calendar-month window | 70 |
| `raw:<provider>:<id>` | A window quota-cache could not map. `matched` is `false`; render it generically. | 100 |

`title` is a ready-to-display heading. `matched` is `false` only for `raw:` rows.
xAI's `raw:xai:product/<id>` windows are each a product's share of its one
credit pool, so they are folded into `credits` rather than given rows of their
own; a credential that reports no `credits` window keeps them.

### Membership

A credential is a member of a row if it reported that window, with the one
exception below. Whether CPA will route to it, and whether it is disabled, play
no part. The figures a parked credential last reported are still true, and a
credential vanishing from every card at the exact moment it runs out is the
opposite of what the card is read for.

**A credential that did not report a window is excluded from the mean, not
counted as full** — otherwise one silent credential quietly inflates the single
number the whole card is read from. It is still listed, with `hasReading: false`.

**On Claude's session row, an account whose weekly is spent is held out** for
the same reason: its idle session reads 100% with nothing able to use it. It is
still listed, with its reading and `heldOut: true`. No other row holds anything
out; [Claude's session and Fable](#claudes-session-and-fable-bounded-by-the-weekly)
has the rule.

| Field | Counts |
| --- | --- |
| `memberCount` | The credentials the mean is taken over: those with a reading that are not held out. |
| `excludedCount` | The rest: no reading, or held out. `memberCount + excludedCount == credentialCount`, which is also `entries.length`. Neither is ever negative. |
| `heldOutCount` | The part of `excludedCount` that has a reading and was held out, so `excludedCount - heldOutCount` is the credentials with no reading. `0` on every row but Claude's session. |

Everything the aggregate says is taken over members alone.
`aggregate.remainingFraction` is the arithmetic mean of their `pooledFraction`,
which is each one's own remaining fraction on every row but Claude's Fable.
`soonestResetAtEpoch` is the earliest *future* reset among them, and
`projectedGainPercent` is the capacity the row regains when it fires, summed
over every member resetting in that same minute. `subtext` is that sentence
already written out, naming the first member in entry order that resets at
exactly `soonestResetAtEpoch`; it is empty when no member has a future reset. A
held-out account's reset is never the soonest, never part of the gain, and
never named, because it returns nothing the row can use.

A session row whose every reporting account is held out has no members. It
reads `remainingFraction: 0` and `critical`, with every reset, gain and full-at
field `null` or `0` and `subtext` empty, while every account is still listed
with its reading.

### The pool, slice by slice

A client that draws the row as one pooled bar — each credential's part of it,
and what the next reset returns — reads every width from the document rather
than dividing anything itself. The slices are the aggregate taken apart, so
they always add back up to it.

| Field | Meaning |
| --- | --- |
| `entries[].pooledFraction` / `pooledPercent` | What this credential counts as in the row's mean, as a fraction and as printed. Equal to `remainingFraction` / `remainingPercent` on every row but Claude's Fable, where it is the lesser of the account's Fable and its weekly remaining. A held-out entry's is its own reading, though the mean leaves it out. `0` with no reading. |
| `entries[].heldOut` | `true` for an entry with a reading that the mean leaves out: on Claude's session row, an account whose weekly is spent. Its figures are still its real reading; its shares are `0` and `resetsNext` is `false`. `false` everywhere else. |
| `entries[].poolShare` | This credential's slice of `remainingFraction`, on the row's 0–1 scale: its `pooledFraction` over `memberCount`. Across a row's entries the shares sum to `aggregate.remainingFraction`. `0` with no reading, and `0` when held out. |
| `entries[].resetsNext` | The credential's window resets at `soonestResetAtEpoch` or within the minute after it — on Claude's Fable row, its Fable window or its weekly does. It is part of the next recovery. True too for a credential whose reset returns nothing — one already full, or a Fable reset under a weekly that still holds it down (one with no more left than the Fable, a spent one included): it is still the next thing to happen to the row. Always `false` when held out. |
| `entries[].recoveryShare` | What the row regains from this credential at that recovery, on the same scale: the rise in its `pooledFraction`, over `memberCount`. On every row but Claude's Fable that is everything it has used. On Fable it is the rise in the lesser of its Fable and its weekly, each counted full if it resets in that minute. Never negative. `0` unless `resetsNext`. Across a row they sum to `projectedGainFraction`. |
| `aggregate.projectedGainFraction` | `projectedGainPercent` unrounded. Size a "what the next reset returns" mark from this and print the percent beside it, as with every other fraction/percent pair. |
| `aggregate.fullAtEpoch` / `fullInSeconds` | When the row would read 100% if nothing more were used: the latest reset among members below full. On Claude's Fable row a member waits for its weekly's reset as well, when the weekly is below full. `null` when every member is already full, when a reset one below full waits for has no instant (that window does not refill on a schedule), or when every one below full is already mid-turnover. A held-out account is not a member and is not waited for. |

Name the next recovery from the entries with `resetsNext` rather than from
`subtext`, which is a sentence for printing whole and is never parsed. A
client that draws the pool account by account lays the `poolShare` slices end
to end in entry order and the `recoveryShare` slices after them, hatched; the
track that is left is capacity no scheduled reset is about to return. A
held-out account has no slice, because its shares are `0`. The dashboard draws
the pool as one fill sized from `remainingFraction` and one hatched stretch
from `projectedGainFraction`, which are the same totals.

Print an account's own figure from `remainingPercent`, as everywhere else. A
`heldOut` entry, or a `pooledPercent` below `remainingPercent`, is the pool
counting that account as less than it reads, and is worth saying beside it:
otherwise the figures under a card do not average to its headline.

### Claude's session and Fable, bounded by the weekly

A Claude account's weekly limit sits over its other windows. Once the weekly is
spent the account can send nothing, however much of its session or its Fable
allowance reads as left, and an idle session reads 100%. Averaged as they read,
a pool whose accounts had burned through their weeklies showed a nearly full
session. So on Claude, those two rows count each account by what its weekly
still lets it use:

- **Session holds a spent weekly out.** An account whose weekly is spent is
  left out of the session mean and out of everything taken from it: the soonest
  reset, the full-at instant, the projected gain, `subtext` and the trend. Its
  entry is still listed with its real reading, `heldOut: true`, no `poolShare`
  or `recoveryShare`, and `resetsNext: false`.
- **Fable counts no more than the weekly leaves.** Nothing is held out of
  Fable. Each account counts as the lesser of its Fable and its weekly
  remaining (`pooledFraction`), since no account can spend more Fable than its
  weekly allows, so the row draws down as the weeklies do. Its entry still
  prints its own Fable figure, because the expanded card is where the actual
  numbers are read.
- **The Weekly row is unchanged.** It is the gate, not gated: an account whose
  weekly is spent is in its mean at 0%. So is every other Claude row (per-model,
  `credits`, unmapped) and every row of every other provider, where
  `pooledFraction` always equals `remainingFraction`. No other provider's
  windows nest this way.

Burn through every account's weekly and the card reads 0% on all three: the
weekly mean is 0, every Fable counts as 0, and the session, with every
reporting account held out, has no members and takes the empty row's shape
described under [Membership](#membership).

These rules, and `heldOut`, `heldOutCount`, `pooledFraction` and
`pooledPercent`, arrived in 0.6.0. An older plugin omits all four, so a client
that has to tell an empty session from a held-out one reads a missing
`heldOutCount` as nothing held out.

#### The gate

Each account's weekly is read once per build, and both rows are judged against
that same reading.

| Term | Meaning |
| --- | --- |
| **Which window** | The account's first window with key `weekly` and no model: the very reading the Weekly row shows for it. A per-model weekly is not the gate. |
| **Known** | There is such a window, its figure is a number, and its reset has not passed. A weekly with no reset instant is still known: at 100% it is a limit reached that Claude gives no end for, and is spent. |
| **Spent** | Known, and printed as 0% on the Weekly row. Judged on the rounded percent for the same reason `level` is: an account the page calls empty on one card must not count as usable on the card beside it. 99.6% used is spent; 99.4% is not. |

A gate that is not known holds nothing out and caps nothing, so the two rows
count the account at its own reading:

- **Turned over.** A weekly whose reset is at or before the build instant has
  refilled since the poll, so the figure no longer holds. It gates nothing
  until the next poll reads the new window — the same mid-turnover reading
  `fullAtEpoch` waits out.
- **Not a number.** A weekly figure that is not finite (`percentInvalid`) says
  nothing about the weekly, so there is nothing to judge the session or Fable
  against. The Weekly row still reports that entry as 0% with the issue, as it
  does any invalid figure.
- **No weekly window.** An account that reported no weekly is counted at its
  own readings.

The gate is decided from that reading and nothing else, because anything
folded in would move the session or Fable figure for a reason the reader
cannot see on the Weekly card beside it:

- **Not routing status.** A credential CPA has parked or that is disabled is
  gated exactly as it would be otherwise. `credentials[].status` says whether
  CPA routes to it; the weekly says whether the account has anything left.
- **Not freshness.** A `stale` or `error` weekly still gates on its last
  figure, as that figure still counts in the Weekly mean. The entry's `state`
  says how far to trust it.
- **Not credits or banked resets.** Usage an account can buy beyond its plan,
  and a banked reset waiting to be spent, are not the plan's capacity. Until a
  reset is spent the weekly is still spent, and the next poll after spending
  one shows it refilled.

#### What Fable recovers

A capped Fable account rises when its Fable window resets or when its weekly
does, so on the Fable row both are recovery events for an account whose gate is
known.

- `soonestResetAtEpoch` is the earliest future event over every member, a Fable
  reset or a weekly one. `subtext` names the first account with an event at
  exactly that instant.
- `resetsNext` is true for an account with either event at that instant or in
  the minute after it. The minute counts from the row's soonest instant, not
  from each account's own first reset, so an account has its Fable and weekly
  gathered into one recovery only when both fall inside it. In the fixtures
  each account's Fable resets a minute before its weekly, so the account
  resetting soonest has both gathered. A reset after that minute waits for a
  later recovery, even when it is seconds from the account's other reset.
- What the account regains is the rise in the lesser of the two: each window
  that resets in that span counts as full, and the other keeps its figure. A
  Fable reset returns only up to what its weekly has left, so under a weekly
  that still holds it down — one with no more left than the Fable, a spent one
  included, even one that resets just after the minute — it returns nothing,
  and that account is `resetsNext` with a `recoveryShare` of 0. A weekly reset
  under a Fable that binds returns only up to the Fable's own figure.
- `fullAtEpoch` waits, for each account below full, for its Fable reset when the
  Fable is below full and for its weekly reset when the weekly is. A weekly
  below full with no reset instant makes it `null`, as any window that does not
  refill on a schedule does.

Without a known gate each of these reduces to the plain rule: the window's own
reset, and everything the account has used coming back.

## Balances — prepaid accounts

`balances[]` holds money left on prepaid accounts that belong to no CPA
credential. Today that means the OpenRouter account, which quota-cache 0.1.9
and newer reads with a management key from its own configuration. It is always
an array, and is empty when no key is configured. It is sorted by `order`, then
`id`.

A balance is not a credential and takes part in nothing above: it is not in
`credentials[]` or `providers[]`, it is not counted in `counters`, and it does
not move `observedAtEpoch`, `nextAttemptEpoch`, or `staleReason`, all of which
describe the roster. Each balance carries its own instants and state instead.

| Field | Meaning |
| --- | --- |
| `id` | quota-cache's name for the account, `key-<fingerprint>`. It changes when the key is rotated. |
| `provider`, `title`, `order` | `openrouter`, `OpenRouter`, and the sort key. |
| `currency` | ISO 4217; `USD`. Every amount below is in it. |
| `hasReading` | `false` until a poll has succeeded. Every amount is then `0` and means nothing, `remainingText` is `""`, and `level` is `""`: render a dash, never `$0.00`. |
| `remaining` | `purchased` minus `used`, computed exactly from the provider's decimal strings. Negative on an overdrawn account. |
| `remainingText` | `remaining` as the dashboard prints it: `$74.75`, `$1,234.50`, `-$1.20`. Rounded to the cent, half away from zero; an amount that rounds to nothing is `$0.00`. |
| `purchased`, `used` | OpenRouter's all-time totals, `total_credits` and `total_usage`. |
| `warnBelow` | The configured `openrouter-warn-below`, default `5`. |
| `level` | `critical` once `remaining` is zero or below, whatever the threshold; `low` below `warnBelow`; otherwise `ok`. |
| `subtext` | The line under the amount, already written: `$25.75 spent of $100.50 purchased · warns below $5.00`, `Below your $5.00 warning · …`, `Out of credit · …`, or, with no reading, why there is none. It never says anything relative to now. |
| `observedAtEpoch` | When the reading was taken. `0` with no reading. |
| `nextAttemptEpoch` | quota-cache's next scheduled poll of this account. |
| `state`, `dataIssues` | The same vocabulary as a row entry: `ok`, `stale`, `error`, `pending`; `observeError`, `refreshPending`, `stale`. A failed poll keeps the last reading with `state: "error"`. |

**It is never a bar.** OpenRouter reports lifetime totals, so a fraction would
be a share of everything ever bought, which says nothing about whether the next
request will be paid for. The amount is the headline, coloured by `level`.

## API credits — Claude Console organizations

`apiCredits` is the monthly Claude API credit that a Max or Team plan deposits
into its linked Claude Console organization, for every organization in Quota
Cache's `claude-api-credits` configuration, **estimated** from the API traffic
those organizations sent through CPA, and pooled. It is an object, or **`null`
when the snapshot has no `anthropic-api` entry**, which is every snapshot until
that list is configured. It is never omitted, and `null` and an object with
nothing counted are different facts: the first means "not configured", the
second "configured, nothing counted yet".

Like `balances`, it is not a credential and takes part in nothing above: it is
not in `credentials[]` or `providers[]` (so the menu bar neither offers nor
colours it), not counted in `counters`, and does not move `observedAtEpoch`,
`nextAttemptEpoch`, or `staleReason`. Each account carries its own state.
Adding it did not bump `schemaVersion`. An older plugin omits the key, so a
client reads a missing `apiCredits` as `null`.

### Where the figures come from

- **Tokens** from Quota Cache 0.1.14's API meter, the file
  `snapshot.meter.json` beside the snapshot. Quota Cache counts every Claude
  API-key request CPA makes, per Console organization (by Anthropic's
  `anthropic-organization-id` response header), per UTC day and hour and per
  model. It never asks Anthropic anything. The meter's own format is in Quota
  Cache's [cache format](../../quota-cache/docs/cache-format.md).
- **Prices** from a table embedded in this plugin, read from Anthropic's
  pricing page on the date in `pricing.asOf`. Cache writes are priced at the
  5-minute rate; CPA does not say which were 1-hour writes.
- **The credit and its refill date** from the dashboard when set there, else
  from Quota Cache's configuration (`monthly-usd`, `renews`). Anthropic reports
  neither.
- **A Console reading** the operator types in: what Console showed as left, and
  when. It is the anchor that corrects everything the meter cannot see.

So every amount is an estimate, and one that may be missing spend says so with
`lowerBound`. Print `≈` before an estimate and `≤` / `≥` before a bound.

### Money

Every amount is in US dollars (`currency: "USD"`), computed on exact rationals:
a token's price is the table's dollars per million tokens divided by a million,
so one Haiku 5.5 cache-read token is exactly $0.00000001. Nothing is computed
in floating point. Each amount is emitted twice: a JSON number, converted once
at the end, and the text the dashboard prints (`$1,234.56`, rounded to the cent
half away from zero, never `-$0.00`). Print the text; size bars from the
fractions.

### The cycle

An organization's cycle comes from its refill date through Quota Cache's
`client.CreditCycleAt`, the one rule both plugins use. It starts at 00:00 UTC on
the most recent occurrence of the refill day of the month and ends at the next,
the day clamped to each month's length (a refill on the 31st falls on Feb 28,
then Mar 31). The meter's day buckets start at 00:00 UTC too, so a cycle's spend
is exact to them.

### The estimate, account by account

1. **Credit.** The dashboard's `monthlyUsd`, else the configured `monthly-usd`,
   else unknown (`monthlyCreditSource`: `dashboard`, `config`, `none`).
2. **Cycle.** From the dashboard's `renews`, else the configured one, else none
   (`renewsSource` likewise).
3. **Reading.** A stored Console reading is used when it was taken for this
   account's current organization (else `otherOrganization`), is not in the
   future (`future`), and, with a cycle, is from this cycle (`beforeRefill`) or,
   without one, from the last 31 days (`tooOld`). An unused reading is kept and
   reported with its reason. A reading taken in the first 24 hours of the
   cycle adds `readingOnRefillDay`: credits arrive "shortly after" the plan's
   payment, so Console may still have shown the old cycle's remainder.
4. **Spend.** The cycle's spend is the meter's usage from the cycle's start,
   priced. The spend since a reading is the meter's usage from 00:00 UTC of the
   reading's day, less the baseline stored with the reading (that day's usage
   up to the start of the reading's hour), priced. A reading anchors at the
   start of its hour: spend earlier in that hour Console already reflected is
   counted twice, which understates what is left by at most an hour's spend.
   `spent` is the cycle's spend with a cycle, the spend since the reading
   without one, and `spentSinceEpoch` says which.
5. **Basis and left.** With a used reading of `R`: `basis: "reading"`, `left =
   max(0, R - spent since)`. Else with a credit and a cycle: `basis: "credit"`,
   `left = max(0, credit - cycle's spend)`. Else no basis and no `left`. `used`
   is `credit - left`, clamped to the credit. Spend past the basis is `overage`
   and adds `overCredit`: credit is spent first, so it is likely purchased
   credit. A reading above the credit adds `readingAboveCredit`.
6. **Refusals.** Since the anchor (the reading's time, else the cycle's start),
   a low-credit refusal from Anthropic not followed by a success marks the
   account **out** (`refused`). A refusal of a Claude Code-based client's request
   (Claude Code, which the credit does not cover, or the Agent SDK, which it
   does) marks it out only when the estimate already had a tenth of the credit
   or less left (`refusedNearlySpent`); otherwise it is `claudeCodeRefused` and
   not out. Out means `left` 0, `used` the credit, fraction 0, `critical`, and
   `estimateLeftText` keeps what the estimate had.
7. **Lower bound.** `lowerBound` is true when any of these happened after the
   anchor: counting began late (`meterStartedLate`); Quota Cache stopped
   counting for five minutes or more, or has been stopped that long now
   (`meterGap`; a briefer stop, a restart or an update, is ignored everywhere);
   it dropped records (`meterDropped`); its meter was full (`meterFull`); a
   failed request with tokens could not be matched to an organization
   (`meterUnattributed`); it could not read a record (`meterRejected`); the
   window used a model the price table does not list (`unpricedModel`); or the
   meter is stale.
8. **No traffic.** Nothing ever seen adds `noTraffic`. With every request routed
   through CPA, nothing seen is nothing spent, so it is not a bound by itself.
9. **Stale.** A meter not saved within `stale-after`: Quota Cache saves at least
   every ten minutes while it counts.
10. **Fraction and level.** `left / credit`, clamped to 0..1, on the usual
    thresholds. A credit of `0` has fraction 0, level `""` and `zeroCredit`.

### `accounts[].state` — the first rule that matches

| # | When | `state` | Counted |
| --- | --- | --- | --- |
| 1 | No configuration, or Quota Cache found a problem with the item | `misconfigured` | no |
| 2 | Written by Quota Cache 0.1.13, which names no organization | `cacheTooOld` | no |
| 3 | No meter file, or the organization is not in it yet, or is dormant (the meter has not saved since the configuration changed) | `pending` | no |
| 4 | The credit is unknown, or there is no basis (no used reading and no cycle) | `needsSettings` | no |
| 5 | Out (rule 6) | `out` | yes |
| 6 | The meter is stale | `stale` | yes |
| 7 | Otherwise | `ok` | yes |

`misconfigured`, `cacheTooOld` and `pending` carry no amounts. `needsSettings`
still carries `spent` when it has an anchor, and `left` with a used reading;
`hasEstimate` says whether `left` and `used` mean anything.

`dataIssues` is always an array, each issue at most once, in this order, and
`issue` is the sentence for the first (`needsCredit` with `needsRefillDate`
reads as one):

| Issue | Sentence |
| --- | --- |
| `misconfigured` | The problem's own sentence, e.g. "Add this organization's organization-id, from Console under Settings, Organization." An unknown code, or one only 0.1.13 wrote, reads "This item's configuration has a problem." |
| `cacheTooOld` | Quota Cache 0.1.13 reads Anthropic's cost report, which Quota Glance no longer uses. Update Quota Cache to 0.1.14 and add this organization's organization-id. |
| `meterMissing` | Quota Cache has not saved an API meter yet. Update it to 0.1.14 or newer; counting starts when it next loads. |
| `orgNotCounted` | Counting starts at Quota Cache's next save. |
| `needsCredit` | Set this organization's monthly credit to count it in the total. (With `needsRefillDate`: Set this organization's monthly credit and refill date.) |
| `needsRefillDate` | Set the refill date, or enter a Console reading. |
| `refused` | Anthropic refused a request for low credit, so this credit is spent. With an estimate above zero: … The estimate had $35.00 left; enter a Console reading to correct it. |
| `refusedNearlySpent` | Anthropic refused requests from a Claude Code-based client for low credit, and the estimate is nearly spent, so this credit is shown as spent. Enter a Console reading to check. |
| `stale` | Quota Cache has not saved its meter recently, so recent spend may be missing. |
| `meterStartedLate` | By basis: Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it. / Counting began after your Console reading, so spend in between is missing. / Counting began recently, so earlier spend is missing. |
| `meterGap` | Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it. |
| `meterDropped` | Quota Cache dropped usage records it could not keep up with, so some spend is missing. |
| `meterFull` | Quota Cache's meter was full, so some spend is missing. |
| `meterUnattributed` | Some failed requests could not be matched to an organization, so some spend may be missing. |
| `meterRejected` | Quota Cache could not read some usage records from CPA, so some spend may be missing. |
| `unpricedModel` | Some requests used a model with no listed price and are left out: claude-mythos-preview (1.0M tokens). At most three, most tokens first, then "and N more". |
| `zeroCredit` | This organization's monthly credit is set to $0.00. |
| `overCredit` | Spend is $40.10 past the monthly credit. Anthropic bills purchased credit after the monthly credit; if there is none, enter a Console reading. |
| `readingAboveCredit` | Your Console reading is more than the monthly credit; check the monthly credit. |
| `readingOnRefillDay` | This Console reading was taken on the refill day. If Console did not show the new credit yet, enter a new reading once it does. |
| `readingUnused` | By reason: Your Console reading of $143.20 on Sep 30 was before the last refill, so it is not used. / … was for a different organization-id … / … is over 31 days old and there is no refill date … / … is dated in the future … |
| `claudeCodeRefused` | Anthropic refused requests from a Claude Code-based client (Claude Code or the Agent SDK) for low credit. If they were Agent SDK requests, this credit may be spent; enter a Console reading. |
| `noTraffic` | No API traffic for this organization has reached CPA since counting began. |
| `configMonthlyUsdInvalid`, `configRenewsInvalid` | monthly-usd (renews) in Quota Cache's config is not a dollar amount (a date), so it is ignored. |
| `adminKeyIgnored` | Quota Cache no longer uses this item's admin-key. Delete it from the config. |

### The object

| Field | Meaning |
| --- | --- |
| `title`, `currency` | `Monthly API Credit`, `USD`. |
| `pricing` | `{asOf, source, cacheWrites}`: the day the price table was read, the page, and `"5m"`. |
| `meter` | Quota Cache's meter, or `null` without a readable meter file: `sinceEpoch`, `updatedAtEpoch` (when it was saved), `stale`, `stoppedAtEpoch` and `stopReason` while it is stopped, `dropped`, `unattributed`, `rejected` and `foreign` counts with the last instant of the first three, and `gaps` (`[{fromEpoch, toEpoch, reason}]`, oldest first, leaving out every gap under five minutes). |
| `editing` | `{available, reason}`; `reason` is `""`, `disabled` (`allow-edit: false`; stored values still apply) or `settingsUnreadable` (stored values do not apply). |
| `pool` | Below. |
| `accounts` | Every configured organization, counted or not, sorted by `order`, then `label`, then `id`. Do not re-sort. |
| `unlinked` | Organizations that sent API traffic through CPA while no counted item names them, from the meter, newest `lastSeenEpoch` first: `{organizationId, firstSeenEpoch, lastSeenEpoch, requests, reason}`, `reason` being `overLimit` (an item past the first 16 names it) or `notConfigured`. Always an array. |
| `orphans` | Values stored on the dashboard for an account no longer listed, `{id, monthlyUsd, renews, hasReading, revision, updatedAtEpoch}`, oldest first. A save may only clear one. Always an array. |

### `pool`

The counted accounts (`ok`, `stale`, `out`) summed. `hasEstimate` is `false`
when none is counted: every amount is then `0` with `""` text, `level` is `""`,
and `nextRefill` and `fullAtEpoch` are `null`. `lowerBound` is true when any
counted account's is.

`monthlyCredit`, `used`, `left` and `overage` are the counted accounts' own,
summed, so one organization's overage never consumes another's credit.
`remainingFraction`, `remainingPercent` and `level` are `left / monthlyCredit`
(level `""` when that credit is 0). `nextRefill` is `{accountIds,
refillAtEpoch, refillInSeconds, gain, gainText, gainFraction, gainPercent}`:
the soonest refill among counted accounts with a cycle that have used any
credit, every account refilling at that second, and the credit they have used,
which the refill restores. `fullAtEpoch` and `fullInSeconds` are the latest
such refill. `accountCount == countedCount + missingCount`.

### `accounts[]`

| Field | Meaning |
| --- | --- |
| `id` | `org-<12 hex>` from the item's organization, or `item-<n>` for an item Quota Cache could not link to one. Dashboard values are keyed by it, so a label rename keeps them and a different organization does not. |
| `label`, `order`, `organizationId` | As configured; `organizationId` is `""` when missing or invalid. |
| `state`, `counted`, `hasEstimate`, `basis`, `lowerBound` | Above. |
| `monthlyCredit`, `monthlyCreditText`, `monthlyCreditSource` | The credit in force and where it comes from; `""` text when unknown. |
| `spent`, `spentText`, `spentSinceEpoch` | Metered spend since the cycle's start, or the reading's time without a cycle; `null` and `""` without either. |
| `used`, `left`, `overage` (each with `…Text`) | As above; `""` text when not meaningful. |
| `estimateLeftText` | What the estimate had left before a refusal marked the account out; `""` unless it is out. |
| `remainingFraction`, `remainingPercent`, `level` | Rule 10. |
| `cycleStartEpoch`, `renewsAtEpoch`, `renewsInSeconds`, `renewsSource` | The cycle containing the build instant, `null` without a refill date. |
| `reading` | The reading in use, `{remaining, remainingText, atEpoch, enteredAtEpoch, spentSince, spentSinceText}`, or `null`. |
| `cacheWriteExtra`, `cacheWriteExtraText` | How much more the estimated window would cost were every cache write a 1-hour one; the text (`$6.00`) is `""` below $0.01. The page words it: "Cache writes are priced at the 5-minute rate; at the 1-hour rate this would be $6.00 more." |
| `unpriced` | `[{model, tokens}]` for every model in the estimated window the table has no price for, most tokens first. `(other)`, the meter's overflow, is never listed. Always an array. |
| `refusals` | `{total, lastAtEpoch, claudeCodeTotal, claudeCodeLastAtEpoch}`. |
| `meterSinceEpoch`, `lastSeenEpoch` | When the meter began counting the organization, and its latest request. |
| `dataIssues`, `issue` | Above. |
| `settings` | The editor's view, below. |

### `accounts[].settings`

| Field | Meaning |
| --- | --- |
| `editable`, `notEditableReason` | `notEditableReason` is `overLimit` (an `item-<n>` past the first 16), `duplicateOrganization`, `noOrganization` (any other `item-<n>`), `cacheTooOld`, or, when editing is unavailable altogether, `editing.reason`. |
| `revision` | The stored entry's revision, `""` with nothing stored; a save sends it back as `baseRevision`. |
| `monthlyUsd`, `renews` | The dashboard's values, `""` when none. |
| `configMonthlyUsd`, `configMonthlyUsdInvalid`, `configRenews`, `configRenewsInvalid` | Quota Cache's configured values, always present so the page can show them beside an override and offer **Use config**; `configRenews` is `YYYY-MM-DD`. |
| `reading` | The stored reading, used or not, `{remainingUsd, atEpoch, enteredAtEpoch}`, or `null`. |
| `readingUnusedReason` | `beforeRefill`, `otherOrganization`, `tooOld`, `future`, or `""`. |
| `updatedAtEpoch` | When the entry was last saved, or `null`. |

### What the estimate cannot see

Traffic not routed through CPA; web search, code execution hours, fast mode
and US-only inference, none of which the usage record carries; whether a cache
write was a 1-hour one; spend while Quota Cache was stopped for five minutes or
more (flagged as a gap) or between a crash and its last save (at most a
minute); a prompt-length tier added to a model after the table was read; and
Claude Code requests in an organization that also holds purchased credit, which
Anthropic bills to the purchased credit but the meter counts against the
monthly one. A Console reading resets all of these.
