# The summary document

`GET /v0/management/plugins/quota-glance/summary` returns one JSON document.
CPA authenticates it with the management key before this plugin sees the
request. The identical document is served on
`GET /v0/resource/plugins/quota-glance/summary` for a reader with no console
session; CPA authenticates nothing there, so that path carries the plugin's own
`web-token`. Same bytes, same ETag, two gates. This
page is its reference: the vocabulary a client has to understand, and the
guarantees it can rely on. Two committed examples live under `testdata/golden/`:

| File | What it shows |
| --- | --- |
| `summary.json` | Seven healthy credentials. The layout the design was drawn against. |
| `summary-degraded.json` | Every degraded state a real deployment produces — stale, failed, never-polled, disabled, unavailable, unsupported, entries with no reading, per-model rows, an unmapped window, a reset in the past, a window with no reset at all. |

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
appears on every card, and still counts in every mean. Render it normally and
mark it; `credentials[].status` is the only place that fact lives.

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
server-side. Claude reports `Max` / `Team` and Grok reports its tier name, which
pass through untouched. Codex reports a plan enum, which is mapped: `pro` is
Pro 20x and `prolite` is Pro 5x — the enum separates the two tiers, so this is a
derivation rather than a guess. An unrecognized enum value is rendered readably
(`self_serve_business` becomes "Self Serve Business") rather than shown raw.
`plan` is `""` when the provider reported none.

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

Codex banks **rate-limit resets**: entitlements already granted to the account
that clear its current windows when one is spent. It is not extra allowance.
Spending one restores that account's session and weekly Codex windows and moves
its weekly reset date, which is why it hangs off the **credential** rather than
off any of the windows it would reset — showing it once per card would put three
controls on screen for one irreversible action.

`null` means there is nothing to show, and the correct rendering is **nothing at
all** — no badge, no zero. That covers every credential on a provider with no
such concept and every Codex account that has not been granted one, which is the
ordinary case.

```json
"resetCredits": {
  "availableCount": 2,
  "expiresAtEpoch": 1790251200,
  "expiresInSeconds": 1238400,
  "redeemable": true
}
```

| Field | Meaning |
| --- | --- |
| `availableCount` | At least 1 whenever this object exists. |
| `expiresAtEpoch` / `expiresInSeconds` | The soonest credit that can still be spent. **`null` when the provider did not date it** — render that differently from a distant deadline rather than implying safety. |
| `redeemable` | Whether this dashboard may spend it. |

**A banked reset lapses thirty days after it is granted**, which is the
documented way operators lose them, so the deadline is carried beside the count
rather than left to be discovered. quota-cache reads it from a second endpoint
and only when the count is non-zero; a credit it could not date still reports
its count.

**Never offer redemption without `redeemable`.** It is false when redeeming is
switched off in configuration (`allow-redeem: false`), when CPA has the
credential parked or disabled, and when the credential is not one this plugin
can authenticate — an API-key login has no reset credits at all. The count still
shows in every one of those cases; only the control goes.

### Spending one — `POST .../redeem`

The one route on this plugin that changes anything, and the one place it
contacts a provider. It exists on both doors, authenticated exactly as
`/summary` is on each:

| Route | Auth |
| --- | --- |
| `POST /v0/management/plugins/quota-glance/redeem` | CPA management key |
| `POST /v0/resource/plugins/quota-glance/redeem` | `Authorization: Bearer <web-token>` |

Only the management route works today. The plugin registers the resource route,
but current CPA dispatches only GET to resource routes, so a POST there returns
404 before the plugin sees it. A client without a CPA console session must hide
the button rather than call the resource route. `make smoke` reports which
behaviour the running CPA has, and the resource route starts working with no
plugin change if CPA ever dispatches POST there.

```json
{ "credentialId": "codex-noor@example.com.json", "confirmed": true }
```

`confirmed` must be `true`. The dialog belongs in the client, but a request that
arrives without an answer to it is refused rather than assumed — spending a
credit cannot be undone. The body must be JSON; a form-encoded content type is
rejected, because a form post is the one a cross-site page can make without
script. The credential must be one the served document already reports as
`redeemable`, so a caller cannot nominate a credential the dashboard is not
offering.

```json
{ "outcome": "reset", "windowsReset": 2, "remainingCount": 1, "snapshotPending": true }
```

| `outcome` | Meaning |
| --- | --- |
| `reset` | The credit was spent and windows were cleared. |
| `nothingToReset` | The provider accepted it but no window needed clearing. **The credit is still gone** — it is consumed on acceptance, and saying otherwise invites a second press. |
| `noCredit` | Nothing was left to spend; the count on the card was out of date. Nothing was consumed. |

`snapshotPending` is `true` because the count on the card comes from
quota-cache's snapshot and will not move until its next poll. Say so rather than
letting the dashboard silently disagree with itself.

Failures carry a fixed code and never provider text: `provider_refused`,
`provider_unavailable`, `already_in_flight`, `not_redeemable`,
`credential_unusable`, `confirmation_required`, `invalid_request`. A `404` means
redeeming is switched off. **A request that never completed is not a request
that did nothing** — the credit may have been spent, so report the uncertainty
instead of inviting a retry.

### `level` — computed server-side, on both rows and entries

`critical` below 20% remaining, `low` below 40%, otherwise `ok`. The design
turns the bar red at `critical`. **Do not recompute this threshold in CSS**; if
the rule changes it changes here, once.

### `trend` — `up`, `down`, `flat`, `unknown`

Compares the row now against the same members an hour ago; more than ±2 points
of movement is `up`/`down`. `unknown` when there is too little history — fewer
than two samples spanning half an hour, which is the normal state for the first
half hour after a restart. Hide the arrow entirely on `unknown`.

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

A credential is a member of a row if it reported that window. That is the whole
test: not whether CPA will route to it, not whether it is disabled. The figures
a parked credential last reported are still true, and a credential vanishing
from every card at the exact moment it runs out is the opposite of what the card
is read for.

**A credential that did not report a window is excluded from the mean, not
counted as full** — otherwise one silent credential quietly inflates the single
number the whole card is read from. It is still listed, with `hasReading: false`.

`memberCount` is how many the mean covers and `excludedCount` the rest; they sum
to `credentialCount`, which is also `entries.length`. Neither is ever negative.

`aggregate.remainingFraction` is the arithmetic mean over members.
`soonestResetAtEpoch` is the earliest *future* reset among them, and
`projectedGainPercent` is the capacity the row regains when it fires, summed
over every member resetting in that same minute. `subtext` is that sentence
already written out — it is empty when no member has a future reset.
