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
export type EntryState = Open<"ok" | "error" | "stale">
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
}

export interface Aggregate {
  remainingFraction: number
  remainingPercent: number
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
