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
   * Where `renewalAtEpoch` comes from: the provider's own date, one set on
   * the dashboard, or the estimate; null with no renewal. Optional because an
   * older plugin sends none.
   */
  renewalSource?: "reported" | "dashboard" | "estimated" | (string & {}) | null
  /**
   * A Claude credential's estimate, the next billing anniversary of when the
   * subscription began, whatever `renewalSource` says: what Use estimate
   * returns to beside a date set on the dashboard. Null for any other
   * provider and when no start is known; absent from an older plugin.
   */
  renewalEstimateAtEpoch?: number | null
  /** The page may set this credential's renewal date: a Claude credential, while editing is available. */
  renewalEditable?: boolean
  /** The renewal date stored on the dashboard, used or not; null when none. */
  renewalSetting?: RenewalSetting | null
  /**
   * The credit balance the provider reports for this account, or null when it
   * reports none. Codex accounts carry ChatGPT credits and Grok accounts a
   * prepaid dollar balance. A figure to print, never a bar: there is no
   * allowance for it to be a fraction of.
   */
  credits: Credits | null
}

/** A renewal date set on the dashboard. `revision` is what a save sends back as `baseRevision`. */
export interface RenewalSetting {
  /** YYYY-MM-DD. */
  date: string
  revision: string
  updatedAtEpoch: number
}

/** A renewal date stored for a credential CPA no longer lists. */
export interface RenewalOrphan extends RenewalSetting {
  id: string
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
   * among the members below full, and on Claude's Fable row a member's weekly
   * reset as well when its weekly is below full — or null when that cannot be
   * said: every member already full, or a reset one below full waits for has
   * no instant at all.
   */
  fullAtEpoch?: number | null
  fullInSeconds?: number | null
  /**
   * The part of `excludedCount` that does have a reading but was held out of
   * the mean — on Claude's session row, the accounts whose weekly limit is
   * spent — so the rest of `excludedCount` is the credentials with no reading.
   * Zero on every other row. Absent from an older plugin, whose every
   * exclusion was a missing reading.
   */
  heldOutCount?: number
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
   * server's slices of the server's mean — for a client that draws the pool
   * account by account. This dashboard sizes its bar from the aggregate
   * itself. Zero with no reading or when held out. Absent from an older
   * plugin.
   */
  poolShare?: number
  /**
   * What the row regains from this credential at its next recovery, on the
   * same scale; zero unless `resetsNext`. Summed, `projectedGainFraction`.
   */
  recoveryShare?: number
  /**
   * Part of the row's next recovery: its window resets at the row's soonest
   * reset or within the minute after — on Claude's Fable row, its Fable window
   * or its weekly does. True too for a credential whose reset returns nothing,
   * one already full or a Fable reset under a weekly that still caps it, which
   * the recovery names anyway. Always false when held out.
   */
  resetsNext?: boolean
  /**
   * Left out of the row's mean although it has a reading. On Claude's session
   * row, an account whose weekly limit is spent: its session can sit at 100%
   * with nothing able to use it, and counting it would make the pool look
   * fuller than it is. The figures above are still its real reading; its
   * shares are zero and `resetsNext` is false. Absent from an older plugin,
   * which held nothing out.
   */
  heldOut?: boolean
  /**
   * What this credential counts as in the row's mean, as a fraction and as
   * the percent printed for it. The same as `remainingFraction` and
   * `remainingPercent` on every row but Claude's Fable, where an account
   * cannot use more Fable than its weekly has left and so counts as the
   * lesser of the two. A held-out credential's is its own reading, though the
   * mean leaves it out. Zero with no reading. Absent from an older plugin.
   */
  pooledFraction?: number
  pooledPercent?: number
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
  /**
   * The pooled monthly Claude API credit. `null` when Quota Cache has no
   * `claude-api-credits` configured; absent from a proxied older plugin.
   */
  apiCredits?: APICredits | null
  /** Renewal dates stored for credentials no longer listed, oldest first. Absent from an older plugin. */
  renewalOrphans?: RenewalOrphan[]
}

export type APICreditState = Open<
  "ok" | "stale" | "out" | "pending" | "needsSettings" | "misconfigured" | "cacheTooOld"
>

/** Where an account's monthly credit or refill date comes from. */
export type APICreditSource = Open<"dashboard" | "config" | "none">

/** The price table every amount was computed at. */
export interface APICreditPricing {
  /** The day the table was read, "2026-10-09". */
  asOf: string
  source: string
  /** "5m": every cache write is priced at the 5-minute rate. */
  cacheWrites: string
}

/** A stretch the meter was not counting, under 5 minutes never listed. */
export interface APICreditGap {
  fromEpoch: number
  toEpoch: number
  reason: string
}

/** Quota Cache's usage meter, as the last read found it. */
export interface APICreditMeter {
  sinceEpoch: number
  /** When the meter last saved its figures. */
  updatedAtEpoch: number
  stale: boolean
  stoppedAtEpoch: number | null
  /** "" while counting, else why it stopped. */
  stopReason: string
  dropped: number
  lastDroppedEpoch: number | null
  unattributed: number
  lastUnattributedEpoch: number | null
  rejected: number
  lastRejectedEpoch: number | null
  foreign: number
  /** Oldest first. Only gaps of 5 minutes or more. */
  gaps: APICreditGap[]
}

/** Whether the page may offer the editor, and why not. */
export interface APICreditEditing {
  available: boolean
  reason: Open<"" | "disabled" | "settingsUnreadable">
}

/** The soonest refill that restores anything to the pool. */
export interface APICreditRefill {
  /** Every counted account refilling at this instant, in account order. */
  accountIds: string[]
  refillAtEpoch: number
  refillInSeconds: number
  gain: number
  gainText: string
  /** `gain` on the pool's 0-1 scale; `gainPercent` is the same, printed. */
  gainFraction: number
  gainPercent: number
}

/**
 * The counted accounts summed: those in state ok, stale or out. `left` is the
 * sum of each account's own left, so one account's overage never eats
 * another's credit.
 */
export interface APICreditPool {
  /** False when nothing is counted: amounts are 0 with "" text and `level` is "". */
  hasEstimate: boolean
  /** Some counted account may have spent more than it shows: `used` is at least, `left` at most. */
  lowerBound: boolean
  monthlyCredit: number
  monthlyCreditText: string
  used: number
  usedText: string
  left: number
  leftText: string
  overage: number
  overageText: string
  remainingFraction: number
  remainingPercent: number
  level: Level
  nextRefill: APICreditRefill | null
  fullAtEpoch: number | null
  fullInSeconds: number | null
  /** `accountCount == countedCount + missingCount`. */
  accountCount: number
  countedCount: number
  missingCount: number
}

/** The Console reading an account's estimate starts from. */
export interface APICreditReading {
  remaining: number
  remainingText: string
  /** When Console showed it. */
  atEpoch: number
  enteredAtEpoch: number
  /** Metered since the reading. */
  spentSince: number
  spentSinceText: string
}

export interface APICreditRefusals {
  total: number
  lastAtEpoch: number | null
  claudeCodeTotal: number
  claudeCodeLastAtEpoch: number | null
}

/** A model with no listed price, and its tokens in the priced window. */
export interface APICreditUnpriced {
  model: string
  tokens: number
}

/** A Console reading as it was saved, used or not. */
export interface StoredReading {
  /** As saved, "143.20". */
  remainingUsd: string
  atEpoch: number
  enteredAtEpoch: number
}

/**
 * What the editor works from: the values set on the dashboard, the config's
 * beside them, and the revision a save sends back as its `baseRevision`.
 */
export interface APICreditSettings {
  editable: boolean
  notEditableReason: Open<
    "" | "noOrganization" | "duplicateOrganization" | "overLimit" | "cacheTooOld" | "disabled" | "settingsUnreadable"
  >
  /** "" with nothing stored. */
  revision: string
  /** The dashboard value, "" when none. */
  monthlyUsd: string
  configMonthlyUsd: string
  configMonthlyUsdInvalid: boolean
  /** The dashboard date, YYYY-MM-DD, "" when none. */
  renews: string
  configRenews: string
  configRenewsInvalid: boolean
  reading: StoredReading | null
  readingUnusedReason: Open<"" | "beforeRefill" | "otherOrganization" | "tooOld" | "future">
  updatedAtEpoch: number | null
}

/** One Claude Console organization's credit this cycle, estimated from CPA traffic. */
export interface APICreditAccount {
  /** "org-<12 hex>" for a linked organization, "item-<n>" otherwise. */
  id: string
  /** "" when the item has no usable label. */
  label: string
  order: number
  /** "" when missing or invalid. */
  organizationId: string
  state: APICreditState
  /** In the pool's sums: ok, stale or out. */
  counted: boolean
  /** `left` and `used` mean something. */
  hasEstimate: boolean
  basis: Open<"reading" | "credit" | "">
  /** `used` is at least what is printed, `left` at most. */
  lowerBound: boolean
  monthlyCredit: number
  /** "" when the credit is not known. */
  monthlyCreditText: string
  monthlyCreditSource: APICreditSource
  spent: number
  spentText: string
  /** What `spent` counts from, or null when it counts from nothing. */
  spentSinceEpoch: number | null
  used: number
  usedText: string
  left: number
  leftText: string
  /** What the estimate had left before a refusal made the account out; "" otherwise. */
  estimateLeftText: string
  overage: number
  overageText: string
  remainingFraction: number
  remainingPercent: number
  level: Level
  cycleStartEpoch: number | null
  renewsAtEpoch: number | null
  renewsInSeconds: number | null
  renewsSource: APICreditSource
  /** The reading in use, else null. */
  reading: APICreditReading | null
  cacheWriteExtra: number
  /** Money text, "" below $0.01; the page writes the sentence. */
  cacheWriteExtraText: string
  unpriced: APICreditUnpriced[]
  refusals: APICreditRefusals
  meterSinceEpoch: number | null
  lastSeenEpoch: number | null
  dataIssues: string[]
  /** One sentence, already written, for the first issue; "" when there is nothing to say. */
  issue: string
  settings: APICreditSettings
}

/** A Console organization CPA sent traffic from that no counted item names. */
export interface APICreditUnlinked {
  organizationId: string
  firstSeenEpoch: number
  lastSeenEpoch: number
  requests: number
  reason: Open<"notConfigured" | "overLimit">
}

/** API credit values stored for an account Quota Cache no longer lists. */
export interface APICreditOrphan {
  id: string
  /** "" when none. */
  monthlyUsd: string
  /** "" when none. */
  renews: string
  hasReading: boolean
  revision: string
  updatedAtEpoch: number
}

/** The monthly Claude API credit across every configured Console organization. */
export interface APICredits {
  title: string
  currency: string
  pricing: APICreditPricing
  /** Null without a readable meter file. */
  meter: APICreditMeter | null
  editing: APICreditEditing
  pool: APICreditPool
  /** Pre-sorted by configured order; do not re-sort. */
  accounts: APICreditAccount[]
  unlinked: APICreditUnlinked[]
  orphans: APICreditOrphan[]
}
