// The summary document, mirroring plugins/quota-glance/internal/aggregate.
// docs/summary-contract.md is the reference; the short version is that
// everything here is already computed and this app only renders it.
//
// Enums are widened with `(string & {})` on purpose. The contract evolves
// additively and promises new enum values without a schemaVersion bump, so an
// unknown level or state has to fall through to a neutral rendering rather than
// fail to type-check or throw.

type Open<T extends string> = T | (string & {})

export const SUPPORTED_SCHEMA = 1

export type Level = Open<"ok" | "low" | "critical">
export type Trend = Open<"up" | "down" | "flat" | "unknown">
export type EntryState = Open<"ok" | "error" | "stale" | "noData" | "pending" | "unsupported">
export type ResetDisplayHint = Open<"countdown" | "none">
export type StaleReason = Open<
  "neverObserved" | "cacheMissing" | "cacheStale" | "snapshotSchemaUnsupported" | "rosterUnavailable"
>
export type CredentialStatus = Open<"ok" | "error" | "pending" | "unsupported" | "disabled" | "unavailable">

export interface Counters {
  credentials: number
  observedOK: number
  observeError: number
}

export interface Credential {
  id: string
  email: string
  provider: string
  plan: string
  status: CredentialStatus
  lastObservedEpoch: number
  /**
   * What CPA has routed here recently, or null when the host reports no such
   * counter at all — an older CPA. Null is not "nothing arrived": a credential
   * nothing arrived at has a full ring of empty buckets, and the difference is
   * the difference between "idle" and "unknown".
   */
  activity: Activity | null
  /**
   * Banked rate-limit resets this account holds, or null when it holds none —
   * which is every credential on a provider that has no such thing, and every
   * Codex or Claude account that has not been granted one. Null is the signal
   * to render nothing at all, rather than a badge reading zero.
   */
  resetCredits: ResetCredits | null
  /**
   * When the account's subscription renews or ends, or null when there is
   * nothing to say. Codex reports its own date; Claude reports none, and gets
   * an estimate instead (`renewalEstimated`). An instant, so the page ticks
   * the distance to it like every other countdown.
   */
  renewalAtEpoch: number | null
  /**
   * True when `renewalAtEpoch` is not the provider's date but the next
   * billing anniversary of when the subscription began, which is all
   * Anthropic reports. It must be shown as an estimate: a billing date that
   * has moved since the subscription started is invisible to the server.
   * Optional because a plugin built before it sends no such field, and every
   * renewal it sent was the provider's own.
   */
  renewalEstimated?: boolean
  /**
   * The credit balance the provider reports for this account, or null when it
   * reports none. Codex accounts carry ChatGPT credits and Grok accounts a
   * prepaid dollar balance. A figure to print, never a bar: there is no
   * allowance for it to be a fraction of.
   */
  credits: Credits | null
}

/**
 * A credential's own credit balance. Not to be confused with `ResetCredits`,
 * which are banked resets, or with `Balance`, which is a prepaid account that
 * belongs to no credential at all.
 */
export interface Credits {
  /**
   * Already formatted by the server — "57,706.15", "$12.40", "Unlimited" — so
   * every client prints the same figure the same way.
   */
  display: string
  unlimited: boolean
  /** The provider's decimal, in `unit`. Empty when unlimited. */
  amount: string
  unit: Open<"credits" | "usd">
}

/**
 * Entitlements that clear this account's windows when one is spent. Not
 * capacity, and never drawn as a bar. On Codex, spending one resets the
 * session and weekly windows outright and moves the weekly reset date; on
 * Claude it clears the 5-hour and weekly limits, and only once the account is
 * at one.
 */
export interface ResetCredits {
  /** At least 1 whenever this object exists. */
  availableCount: number
  /**
   * The soonest credit that can still be spent. Null when the provider did not
   * date it — a Codex banked reset lapses thirty days after it is granted and
   * a Claude grant has an end date of its own, so the absence of a deadline is
   * worth rendering differently from a distant one.
   */
  expiresAtEpoch: number | null
  expiresInSeconds: number | null
  /**
   * Whether this dashboard may spend it. False when redeeming is switched off,
   * when the credential is parked, or when it is not one this plugin can
   * authenticate. The count still shows; only the button goes. Never offer
   * redemption without this.
   */
  redeemable: boolean
  /**
   * The provider's reason, at the last poll, that none can be spent right
   * now. Empty when one can be or the provider does not say. A hint printed
   * beside the button, never a reason to hide it: the plugin checks the
   * provider afresh before spending anything.
   */
  hold: "" | "notLimited" | "cooldown" | "paused" | "ineligible" | (string & {})
  /** When a "cooldown" hold lifts, or null. */
  holdUntilEpoch: number | null
}

/** One bucket of the ring. Empty buckets are present, and are half the shape. */
export interface ActivityBucket {
  success: number
  failed: number
  /** 0 for no traffic up to 3 for the busiest bucket in the provider. */
  intensity: number
}

export interface Activity {
  /** Both served, so nothing here has to assume CPA's current ring shape. */
  bucketSeconds: number
  windowSeconds: number
  /** Oldest first. The last bucket is the one in progress. */
  buckets: ActivityBucket[]
  success: number
  failed: number
  /** Upper bound on the last request; null when the window is empty. */
  lastRequestAtEpoch: number | null
  /** Traffic in the bucket in progress — as close to "now" as this data goes. */
  live: boolean
}

export interface Aggregate {
  remainingFraction: number
  remainingPercent: number
  /** Credentials the mean covers, and the rest. They sum to `entries.length`. */
  memberCount: number
  excludedCount: number
  level: Level
  trend: Trend
  soonestResetAtEpoch: number | null
  soonestResetInSeconds: number | null
  projectedGainPercent: number
  /**
   * The same gain unrounded, on the 0-1 scale of `remainingFraction`. The
   * width of what the next reset returns comes from this; the label beside it
   * prints `projectedGainPercent`. Absent from a plugin older than the pooled
   * bar, which then draws no such mark rather than inventing one.
   */
  projectedGainFraction?: number
  /**
   * When the row would read 100% if nothing more were used — the latest reset
   * among the members below full — or null when that cannot be said: every
   * member already full, or one below full with no reset at all.
   */
  fullAtEpoch?: number | null
  fullInSeconds?: number | null
  /** The recovery as one sentence, for printing whole. Never parsed. */
  subtext: string
}

export interface RowEntry {
  credentialId: string
  /**
   * False when this credential reported nothing for this window. Every numeric
   * field below is then zero and means nothing — print a dash, not 0% — and
   * `level` is "", which falls through to the neutral rendering every unknown
   * enum value gets.
   */
  hasReading: boolean
  remainingFraction: number
  remainingPercent: number
  level: Level
  /**
   * This credential's slice of the row's pool, on the row's 0-1 scale. Laid
   * end to end, the entries' shares are `aggregate.remainingFraction` — the
   * server's slices of the server's mean, so the pooled bar needs no
   * arithmetic here. Zero with no reading. Absent from an older plugin.
   */
  poolShare?: number
  /**
   * What the row regains from this credential at its next recovery, on the
   * same scale; zero unless `resetsNext`. Summed, `projectedGainFraction`.
   */
  recoveryShare?: number
  /**
   * Part of the row's next recovery: its window resets at the row's soonest
   * reset or within the minute after. True for a full credential too, which
   * the recovery names but which returns nothing.
   */
  resetsNext?: boolean
  resetAtEpoch: number | null
  resetInSeconds: number | null
  resetDisplayHint: ResetDisplayHint
  observedAtEpoch: number
  nextAttemptEpoch: number
  sourceWindowKey: string
  sourceModel: string | null
  dataIssues: string[]
  state: EntryState
}

export interface Row {
  rowId: string
  title: string
  order: number
  matched: boolean
  aggregate: Aggregate
  entries: RowEntry[]
}

export interface Provider {
  id: string
  title: string
  order: number
  credentialCount: number
  rows: Row[]
}

/**
 * Money left on a prepaid account that belongs to no CPA credential — the
 * OpenRouter account quota-cache reads with a management key of its own.
 *
 * Never a bar: the provider reports all-time totals, so a fraction would be a
 * share of everything ever bought. The amount is the headline and `level` is
 * already judged against the operator's warn-below threshold.
 */
export interface Balance {
  id: string
  provider: string
  title: string
  order: number
  currency: string
  /**
   * False until a poll has succeeded. The amounts are then zero and mean
   * nothing, and `level` is "": print a dash, never $0.00.
   */
  hasReading: boolean
  /** Negative on an overdrawn account. */
  remaining: number
  /** `remaining` as the dashboard prints it: "$74.75", "-$1.20". */
  remainingText: string
  purchased: number
  used: number
  warnBelow: number
  level: Level
  subtext: string
  observedAtEpoch: number
  nextAttemptEpoch: number
  dataIssues: string[]
  state: EntryState
}

export interface Summary {
  schemaVersion: number
  generatedAtEpoch: number
  observedAtEpoch: number | null
  nextAttemptEpoch: number | null
  stale: boolean
  staleReason: StaleReason | null
  counters: Counters
  credentials: Credential[]
  providers: Provider[]
  /** Always an array from this plugin; optional for a proxied older one. */
  balances?: Balance[]
}
