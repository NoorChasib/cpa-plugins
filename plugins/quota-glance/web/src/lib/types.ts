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
   * Codex account that has not been granted one. Null is the signal to render
   * nothing at all, rather than a badge reading zero.
   */
  resetCredits: ResetCredits | null
}

/**
 * Entitlements that clear this account's windows when one is spent. Not
 * capacity, and never drawn as a bar: spending one resets the session and
 * weekly Codex windows outright and moves the weekly reset date.
 */
export interface ResetCredits {
  /** At least 1 whenever this object exists. */
  availableCount: number
  /**
   * The soonest credit that can still be spent. Null when the provider did not
   * date it — a banked reset lapses thirty days after it is granted, so the
   * absence of a deadline is worth rendering differently from a distant one.
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
}
