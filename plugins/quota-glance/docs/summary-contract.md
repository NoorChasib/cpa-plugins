# The summary document

`GET /v0/resource/plugins/quota-glance/summary` returns one JSON document. This
page is its reference: the vocabulary a client has to understand, and the
guarantees it can rely on. Two committed examples live under `testdata/golden/`:

| File | What it shows |
| --- | --- |
| `summary.json` | Seven healthy credentials. The layout the design was drawn against. |
| `summary-degraded.json` | Every degraded state a real deployment produces — stale, failed, never-polled, disabled, unavailable, unsupported, per-model rows, an unmapped window, a reset in the past, a window with no reset at all. |

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
- **`credentials[]` is pre-sorted** by each credential's `weekly` window reset,
  soonest first; those without one sort last. **Every row's `entries[]` repeats
  that order.** Do not re-sort.
- **`entries[]` contains only members** of that row, so it is usually shorter
  than `credentials[]`. Match on `credentialId`.
- **Times are integer Unix epoch seconds.** Nullable ones are `null`, never
  omitted and never zero. `generatedAtEpoch + resetInSeconds == resetAtEpoch`.
- **Additive evolution only.** New fields and new enum values may appear without
  a `schemaVersion` bump; handle an unknown value by falling through to a
  neutral rendering rather than failing.

## Vocabulary

### `credentials[].status` — does this credential produce data?

| Value | Meaning |
| --- | --- |
| `ok` | Polled successfully. |
| `error` | Its last poll failed. Last-known figures are still shown, marked. |
| `pending` | quota-cache knows about it but has not polled it yet. No data, not an error. |
| `unsupported` | quota-cache does not poll this provider. |
| `disabled` | Disabled in CPA. Catalogued, excluded from every row. |
| `unavailable` | Marked unavailable by CPA. Catalogued, excluded from every row. |

`counters.observedOK` counts `ok` only; `counters.observeError` counts `error`
only. The rest are in `counters.credentials` and in neither.

### `entries[].state` — is this particular reading trustworthy?

A different question from `status`, and a different set: `ok`, `error`, `stale`.
`stale` means the observation is older than the configured `stale-after`.

### `entries[].dataIssues` — always an array, often empty

| Value | Meaning |
| --- | --- |
| `percentOutOfRange` | The provider reported below 0 or above 100. Clamped. |
| `percentInvalid` | Not a finite number. Reported as no remaining capacity, because overstating headroom is the damaging direction. |
| `resetInPast` | The reset instant has passed and the next poll has not landed. Normal; render "resetting…", not a negative countdown. |
| `observeError` | The last poll for this credential failed. |
| `refreshPending` | A poll is in flight. Not an error. |
| `stale` | Older than `stale-after`. |

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

A credential is a member of a row only if it reported that window and is neither
disabled nor unavailable. **A credential that did not report a window is
excluded from the mean, not counted as full** — otherwise one silent credential
quietly inflates the single number the whole card is read from. `memberCount`
and `excludedCount` say what happened; `excludedCount` is never negative.

`aggregate.remainingFraction` is the arithmetic mean over members.
`soonestResetAtEpoch` is the earliest *future* reset among them, and
`projectedGainPercent` is the capacity the row regains when it fires, summed
over every member resetting in that same minute. `subtext` is that sentence
already written out — it is empty when no member has a future reset.
