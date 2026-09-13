# Cached quota data

Quota Cache 0.1.4 adds an optional `quota` object to each `entries[provider:auth_index]` record in `snapshot.json`. The same records are returned by authenticated `GET /v0/management/plugins/quota-cache/status`. No extra provider requests are made for these fields.

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
| `truncated` | True if a provider response exceeded the bounded entry count |

Every window can contain `used_percent`, `duration_seconds`, `starts_at`, `resets_at`, and `period`. Unavailable values are omitted. Unlike the compatibility percentage, extended percentages preserve reported values above 100. Absolute timestamps use UTC; relative reset times are anchored to the observation, never recalculated on reads.

Every balance contains `unit` and may contain `used`, `limit`, `remaining`, `used_percent`, `remaining_percent`, `resets_at`, `source`, `enabled`, `has_credits`, and `unlimited`. Amounts are decimal **strings**, preserving precision and any reported negative balance. Missing values mean unknown; `"0"` and `false` are explicit values. No remaining amount is inferred from a percentage or subtraction. `provider_units` means the endpoint does not provide a verified currency/unit contract; do not display it as dollars.

## Provider mappings

| Provider | Cached fields from its existing response |
| --- | --- |
| Claude | `windows.five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`, `seven_day_oauth_apps`, `seven_day_cowork`, when supplied; `balances.extra_usage` with enabled state, monthly limit, used credits, and utilization in `provider_units` |
| Codex | `windows.regular/primary` and `regular/secondary`; corresponding `limits.regular`; `code_review/primary` and `code_review/secondary` plus `limits.code_review`; `additional/<limit-name>/primary` and `/secondary` with associated flags; `balances.credits` with remaining credits, has-credits and unlimited flags; plan type; `limits.spend_control` and `balances.spend_control` with used/limit/remaining amounts, percentages, reset, and source in `provider_units` |
| Grok | `windows.shared` percentage and billing-period start/end/type; `product/<product>` usage; `balances.included` used/monthly limit, `prepaid` remaining, and `on_demand` used/cap/enabled; amounts in `usd_cents`; unified billing and subscription tier |

Codex primary/secondary IDs describe provider slots; use `duration_seconds` to identify five-hour versus weekly windows, since the slots can change. Additional Codex limits accept array and object forms. Grok retains the response's period type rather than assuming a weekly cycle. A Grok money object `{}` means zero under its documented proto3 encoding; an absent/null object means unknown.

The extension is allowlisted: raw response bodies, tokens, headers, arbitrary nested objects, billing history, payment methods, and auto-top-up settings are not copied. No separate billing or auto-top-up endpoint is queried. At most 32 windows, 32 limit groups, and 32 additional/product input items are processed; labels are bounded to 64 characters. Unknown fields remain unsupported until explicitly mapped.

## Reading from a new plugin

Use `github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client`:

```go
q, err := client.ReadQuota(path, provider, authIndex, now, 30*time.Minute)
// err means absent, unsupported, stale, future-dated, or a failed/pending poll.
// q includes the last response's windows; some may already have reset.

w, err := client.ReadWindow(path, provider, authIndex, "regular/primary", now, 30*time.Minute)
// Also rejects absent, not-yet-started, or expired windows. Check optional fields before use.
```

`ReadFresh` remains the compatibility reader for the regular weekly/pool observation. Old snapshots without `quota` still work with that reader. Extended readers wait until a successful poll supplies their fields.

On a successful fetch, the extension is replaced as one observation, so fields omitted by the latest response disappear. On a failed fetch, previous values remain visible as last-known data, but `last_error` makes both new readers return unavailable. Balance reset times also need to be checked before acting on a spend allowance. Each window retains its own reset time; a still-current weekly observation does not make an expired five-hour window usable. Provider cooldowns, failure backoff, and the single-writer schedule are unchanged.

Future consumers should retain opt-in cache configuration and wait on unavailable data. This release adds read helpers, not a push/event-delivery protocol.

## Source evidence and validation

The provider usage endpoints are not stable public APIs. Mappings are grounded in existing response fixtures and the current official client source inspected during implementation:

- [Codex backend models](https://github.com/openai/codex/tree/main/codex-rs/codex-backend-openapi-models/src/models): rate-limit windows/groups and `CreditStatusDetails` (credit balance is a string).
- [Grok billing types](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/extensions/billing.rs): `BillingConfig`, `BillingConfigResponse`, `Cent` (USD cents), and period types.
- Claude OAuth response shapes in the repository's quota fixtures. Missing and malformed optional fields are omitted rather than guessed.

Tests cover provider mappings, numeric precision, explicit zero/false, missing values, bounded metadata, legacy/new readers, short-window-only accounts, expiry, restart persistence, failure retention, successful replacement, native status serialization, and sidebar rendering. Account Health and Reset Priority retain their existing weekly semantics.
