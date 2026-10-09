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
| `summary.json` | Seven healthy credentials and an OpenRouter balance. The layout the design was drawn against, with banked resets on Codex and Claude (one Claude account holding them with a `notLimited` hold), Codex credits and a renewal date, estimated Claude renewals (a month-end start clamped to a 30-day month, an annual plan, and a plan whose billing period was never read), and Grok prepaid credits. Claude's Fable row counts each account at no more than its weekly has left, so two entries print one figure and count another (`pooledPercent`), and the row reads 36% where the Fable figures alone average 43%. |
| `summary-degraded.json` | Every degraded state a real deployment produces — stale, failed, never-polled, disabled, unavailable, unsupported, entries with no reading, per-model rows, an unmapped window, a reset in the past, a window with no reset at all, and a low OpenRouter balance whose last poll failed and has gone stale. A Claude account whose weekly is spent: held out of the session mean (`heldOut`, `heldOutCount`) while its idle session still prints 98%, its session reset the soonest on the card yet not the recovery the card announces, and counted as 0% on Fable while its own Fable figure reads 60%. Also the edge shapes of the credential extras: a Claude account in CPA cooldown still offering its reset with a dated `cooldown` hold, a cooldown already over, a disabled account's count without its button, unlimited Codex credits, a renewal already past, an estimated renewal for an annual plan begun on Feb 29, a subscription start still ahead of the build that gives no estimate, and an empty Grok balance. |

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
Claude's is an estimate, flagged by `renewalEstimated` below. `null` for every
other provider, when quota-cache has read neither, and once a reported instant
has passed: a renewal behind us is a poll that has not yet seen the next one,
and counting down past zero to it would be wrong. Render it as a date with a
countdown, ticking against your own clock.

### `renewalEstimated` — whether that renewal is an estimate

Always present. `true` only when `renewalAtEpoch` is an estimate rather than the
provider's own date, and `false` otherwise, including when `renewalAtEpoch` is
`null`. A plugin older than this field omits it, so read a missing field as
`false`.

Anthropic does not report when a Claude subscription renews: Claude Code and
the CPA management centre show no renewal date either. It does report when the
subscription was created, and Quota Cache 0.1.10 keeps that as
`account_details.subscription_started_at` with the plan's `billing_period`
(`monthly` or `annual`) beside it. When a credential has no reported renewal
still ahead and has a start, `renewalAtEpoch` is the start's next anniversary
strictly after the build, and `renewalEstimated` is `true`:

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

A reported renewal always takes precedence and is never marked estimated. Codex
reports its own date and has no start, so a Codex renewal is always `false`.

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
never sent in place of data that was valid a moment ago.

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
