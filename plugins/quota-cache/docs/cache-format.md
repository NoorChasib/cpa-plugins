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
| `cost_report` | Written only by Quota Cache 0.1.13 for Claude API credits; 0.1.14 never writes it and never polls those entries. See [Claude API credits](#claude-api-credits) |
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
| Claude API credits (`anthropic-api`) | Nothing from a response: the entry is never polled. `api_credit` carries the configuration, and the organization's metered usage is in the meter file beside the snapshot. See [Claude API credits](#claude-api-credits) |

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
| Claude API credits, per organization | 0 | none: the organization's traffic is counted from CPA's usage records | none | 0 |

Reading Claude's subscription start and billing period added no requests. The start comes from the profile response already read for the plan, and the billing period from the usage response already read every poll, so the figures above are unchanged.

Before account details were read, the same credentials cost 96 (Claude), 96 to 192 (Codex), and 96 (Grok) per day. The first poll after upgrading reads each credential's account details once; after that, restarts add nothing. A longer `poll-interval` lowers the usage figures; account details stay at no more than four reads a day, and fall below that once the interval exceeds six hours, since they are only read after a poll.

## Polling schedule and failures

`request-spacing` (default 10 seconds) is the gap between one poll and the next, across every credential. `poll-interval` (default 15 minutes) is the least time between two polls of the same credential. The follow-up requests a poll can make, Codex's reset inventory and the account details, run straight after it and are not spaced.

- Polls are sent on a timer that ticks once per spacing. Ticks wake a few milliseconds earlier or later than one another, so a tick is admitted up to a tenth of the spacing, and at most one second, before the spacing is over. Before 0.1.12 a tick that woke a moment early was skipped, and many gaps were twice the spacing. The next slot is counted from the previous slot rather than from an early tick, so two polls are never closer than the spacing less that tolerance, and never more frequent than the spacing on average.
- A credential falls due one interval after its last poll and is polled at the first free tick after that, so its gaps run a little over the interval.
- Every credential has to fit into an interval. With N credentials, N times `request-spacing` should be well below `poll-interval`, which leaves room to catch up after a pause. For example, twelve credentials at a 30-second spacing need six minutes per round and cannot all be read every five minutes; at a 10-second spacing they need two.

When a poll fails:

- The previous observation is kept and `last_error` is set. The credential is retried one interval later, and each further consecutive failure doubles that, up to six hours.
- A 429 (`last_error` `provider rate limited`, poll outcome `rate_limited`) pauses every credential of the same provider for one interval, and at most six hours. The pause is `provider_cooldown`. It never takes the rate-limited credential's own backoff, which also counts that credential's earlier failures: before 0.1.12, a credential that had been answering 401 for a day could pause all of its siblings for six hours with one 429.
- The rate-limited credential's own backoff doubles as above but stops at one hour, or at the interval when that is longer, and at most six hours. A 429 is about the provider rather than the credential, and an hourly retry is what notices it has lifted. Once a credential's backoff has reached that cap, it draws at most one 429 an hour. While the backoff is still doubling it can draw more, so at a short interval the first few hours of a long outage cost some extra 429s; every 429 pauses the provider, though, so the provider never draws more than one per pause.
- A successful poll does not shorten its siblings' backoff. After a long outage, each credential is therefore read again only when its own backoff runs out, which can be up to an hour after the limit lifts (or one interval, if that is longer), though the first is usually read much sooner. Bringing the others forward on a success would cost a 429 and a provider pause each time the limit is on one account rather than the whole provider.
- A `Retry-After` longer than the pause or the backoff replaces it, for both.
- Only the poll's own request decides its outcome and `http_status`. A failure or 429 from an account-details or reset-inventory request never fails the poll, never counts as a rate limit, and never pauses the provider.

Each failed poll also writes one line to CPA's log through the plugin host, so failures and rate limits can still be dated after the 100-poll `history` has moved on. The plugin's own provider requests never appear in CPA's request log. A 429 is logged at `warn` as `quota-cache poll rate limited; this provider's credentials are paused`. Any other failure is logged at `info` as `quota-cache poll failed; this credential is retried at next_attempt`. Claude API credit entries are never polled, so none of these lines is ever about one. The fields are:

| Field | Meaning |
| --- | --- |
| `provider` | `claude`, `codex`, `xai`, or `openrouter` |
| `auth_index` | the credential's opaque index, the one its snapshot entry is keyed by |
| `http_status` | the poll's HTTP status; absent when no response arrived |
| `retry_after` | the instant the provider's `Retry-After` named; present only for a 429 that sent one |
| `next_attempt` | when this credential is polled next |
| `provider_paused_until` | when the provider's other credentials resume; present only for a 429 |

Times are RFC 3339 in UTC. No line names an email, token, auth file name, URL, or response body. A successful poll logs nothing, so there is at most one line per poll.


## Claude API credits

Quota Cache 0.1.13 added `anthropic-api` entries and the `claude-api-credits` setting, and read each organization's spend from Anthropic's cost report with an Admin API key. Quota Cache 0.1.14 asks Anthropic nothing: it registers with CPA as a usage plugin, counts the Claude API-key traffic CPA serves per Console organization, and saves the count to a file of its own beside the snapshot. The snapshot stays on schema 1, and consumers that look entries up by CPA credential, such as Account Health, Reset Priority and Quota Glance before 0.7.0, never see these entries.

It needs **CPA v8.0.4 or newer (verified on v8.0.22)**. CPA gives each upstream attempt its own response headers from v7.2.142; on an older build a retry on another key can carry the earlier attempt's `anthropic-organization-id`, which would attribute spend to the wrong organization and teach the auth map a wrong link. Quota Cache 0.1.13 marks an item that has `organization-id` as `unknown_field` and does not read it; 0.1.12 and older reject `claude-api-credits` altogether and stop polling.

### Configuration

Each item has these keys. The list is read from YAML only: CPA's plugin panel has no way to describe a list of objects or check one item, so it is not offered there, and a panel save keeps it, rewritten through JSON.

| Key | Required | Rule |
| --- | --- | --- |
| `label` | yes | Trimmed, 1 to 64 characters, every one printable (`unicode.IsPrint`), unique ignoring case |
| `organization-id` | yes | The Console organization's id, from Console under Settings > Organization: a UUID written as `8-4-4-4-12` hex digits in either case, not all zeros, stored lower-cased (`client.NormalizeOrganizationID`). Braces, `urn:uuid:` and undashed forms are invalid. No version or variant bits are checked, because Anthropic's own example, `12345678-1234-5678-1234-567812345678`, has neither |
| `monthly-usd` | no | Absent, empty or null means unset. Otherwise a non-negative number of dollars with at most seven whole digits and two decimal places; `"0"` means no credit this cycle. An invalid value is ignored and flagged |
| `renews` | no | Absent, empty or null means unset. Otherwise a real date from 2000 to 2099 written `2026-10-29` or `2026-10-29T00:00:00Z`. An invalid value is ignored and flagged |
| `admin-key` | no | Accepted so a 0.1.13 configuration still loads. Its value is dropped unread: never validated, fingerprinted, stored, sent, logged or formatted. Its presence is flagged |

A value YAML reads as null (`null`, `~`, an empty value, or `!!null`) counts as missing. A quoted `"null"` is text. Values are read as written, after trimming spaces, so an unquoted `renews: 2026-10-29` or `monthly-usd: 200` keeps its text.

Each item is judged on its own, so one bad item never stops the others. Every item becomes an entry. An item with a problem has `api_credit.problem` set, and a consumer counts nothing for it. Only the first problem is reported, in this order:

| Problem | When |
| --- | --- |
| `item_invalid` | The item is not a mapping, a value is not a scalar, or a key appears twice |
| `unknown_field` | The item has a key other than the five |
| `too_many_items` | The item is the 17th or later |
| `label_missing` / `label_invalid` / `label_duplicate` | The label is empty; over 64 characters or containing a character Go's `unicode.IsPrint` rejects (a control character, any space other than the ASCII space, or an invisible format character such as the zero-width joiner); or the same as an earlier item's, ignoring case |
| `organization_id_missing` | `organization-id` is absent, empty or null |
| `organization_id_invalid` | It fails the shape rule |
| `organization_id_duplicate` | An earlier item has the same id; that item keeps it |

The 0.1.13 problems (`admin_key_missing`, `admin_key_invalid`, `admin_key_repeated`, `monthly_usd_missing`, `monthly_usd_invalid`, `renews_missing`, `renews_invalid`) are never written by 0.1.14. Instead `api_credit` carries three flags, none of them a problem: `monthly_usd_invalid`, `renews_invalid` and `admin_key_ignored`. `organization_id` is set whenever the configured value has a valid shape, whatever the problem, so a consumer can name a duplicate or an over-limit organization.

The meter counts an organization whose `organization-id` is valid, first-occurring and in the first 16 items, whatever else is wrong with the item, so fixing a label loses no history. While any item carries `admin-key`, each load logs once at `warn`: `quota-cache no longer uses admin-key in claude-api-credits; delete it from the configuration`, with the field `items`, how many do.

### Entry

The entry is keyed `anthropic-api:org-<12 hex>`, where the hex is the first 12 digits of the SHA-256 of the lower-cased organization id (`client.APICreditOrgAccount`), for an item whose organization is valid, the first with that id, and among the first 16. Any other item is keyed `anthropic-api:item-<position+1>`. Renaming the label, changing the credit or the date, or dropping `admin-key` keeps the entry, and with it the settings Quota Glance stores against it; naming a different organization starts a new one. 0.1.13's `label-…` entries are retired at the first 0.1.14 scan, and anything a 0.1.13 poll left on an entry is cleared then.

The entry is never polled: `used_percent`, `reset_at`, `observed_at`, `last_attempt`, `next_attempt`, `failures`, `last_error` and `quota` stay empty, and it contributes no canonical `windows`. `api_credit` is the configuration, rewritten at every scan:

| Field | Meaning |
| --- | --- |
| `label` | The configured label; empty when it is missing, invalid, or a duplicate |
| `position` | The item's zero-based place in the list |
| `organization_id` | The organization id, lower-cased; absent when missing or invalid |
| `monthly_usd` | The monthly credit in **dollars**, exactly as configured; absent when unset or invalid |
| `renews` | The configured renewal date, exactly as written; absent when unset or invalid |
| `monthly_usd_invalid` / `renews_invalid` | True when the value was present but invalid, and so ignored |
| `admin_key_ignored` | True while the item still has `admin-key` |
| `problem` | The first configuration problem, or absent |
| `key_fingerprint` | Written only by 0.1.13; 0.1.14 never writes it. An entry with it and no `organization_id` was written by 0.1.13 |

`quota.cost_report` and the `last_error` strings of the cost-report poll are written only by 0.1.13. Their types stay in the client package for one release, marked deprecated, so an older snapshot still decodes.

The credit cycle is `client.CreditCycleAt(renews, t)`: it starts at 00:00 UTC on the most recent occurrence of `renews`' day of the month and ends at the next one, clamped to short months (a day of 31 falls on Nov 30 and Feb 28 or 29) and re-anchored every month. Consumers must use the same function. The whole renewal day counts toward the new cycle, although Anthropic deposits the credit "shortly after" payment, so spend that day before the deposit can overstate the new cycle by part of a day.

### The meter file

The count is saved to `client.MeterPath(cache-path)`: the snapshot path with a final `.json` replaced by `.meter.json`, else with `.meter.json` appended, so `plugins/data/quota-cache/snapshot.meter.json` by default. It is written with mode 0600 in the cache's 0700 directory, by a temporary file, fsync, rename and a directory fsync, as the snapshot is, and only while Quota Cache holds the cache's writer lock. It is read by Quota Glance and by the sidebar's status route, which adds it as `api_meter` when it can be read. The snapshot, its 4 MiB limit and its write cadence are unchanged: the meter never goes into it, and a meter can never stop provider persistence.

`client.LoadMeter` reads it: a regular file of at most `MaxMeterBytes` (2 MiB), valid JSON, `schema` 1; nil maps and slices come back empty. Every time in it is UTC, truncated to the second.

| Field | Meaning |
| --- | --- |
| `schema` | 1 |
| `since` | When this file began counting |
| `started_at` | When this run of the meter started |
| `flushed_at` | When these figures were written. The meter saves within a minute of a change and at least every 10 minutes while it counts |
| `stopped_at` / `stop_reason` | Set while the meter is stopped: `shutdown`, `quiesce`, `disabled`, `no_items` or `failed`. Absent while it counts |
| `restarts` | How many starts carried on from this file |
| `received` | Usage records CPA delivered while counting, of every provider and auth type, cumulative since `since` |
| `counted` | Records attributed to a linked organization |
| `foreign` | Claude API-key records from a service other than Anthropic (below) |
| `rejected` / `last_rejected_at` | Records that could not be decoded, or carried a negative token count |
| `dropped` / `last_dropped_at` | Records dropped because the meter could not keep up (its intake holds 4,096) |
| `unattributed` / `last_unattributed_at` | Failed Claude API-key records with no organization header on an auth the meter does not know; the time is the last such record that carried tokens |
| `gaps` | Periods the meter was not counting, oldest first, at most 8: `{from, to, reason}`, with the stop reasons above or `unclean_stop` |
| `organizations` | Keyed by lower-cased organization id, at most 32 |
| `unlinked` | Organizations that sent traffic no linked item names, newest `last_seen_at` first, at most 8: `{organization_id, first_seen_at, last_seen_at, requests}` |
| `auths` | CPA auth index to the organization last seen on it, at most 64: `{organization_id, seen_at}`, with `organization_id` empty for an auth that is not Anthropic's |

Each organization holds `since` (when counting began for it), `unlinked_at` (set while dormant, below), `last_seen_at`, `last_success_at`, `refusals` and `last_refusal_at` (low-credit refusals of requests with no Claude Code session), `claude_code_refusals` and `last_claude_code_refusal_at` (refusals of requests from a Claude Code-based client, which includes the Agent SDK), `last_overflow_at` (the last time usage could not be kept under its own model, below), and two lists of buckets, `days` and `hours`, oldest first.

**Buckets.** A bucket is `{start, usage}`: the start of a UTC day (`days`, today and the 40 before) or a UTC hour (`hours`, this hour and the 72 before), and one `usage` entry per `(model, prompt)` pair, sorted by model then prompt. An entry carries `requests` (successful attempts), `failed` (failed attempts, refusals included), and the tokens Anthropic bills: `input` (excluding cache reads and writes), `output` (thinking included), `cache_read`, and `cache_write` (5-minute and 1-hour writes together, which CPA does not tell apart). The tokens of a failed attempt count, because Anthropic bills them. Every count saturates at 2^64 − 1. Zero fields are omitted.

A request lands in the day bucket and the hour bucket that contain its completion (the request time plus the latency, never after the time CPA delivered it), under the same key in both or in neither, so the hours of a day always sum to a subset of the day. That is what lets Quota Glance take a token baseline at the start of the hour of a Console reading (`MeterOrganization.UsageInHours`) and set it against the daily sums ever after (`UsageFromDay`); every credit cycle starts at 00:00 UTC, so daily sums from the cycle start are exact, and 40 days of them cover a reading entered late in a long cycle.

`model` is the model Anthropic's response named, else the one requested, lower-cased, and kept only when it matches `^[a-z0-9][a-z0-9._:@/-]{0,63}$` (`client.NormalizeMeterModel`); anything else is `(other)`. `prompt` is `over_100k` for a Claude Haiku 5.5 request whose prompt (input, cache read and cache write together) is over 100,000 tokens, which Anthropic prices higher, and absent otherwise (`client.MeterPromptClass`); no other model has a prompt-size tier on Anthropic's price list of 2026-10-09, and a new one needs a Quota Cache release.

**Size.** A bucket holds at most 16 named pairs; a 17th distinct pair in a day or hour bucket goes to `(other)` in both, and the organization's `last_overflow_at` is set. Across every bucket of every organization there are at most 6,000 usage entries; a request whose pair would need a new entry past that is counted but bucketed in neither bucket, with `last_overflow_at` set. With those caps and the ones in the table, the file stays under 2 MiB whatever CPA carries. Should it ever not, the oldest hour buckets are dropped across every organization one hour at a time, then day buckets older than 31 days one day at a time, re-encoding after each, and every organization that lost a bucket is marked overflowed. A meter is never refused for size.

**Retention.** At each event and at each save: day buckets older than 40 days and hour buckets older than 72 hours are dropped; dormant organizations, unlinked entries, auths and gaps older than 40 days are forgotten; auths over 64 lose the one seen longest ago; and gaps over 8 lose the oldest brief gap first (`client.MeterGap.Brief`: shorter than `client.MeterBriefGap`, 5 minutes), and only when every gap is long are the two oldest merged into one, so a gap is never lost, only coarsened, and two brief gaps never become one long span.

### What is counted, and how

CPA calls Quota Cache's `usage.handle` once per upstream attempt, synchronously, on the one goroutine every usage plugin shares; delivery is never replayed, so records published while Quota Cache is unloaded, quiesced, disabled or not declaring the capability are lost to it. The handler decodes the record and hands it to a worker without blocking: it never waits, never does I/O, and when its intake is full it counts a drop. The worker counts, prunes and saves. Every goroutine the meter starts recovers its own panics, because a panic in a native plugin aborts CPA itself: after one, the meter stops with `stop_reason` `failed`, writes the stop, logs once at `error`, and is replaced from its file at the next configure.

Only the record's `Provider`, `AuthIndex`, `AuthType`, `Model`, `ResponseModel`, `SessionID`, `RequestedAt`, `Latency`, `Failed`, `Failure.StatusCode`, `Failure.Body`, the four token counts and the `Anthropic-Organization-Id` response header are decoded. `Source` (the raw upstream API key), `APIKey` (the client's key), `AuthID`, `BaseURL`, `Alias`, `TraceID`, `RequestID` and `ParentSessionID` have no field to land in and are never materialized. The failure body is looked at only to tell a low-credit refusal (it says both "credit balance" and "too low", at any status), the session id only for its `claude:` prefix, and neither is kept. A record that does not decode, or carries a negative count, is `rejected`; a timestamp that does not parse is not a rejection, the delivery time stands in.

A record is counted only when `Provider` is `claude` and `AuthType` is `apikey`, ignoring case: a `claude-api-key` entry's traffic. Claude subscription traffic is `oauth` and is dropped before its headers are read, so the claude.ai organization it carries is never counted, listed or learned. Attribution then goes:

1. With a valid organization header: that organization, and the auth index is taught to map to it.
2. Without one, on an auth the meter knows: the organization the auth maps to; an auth known to be foreign makes the record `foreign`. Anthropic does not document whether its error responses carry the header, so this is what attributes a refusal that arrives without it.
3. Without one, a success on an unknown auth: `foreign`, and the auth is remembered as foreign. A `claude-api-key` entry pointed at another Anthropic-compatible service looks like this; its responses never carry the header.
4. Without one, a failure on an unknown auth: `unattributed`, dated when it carried tokens, since it may be a linked organization's spend.

An organization is **linked** while an item keys it. A linked organization's records are `counted`, bucketed, and move its last-seen, last-success and refusal fields. An organization no longer listed, after a typo or a removed item, becomes **dormant** (`unlinked_at` set): it keeps its buckets and keeps being bucketed for 40 days, is not `counted`, and appears under `unlinked` while it sends traffic, so fixing the typo re-links it with its history whole. An organization never listed appears under `unlinked` only. With no item configured at all the meter stops with `no_items` and the file stays; the next configure with items carries on from it with a gap.

**Stops and gaps.** Every stop is recorded (`stopped_at`, `stop_reason`) and becomes a gap at the next start, a clean `shutdown` included, because CPA stops a plugin while it keeps serving: traffic may have passed unseen. A configuration Quota Cache rejects (a schedule without a unit, a mistyped key, a changed cache path) stops the meter with `disabled` as well: CPA delivers no usage records to a plugin whose reconfigure failed until one succeeds, and the accepted configure that follows carries on from the file with that window as a gap. A meter whose file has `flushed_at` but no `stopped_at` ended without saying so, and its next start records an `unclean_stop` gap from the last save, which loses at most a minute of counts. A CPA restart therefore leaves a brief gap of a few seconds; Quota Glance ignores a gap under 5 minutes and shows the estimate as incomplete after a longer one until a Console reading anchors it again.

**Logging.** The meter writes these lines and no others: `info` `quota-cache API meter counting Claude API-key traffic` with `organizations` when it starts; `warn` `quota-cache could not read its API meter file and starts counting again` when the file is unreadable, over 2 MiB, bad JSON or another schema (it is replaced at the first save); `warn` `quota-cache API meter is dropping usage records; Claude API credit estimates will be low` with `dropped`, at most once per 10 minutes; `warn` `quota-cache cannot save its API meter; Claude API credit estimates may lose recent spend`, at most once per 10 minutes; and the `error` line above after a panic. No line names a key, a header, a body, a session id, a model or an organization id.

### Reading the meter from another plugin

```go
meter, err := client.LoadMeter(client.MeterPath(path))
// errors.Is(err, client.ErrMeterMissing): the Quota Cache writing this snapshot
// predates the meter or has not saved one yet; client.ErrUnavailable: unreadable.
org := meter.Organizations[organizationID]            // lower-cased id
cycle := org.UsageFromDay(cycleStart)                  // the cycle's usage, by (model, prompt)
baseline, covered := org.UsageInHours(dayStart, hourStart, meter.FlushedAt)
```

Price the tokens with your own table; the meter never does. Treat a gap that is not `Brief()`, a `stopped_at` older than `client.MeterBriefGap`, and movements of `dropped`, `unattributed`, `rejected` and `last_overflow_at` after your anchor as reasons the figure is a lower bound.

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
