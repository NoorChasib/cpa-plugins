# Cached quota data

Quota Cache 0.1.4 adds an optional `quota` object to each `entries[provider:auth_index]` record in `snapshot.json`. The same records are returned by authenticated `GET /v0/management/plugins/quota-cache/status`. The `quota` object is read from the usage response every poll already makes; the few fields that need a request of their own are listed under [Account details](#account-details) and [Requests per credential](#requests-per-credential).

The outer snapshot stays at schema 1. The nested `quota.schema` is 1 and has its own `observed_at`. Existing `used_percent`, `reset_at`, and `observed_at` keep their regular weekly/pool meaning for Account Health and Reset Priority. An account without that regular window has an empty legacy observation timestamp, even when additional limits are present. Existing consumers wait rather than treating another window as the weekly allowance.

## Common fields

| Field | Meaning |
| --- | --- |
| `schema` | Nested quota format version, currently 1 |
| `observed_at` | Time the successful provider observation was made; reads never advance it |
| `plan` | Provider-supplied subscription/plan label, when available |
| `active_limit` / `limit_reached_reason` | Codex active limit name and structured exhaustion reason, when supplied |
| `windows` | Named quota windows, described below |
| `limits` | Named availability flags: optional `allowed` and `reached` booleans and Codex `metered_feature` |
| `balances` | Named balances in explicitly stated units |
| `unified_billing` | Grok's shared billing flag, if supplied |
| `reset_credits` | Codex's or Claude's banked rate-limit resets, when the account holds at least one |
| `billing_period` | Claude only: `monthly` or `annual`, how often the subscription is billed, when the usage response states one |
| `cost_report` | Claude API credits only: the organization's daily spend, described under [Claude API credits](#claude-api-credits) |
| `truncated` | True if a provider response exceeded the bounded entry count |

Every window can contain `used_percent`, `duration_seconds`, `starts_at`, `resets_at`, and `period`. Unavailable values are omitted. Unlike the compatibility percentage, extended percentages preserve reported values above 100. Absolute timestamps use UTC; relative reset times are anchored to the observation, never recalculated on reads.

`reset_credits` contains `available_count` and may contain `soonest_expiry`, `hold`, and `hold_until`. It is omitted entirely unless the account holds at least one banked reset, so its presence is the signal that there is one to spend. A banked reset is an entitlement, not allowance: spending one clears the account's session and weekly windows (for Codex it also moves the weekly reset date).

- **Codex**: `available_count` is the usage response's `rate_limit_reset_credits.available_count`. `soonest_expiry` is the earliest expiry among credits whose status is `available` and whose deadline is still ahead of the observation; it is omitted when the inventory could not be read, which never fails the observation. Codex never sets `hold`.
- **Claude**: read from the `cedar_ember` block of the same usage request (`?cedar_ember=1`). A grant needs an `id` of 1–40 characters `[a-z0-9_-]` and a whole `resets_left` from 0 to 32; one that does not is ignored on its own, as Claude Code ignores it, and a repeated id counts once. Other grant fields fall back to Claude Code's defaults when missing or malformed (`paused` false, `usable_now` false, `use_requires_limit` true, unreadable dates as none). `available_count` is the sum of `resets_left` over grants whose `ends_at` is not past; a grant that has not started yet still counts. `soonest_expiry` is the earliest future `ends_at` among them. The whole field is omitted when the block is missing, is not an object, has no boolean `eligible`, or adds up to zero. At most 32 grants are read; more sets `truncated`. Grant ids, labels, and the rest of the block are never stored, apart from `billing_period` below.

`billing_period` is read from the same block's `event_props.billing_period` on every poll, independently of `eligible` and the grants. Only `monthly` and `annual` are stored. `unknown`, any other string, a non-string, a null, and a missing or non-object `event_props` all leave the field out, and none of them can affect `reset_credits` or the poll. No other `event_props` field (the tier, the surface, and so on) is stored. It is the cadence a consumer needs to turn the profile's [subscription start](#account-details) into an estimated renewal date, which Anthropic does not report.

`hold` is the provider's own reason, at this observation, that none can be spent now. It is a hint, not a verdict: anything that spends a reset must check the provider again first. Claude sets it from the claim endpoint's rules, in this order:

| `hold` | When |
| --- | --- |
| `ineligible` | `eligible` is false |
| `cooldown` | `cooldown_until` is in the future; `hold_until` carries that instant |
| *(empty)* | Any live grant can be spent now: not paused, `usable_now`, started, and either `at_limit` or not `use_requires_limit` |
| `paused` | Otherwise, the grant a redemption would try first is paused (which includes every grant being paused) |
| `not_limited` | Otherwise, that grant needs the account to be at a limit and `at_limit` is false |
| *(empty)* | Any other refusal, such as `usable_now` false with no stated reason |

The grant a redemption tries first is `next_grant_id` when that names a live grant, otherwise the one with the soonest `ends_at` (undated last), ties broken by id.

Every balance contains `unit` and may contain `used`, `limit`, `remaining`, `used_percent`, `remaining_percent`, `resets_at`, `source`, `enabled`, `has_credits`, and `unlimited`. Amounts are decimal **strings**, preserving precision and any reported negative balance. Missing values mean unknown; `"0"` and `false` are explicit values. No remaining amount is inferred from a percentage or subtraction. `usd` is US dollars and `usd_cents` is US cents. `provider_units` means the endpoint does not provide a verified currency/unit contract; do not display it as dollars.

## Provider mappings

| Provider | Cached fields from its existing response |
| --- | --- |
| Claude | `windows.five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`, `seven_day_oauth_apps`, `seven_day_cowork`, when supplied; `balances.extra_usage` with enabled state, monthly limit, used credits, and utilization in `provider_units`; `reset_credits` and `billing_period` from the `cedar_ember` block |
| Codex | `windows.regular/primary` and `regular/secondary`; corresponding `limits.regular`; `code_review/primary` and `code_review/secondary` plus `limits.code_review`; `additional/<limit-name>/primary` and `/secondary` with associated flags; `balances.credits` with remaining credits, has-credits and unlimited flags; plan type; `limits.spend_control` and `balances.spend_control` with used/limit/remaining amounts, percentages, reset, and source in `provider_units`; `reset_credits.available_count` |
| Grok | `windows.shared` percentage and billing-period start/end/type; `product/<product>` usage; `balances.included` used/monthly limit, `prepaid` remaining, and `on_demand` used/cap/enabled; amounts in `usd_cents`; unified billing and subscription tier |
| OpenRouter | `balances.credits` in `usd`: `limit` is `total_credits` (all-time purchases) and `used` is `total_usage` (all-time spend). The balance is their difference and is not stored |
| Claude API credits (`anthropic-api`) | `cost_report`: the organization id and one entry per UTC day of the current credit cycle, each day's amounts in cents, verbatim. Nothing is summed. See [Claude API credits](#claude-api-credits) |

The OpenRouter entry exists only while `openrouter-management-key` is configured. It is not a CPA credential: it is keyed `openrouter:key-<fingerprint>`, where the fingerprint is the first 12 hex digits of the key's SHA-256, and the key itself is never written. It has no weekly window, so `used_percent`, `reset_at`, and `observed_at` stay empty and `quota.observed_at` dates the observation. It contributes no canonical `windows`. `GET https://openrouter.ai/api/v1/credits` is the only request, and nothing else in its response is kept.

Codex primary/secondary IDs describe provider slots; use `duration_seconds` to identify five-hour versus weekly windows, since the slots can change. Additional Codex limits accept array and object forms. Grok retains the response's period type rather than assuming a weekly cycle. A Grok money object `{}` means zero under its documented proto3 encoding; an absent/null object means unknown.

The extension is allowlisted: raw response bodies, tokens, headers, arbitrary nested objects, billing history, payment methods, and auto-top-up settings are not copied. No separate billing or auto-top-up endpoint is queried. One further Codex endpoint is read per poll, and only to date banked resets: when `rate_limit_reset_credits.available_count` in the usage response is non-zero, `GET /wham/rate-limit-reset-credits` supplies `soonest_expiry`. If that request fails, including with 429, only `soonest_expiry` is lost: the poll is still a success recorded with the usage request's `http_status`, and the provider is not paused. Credit ids, titles, and statuses are read and discarded; only the one timestamp is cached. At most 32 windows, 32 limit groups, 32 reset grants, and 32 additional/product input items are processed; labels are bounded to 64 characters. Unknown fields remain unsupported until explicitly mapped.

Claude's usage request is `GET /api/oauth/usage?cedar_ember=1`, which adds the reset-grant block to the same response. Claude Code also sends `skip_spend=1` on that path; Quota Cache does not, because it skips the spend-store read that fills `extra_usage`, which is collected from this response. Every other field read from the response is unchanged by the query.

## Canonical windows and subscription identity

Quota Cache 0.1.5 adds four optional fields directly on each entry: `windows`, `plan`, `tier_name`, and `renewal_at`. The outer snapshot stays at schema 1 and every field is additive, so consumers built before this release decode the snapshot unchanged and need no rebuild. `used_percent`, `reset_at`, and `observed_at` keep their regular weekly/pool meaning and are never repurposed.

`windows` is the same observation projected into a canonical vocabulary, so no consumer pattern-matches a provider string. The verbatim per-provider projection remains under `quota.windows`.

| Key | Meaning |
| --- | --- |
| `session` | Short rolling window; Claude's five-hour limit |
| `weekly` | The regular multi-day window; mirrors the entry's `used_percent` |
| `weekly_fable` | Claude's separate seven-day Fable/Opus allowance |
| `model_session` | Per-model or per-feature short window; `model` is set |
| `model_weekly` | Per-model or per-feature multi-day window; `model` is set |
| `credits` | A consumable balance rather than a rate window |
| `monthly` | Calendar-month window, where a provider exposes one; no current provider maps to it |

A window with no canonical meaning is emitted as `raw:<provider>:<original id>` with the upstream identifier as its `title`, rather than dropped, so an unrecognized window is visible before it is mapped.

Each window carries `key`, `used_percent`, `observed_at`, and optionally `title`, `model`, `reset_at`, and `last_error`. `used_percent` is **used** capacity on 0-100 and is clamped exactly like the entry's `used_percent`; a consumer showing remaining capacity inverts it once. Unlike `quota.windows`, values above 100 are not preserved here. `observed_at` is per window because windows for one credential can be observed at different times, and an entry-level timestamp alone would overstate freshness. Windows are emitted in a fixed order (`session`, `weekly`, `weekly_fable`, `model_session`, `model_weekly`, `credits`, `monthly`, then `raw:`, each tie broken by model) so a poll that changes nothing rewrites nothing.

A window whose percentage is missing is omitted rather than published as zero used, which would read as full remaining capacity. It stays visible under `quota.windows`.

Each provider's top-level observation corresponds to one canonical key: `weekly` for Claude and Codex, and `credits` for Grok, whose headline figure is a consumable balance rather than a rate window. When the top-level observation succeeded but no canonical window of that key was produced, one is synthesized from the top-level fields. That covers a response with no extended windows at all, and the subtler case where the extended parser rejected a percentage the top-level parser accepted — without it a credential would look fully observed yet be absent from the row its own `used_percent` describes. A credential whose top-level observation failed gets no synthesized window. Grok therefore reports `credits` and never `weekly`; a consumer that orders by weekly reset has nothing to order it by and should place it last rather than treat it as missing data.

Two upstream windows can reduce to one canonical identity — Codex declares a duration per slot, and both slots of a group can declare the same one. The first in a fixed order keeps the canonical key and the rest are demoted to `raw:`, so a consumer grouping by key never counts one credential twice.

Current mappings: Claude `five_hour`, `seven_day`, `seven_day_opus`, and `seven_day_sonnet` map to `session`, `weekly`, `weekly_fable`, and `model_weekly`; other Claude windows are `raw:`. Codex maps by the window's declared duration, never by its primary/secondary slot, because the slots are not stable: the `regular` group becomes `session`/`weekly` and every other group becomes `model_session`/`model_weekly` carrying its limit name as `model`. Grok's `shared` pool is `credits`, since it is a consumable balance rather than a rate window, and `product/<id>` is `raw:`.

`plan` and `tier_name` come from Claude's `subscription.plan`/`subscription.tierName`, Codex's `plan_type`, and Grok's `subscriptionTier`, validated by the same bounded label rules as every other name. Where the account endpoints below have supplied a plan, it takes precedence: Claude's profile token and Grok's subscription display name. When nothing else names a plan, the tier recorded in the stored credential is used. `renewal_at` is set for Codex only. Anthropic reports no renewal date for a Claude subscription, so a Claude entry has none: its subscription start is kept under `account_details` instead, for a consumer to estimate from. Codex's `renewal_at` is when the subscription is paid up to: the subscription endpoint's `active_until` when one has been read, else the id_token's `chatgpt_subscription_active_until` claim, which costs nothing because the credential is already read, else the spend-control limit's reset instant from the usage response. The first two are used only while they are still ahead of the poll.

## Account details

Some facts about an account change about once a month and are not in the usage response, or not reliably: Claude's plan and when its subscription began, Codex's subscription renewal, and Grok's plan name. Quota Cache reads them from the endpoints the CPA management centre uses for its own cards, on a schedule of their own:

| Provider | Request | Read |
| --- | --- | --- |
| Claude | `GET https://api.anthropic.com/api/oauth/profile` | `plan` as a token: `team`, `enterprise`, `max_20x`, `max_5x`, `max`, `pro`, or `free`; `subscription_started_at` from `organization.subscription_created_at` |
| Codex | `GET https://chatgpt.com/backend-api/subscriptions?account_id=<id>` | `renewal_at` from `active_until` (unix seconds as a number or string; RFC 3339 and milliseconds are also accepted) |
| Grok | `GET https://cli-chat-proxy.grok.com/v1/settings`, and `GET /v1/user?include=subscription` only when settings names no plan | `plan` from `subscription_tier_display` (for example `SuperGrok Heavy`), else `subscriptionTier` |

Claude's token follows Claude Code: `organization.organization_type` `claude_team` or `claude_enterprise` names the seat while `subscription_status` is `active` or absent; `claude_max`, `account.has_claude_max`, or a `rate_limit_tier` naming `claude_max` is Max, with `max_20x` or `max_5x` taken from the rate-limit tier and `max` when it does not say; `claude_pro` or `account.has_claude_pro` is Pro; both account flags explicitly false is `free`. A lapsed Team or Enterprise falls through to the account flags, as in the CPA management centre, because its member can still hold a personal plan.

`subscription_started_at` is when the organisation's subscription began, the field Claude Code itself copies as `subscriptionCreatedAt`. It is read from the same profile response as the plan, so it costs no request. RFC 3339 is expected, and unix seconds or milliseconds as a number or a numeric string are accepted too, as for Codex's `active_until`. Zero or negative, more than one day in the future (more than clock skew), or more than 20 years in the past is not a start, and is treated as absent. It is the start, not a renewal: it is never written to `renewal_at`, and stays as it is once it has passed. Anthropic reports no renewal date, and neither Claude Code nor the CPA management centre shows one. A consumer can estimate one from this start and `quota.billing_period`, as the next monthly or yearly anniversary, but it must present the result as an estimate, because a billing date that has moved since the subscription began cannot be seen here.

The rules:

- Each credential's account endpoints are asked at most once every six hours, and only after a successful usage poll. A failed or rate-limited poll asks nothing further.
- They run after the usage response has been parsed, and their failures are discarded. A refused, failed, or rate-limited account request never fails the poll, never sets `last_error`, never backs the credential off, and never pauses the provider; the poll's `http_status` is never an account request's.
- A read that fails — a transport error, a non-2xx status, an unreadable body — keeps the last good value. A read that answers but no longer names the value (no plan, no `active_until` still ahead, no plausible `subscription_created_at`) clears it, so the weaker sources apply again rather than a lapsed plan or a passed date standing indefinitely. Grok's plan is cleared only when both of its endpoints answered without one. The six-hour clock restarts either way, so a failing endpoint is not asked on every poll.
- A Codex renewal outranks the next source only while it is still ahead: an `active_until` or id_token claim already behind the poll — a token minted before the last renewal, a stored date that has since passed — gives way to the next source down rather than replacing a date that is still to come.
- What was read is persisted on the entry as `account_details`: `checked_at` (when the endpoints were last asked), `plan`, `renewal_at`, and `subscription_started_at`. A restart therefore does not re-read every credential. `plan` and `renewal_at` on the entry already include these values, so consumers need not read `account_details` for them. `subscription_started_at` exists only here, so a consumer that wants it reads it from `account_details`. The profile's plan and start are judged separately: a profile naming a plan with no usable start clears the start and keeps the plan, and the reverse also holds.
- Every stored label passes the same bounded validation as every other name, both when read and when applied from the snapshot.

## Requests per credential

At the default 15-minute `poll-interval`, one credential costs at most:

| Provider | Usage polls per day | Other per-poll requests | Account details per day | Total per day |
| --- | --- | --- | --- | --- |
| Claude | 96 | none (reset grants ride on the usage request) | 4 profile | 100 |
| Codex | 96 | up to 96 inventory reads, only while resets are banked | 4 subscription | 100, or up to 196 while resets are banked |
| Grok | 96 | none | 4 settings, plus up to 4 user reads when settings names no plan | 100 to 104 |
| OpenRouter | 96 | none | none | 96 |
| Claude API credits, per organization | 96 | none normally; `GET /v1/organizations/me` only when the cost report lacks its `anthropic-organization-id` header, and a further cost-report page only if Anthropic pages a window it should return in one | none | 96 |

Reading Claude's subscription start and billing period added no requests. The start comes from the profile response already read for the plan, and the billing period from the usage response already read every poll, so the figures above are unchanged.

Before account details were read, the same credentials cost 96 (Claude), 96 to 192 (Codex), and 96 (Grok) per day. The first poll after upgrading reads each credential's account details once; after that, restarts add nothing. A longer `poll-interval` lowers the usage figures; account details stay at no more than four reads a day, and fall below that once the interval exceeds six hours, since they are only read after a poll.

## Polling schedule and failures

`request-spacing` (default 10 seconds) is the gap between one poll and the next, across every credential. `poll-interval` (default 15 minutes) is the least time between two polls of the same credential. The follow-up requests a poll can make, Codex's reset inventory and the account details, run straight after it and are not spaced.

- Polls are sent on a timer that ticks once per spacing. Ticks wake a few milliseconds earlier or later than one another, so a tick is admitted up to a tenth of the spacing, and at most one second, before the spacing is over. Before 0.1.12 a tick that woke a moment early was skipped, and many gaps were twice the spacing. The next slot is counted from the previous slot rather than from an early tick, so two polls are never closer than the spacing less that tolerance, and never more frequent than the spacing on average.
- A credential falls due one interval after its last poll and is polled at the first free tick after that, so its gaps run a little over the interval.
- Every credential has to fit into an interval. With N credentials, N times `request-spacing` should be well below `poll-interval`, which leaves room to catch up after a pause. For example, twelve credentials at a 30-second spacing need six minutes per round and cannot all be read every five minutes; at a 10-second spacing they need two.

When a poll fails:

- The previous observation is kept and `last_error` is set. The credential is retried one interval later, and each further consecutive failure doubles that, up to six hours.
- A 429 (`last_error` `provider rate limited`, poll outcome `rate_limited`) pauses every credential of the same provider for one interval, and at most six hours. `anthropic-api` is the exception: a 429 there pauses only the organization that got it ([Claude API credits](#requests-and-validation)). The pause is `provider_cooldown`. It never takes the rate-limited credential's own backoff, which also counts that credential's earlier failures: before 0.1.12, a credential that had been answering 401 for a day could pause all of its siblings for six hours with one 429.
- The rate-limited credential's own backoff doubles as above but stops at one hour, or at the interval when that is longer, and at most six hours. A 429 is about the provider rather than the credential, and an hourly retry is what notices it has lifted. Once a credential's backoff has reached that cap, it draws at most one 429 an hour. While the backoff is still doubling it can draw more, so at a short interval the first few hours of a long outage cost some extra 429s; every 429 pauses the provider, though, so the provider never draws more than one per pause.
- A successful poll does not shorten its siblings' backoff. After a long outage, each credential is therefore read again only when its own backoff runs out, which can be up to an hour after the limit lifts (or one interval, if that is longer), though the first is usually read much sooner. Bringing the others forward on a success would cost a 429 and a provider pause each time the limit is on one account rather than the whole provider.
- A `Retry-After` longer than the pause or the backoff replaces it, for both.
- Only the poll's own request decides its outcome and `http_status`. A failure or 429 from an account-details or reset-inventory request never fails the poll, never counts as a rate limit, and never pauses the provider. A Claude API credit poll is one reading made of several requests, so every one of them is the poll's own ([Claude API credits](#requests-and-validation)).

Each failed poll also writes one line to CPA's log through the plugin host, so failures and rate limits can still be dated after the 100-poll `history` has moved on. The plugin's own provider requests never appear in CPA's request log. A 429 is logged at `warn` as `quota-cache poll rate limited; this provider's credentials are paused`, or, for an `anthropic-api` organization, which pauses nothing else, as `quota-cache poll rate limited; this credential is retried at next_attempt`. Any other failure is logged at `info` as `quota-cache poll failed; this credential is retried at next_attempt`. The fields are:

| Field | Meaning |
| --- | --- |
| `provider` | `claude`, `codex`, `xai`, `openrouter`, or `anthropic-api` |
| `auth_index` | the credential's opaque index, the one its snapshot entry is keyed by; for `anthropic-api`, `label-<12 hex>` or `item-<n>`, never the admin key |
| `http_status` | the poll's HTTP status; absent when no response arrived |
| `retry_after` | the instant the provider's `Retry-After` named; present only for a 429 that sent one |
| `next_attempt` | when this credential is polled next |
| `provider_paused_until` | when the provider's other credentials resume; present only for a 429 that paused them, so never for `anthropic-api` |

Times are RFC 3339 in UTC. No line names an email, token, auth file name, URL, or response body. A successful poll logs nothing, so there is at most one line per poll.


## Claude API credits

Quota Cache 0.1.13 adds `anthropic-api` entries, with `api_credit` and `quota.cost_report`, and the `claude-api-credits` setting. Every earlier version rejects that setting and stops polling, so update before adding it. The snapshot stays on schema 1, and consumers that look entries up by CPA credential, such as Account Health, Reset Priority and Quota Glance before 0.7.0, never see these entries.

Quota Cache can also read the spend of each Claude Console organization that receives a Max or Team plan's monthly API credit. The organizations are configured in Quota Cache's own `claude-api-credits` list, not held by CPA, so their entries exist only while configured. The provider is `anthropic-api`, separate from `claude`: a 429 from Anthropic's Admin API pauses only the organization that got it, never another organization or subscription polling, and a subscription 429 never pauses it.

### Configuration

Each item has four keys, all required: `label`, `admin-key`, `monthly-usd`, and `renews`. The list is read from YAML only. CPA's plugin panel has no way to describe a list of objects, mark a value secret, or check one item, so it is not offered there; a panel save keeps it, rewritten through JSON.

Each item is judged on its own, so one bad item never stops the others. Every item becomes an entry. An item with a problem has `api_credit.problem` set and is never polled, never fails, and never backs off. Only the first problem is reported, in this order:

| Problem | When |
| --- | --- |
| `item_invalid` | The item is not a mapping, a value is not a scalar, or a key appears twice |
| `unknown_field` | The item has a key other than the four |
| `too_many_items` | The item is the 17th or later |
| `label_missing` / `label_invalid` / `label_duplicate` | The label is empty; over 64 characters or containing a character Go's `unicode.IsPrint` rejects (a control character, any space other than the ASCII space, such as a no-break space, or an invisible format character such as the zero-width joiner, so a joined emoji sequence is rejected too); or the same as an earlier item's, ignoring case |
| `admin_key_missing` / `admin_key_invalid` / `admin_key_repeated` | The key is empty; not shaped `sk-ant-` followed by 10 to 250 of `A-Z a-z 0-9 _ -`; or the same as an earlier item's |
| `monthly_usd_missing` / `monthly_usd_invalid` | The amount is empty, or not a non-negative number of dollars with at most two decimal places |
| `renews_missing` / `renews_invalid` | The date is empty, or not a real date from 2000 to 2099 written `2026-10-29` or `2026-10-29T00:00:00Z` |

A value YAML reads as null (`null`, `~`, an empty value, or `!!null`) counts as missing. A quoted `"null"` is text. Values are read as written, after trimming spaces, so an unquoted `renews: 2026-10-29` or `monthly-usd: 200` keeps its text. The key shape check does not decide whether a key works; Anthropic does, with 401 or 403.

### Entry

The entry is keyed `anthropic-api:label-<12 hex>`, the first 12 hex digits of the SHA-256 of the label, trimmed and lower-cased (`client.APICreditAccount`). Rotating the key or correcting the amount or date keeps the entry; renaming the label starts a new one. An item without a usable label is keyed `anthropic-api:item-<position+1>`. Like OpenRouter's, the entry has no weekly window: `used_percent`, `reset_at`, and `observed_at` stay empty, it contributes no canonical `windows`, and `quota.observed_at` dates the reading.

`api_credit` is the configuration, rewritten at every scan, so it is current even for an item that has never been polled:

| Field | Meaning |
| --- | --- |
| `label` | The configured label; empty when it is missing, invalid, or a duplicate |
| `position` | The item's zero-based place in the list |
| `monthly_usd` | The monthly credit in **dollars**, exactly as configured |
| `renews` | The configured renewal date, exactly as written |
| `key_fingerprint` | `key-` and the first 12 hex digits of the key's SHA-256; never the key |
| `problem` | The first configuration problem, or absent |

`quota.cost_report` is the last successful reading. Like every observation it is replaced whole by the next success and kept, under `last_error`, by a failure:

| Field | Meaning |
| --- | --- |
| `organization_id` | The organization the key belongs to, from the response's `anthropic-organization-id` header, or from `GET /v1/organizations/me` when the header is absent |
| `key_fingerprint` | The fingerprint of the key that made this reading. When it differs from `api_credit.key_fingerprint` the key has changed since, and the reading may be another organization's |
| `starting_at` / `ending_at` | The window asked for: 00:00 UTC on the first day of the cycle containing the poll, and 00:00 UTC on the day after the poll |
| `days` | One entry per UTC day from `starting_at`, without a gap, each `{starting_at, amounts: [{amount, currency}]}`. A day with no spend has `amounts: []` |

`amount` is a decimal string in the **lowest unit** of `currency`, which for USD is **cents**: `"1250"` is $12.50, and fractions of a cent occur. `monthly_usd` is in dollars. Quota Cache adds nothing up.

The credit cycle is `client.CreditCycleAt(renews, t)`: it starts at 00:00 UTC on the most recent occurrence of `renews`' day of the month and ends at the next one, clamped to short months (a day of 31 falls on Nov 30 and Feb 28 or 29) and re-anchored every month. Consumers must use the same function. The whole renewal day counts toward the new cycle, although Anthropic deposits the credit "shortly after" payment, so spend that day before the deposit can overstate the new cycle by part of a day.

### Requests and validation

One poll sends `GET https://api.anthropic.com/v1/organizations/cost_report?starting_at=<cycle start>&ending_at=<next 00:00 UTC>&bucket_width=1d&limit=31` with `x-api-key`, `anthropic-version: 2023-06-01`, and the User-Agent `cpa-plugins-quota-cache/<version> (https://github.com/NoorChasib/cpa-plugins)`. The window never exceeds 31 daily buckets, so one page covers it; `has_more` is followed for at most three pages. `GET /v1/organizations/me` is sent only when the response has no usable `anthropic-organization-id`. `totals.requests` counts one per poll, which under-counts only those two rare cases.

Every request of a poll is part of its one reading, so each counts toward the poll's outcome, unlike the optional follow-up requests of other providers. The poll's `http_status` is that of the first response that is not 2xx, or 200 when all are; a 401, 403, 404 or 5xx on a later page or on `/me` fails the poll as it would on the first page; and a 429 on any of them makes the poll `rate_limited`, with that response's `Retry-After`.

A reading is stored only when it is whole. Each bucket must start at 00:00 UTC, last exactly one day, and follow the previous one without a gap from the cycle's first day; each `amount` must be a decimal string of at most 64 characters and each `currency` three capital letters; and every complete day before today must be present. Today's bucket may be missing, since it is still in progress; a consumer tells that from `client.CostReport.CoveredUntil`, never by treating the day as $0. A bucket at or after `ending_at` is dropped without failing the poll. Anything else fails the poll and keeps the previous reading.

| `last_error` | Cause |
| --- | --- |
| `admin key rejected` | HTTP 401: the key is malformed, revoked, or expired |
| `admin key not permitted` | HTTP 403 |
| `cost report unavailable` | HTTP 404 |
| `request refused` | HTTP 400 or any other 4xx but 429 |
| `anthropic server error` | HTTP 5xx, including 529 |
| `unreadable response` | A 2xx whose body breaks the rules above |
| `provider rate limited` | HTTP 429, on any request of the poll |
| `quota fetch failed` | No HTTP response |

These are fixed strings. No response body, header, request id, or Anthropic message is stored or logged, and the key is never written to the snapshot, the status route, the history, or the log.

Scheduling is the same as every other account's, with four additions. A change to an item's key, renewal date, or problem makes it due at the next free slot; only a new key also clears its failures. After each poll the next one is brought forward to the end of the current cycle, so a renewed organization is read again at once; a later `Retry-After` still wins. A 429 never pauses the provider, and never sets `provider_cooldown`: each organization has its own Admin API key and its own Anthropic rate limits, and Anthropic describes a spend-cap refusal as a 429 without `Retry-After` that goes on failing, so a provider pause would stop unrelated organizations being read. The organization that got the 429 backs off as any rate-limited credential does, doubling from one interval to at most an hour (or one interval, if that is longer), and waits for a longer `Retry-After` when Anthropic sends one; every other organization keeps its schedule.

The cost report is gross organization spend: what the credit paid and what purchased credit paid alike, so anything above the credit is overage. Priority Tier usage is not in it. Anthropic says new usage typically appears within five minutes. Source: Anthropic's [Usage and Cost API](https://platform.claude.com/docs/en/manage-claude/usage-cost-api) and [Get Cost Report](https://platform.claude.com/docs/en/api/beta/organization/cost_report/retrieve), checked 2026-10-09.

## Reading from a new plugin

Use `github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client`:

```go
q, err := client.ReadQuota(path, provider, authIndex, now, 30*time.Minute)
// err means absent, unsupported, stale, future-dated, or a failed/pending poll.
// q includes the last response's windows; some may already have reset.

w, err := client.ReadWindow(path, provider, authIndex, "regular/primary", now, 30*time.Minute)
// Also rejects absent, not-yet-started, or expired windows. Check optional fields before use.
```

A plugin that renders a dashboard should use `client.Load` and make its own staleness judgement from `observed_at`, `last_error`, `next_attempt`, and `failures`. `ReadFresh`, `ReadQuota`, and `ReadWindow` all return unavailable for stale, failed, or expired data, which is correct for a consumer that must not act on stale evidence and wrong for one that should show a value with a staleness badge instead of a blank.

`ReadFresh` remains the compatibility reader for the regular weekly/pool observation. Old snapshots without `quota` still work with that reader. Extended readers wait until a successful poll supplies their fields.

On a successful fetch, the extension is replaced as one observation, so fields omitted by the latest response disappear. On a failed fetch, previous values remain visible as last-known data, but `last_error` makes both new readers return unavailable. Balance reset times also need to be checked before acting on a spend allowance. Each window retains its own reset time; a still-current weekly observation does not make an expired five-hour window usable. Provider cooldowns, failure backoff, and the single-writer schedule are unchanged.

Future consumers should retain opt-in cache configuration and wait on unavailable data. This release adds read helpers, not a push/event-delivery protocol.

## Source evidence and validation

The provider usage endpoints are not stable public APIs. Mappings are grounded in existing response fixtures and the current official client source inspected during implementation:

- [Codex backend models](https://github.com/openai/codex/tree/main/codex-rs/codex-backend-openapi-models/src/models): rate-limit windows/groups and `CreditStatusDetails` (credit balance is a string).
- [Grok billing types](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/extensions/billing.rs): `BillingConfig`, `BillingConfigResponse`, `Cent` (USD cents), and period types.
- Claude OAuth response shapes in the repository's quota fixtures. Missing and malformed optional fields are omitted rather than guessed.
- [OpenRouter: Get remaining credits](https://openrouter.ai/docs/api/api-reference/credits/get-remaining-credits): `data.total_credits` and `data.total_usage`, management key required. Unlike the endpoints above, this one is a documented public API.

Tests cover provider mappings, numeric precision, explicit zero/false, missing values, bounded metadata, legacy/new readers, short-window-only accounts, expiry, restart persistence, failure retention, successful replacement, native status serialization, and sidebar rendering. Account Health and Reset Priority retain their existing weekly semantics.
