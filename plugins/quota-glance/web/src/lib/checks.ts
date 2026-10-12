// The accounts the page is waiting to see fresh figures for.
//
// After a press that spent, or may have spent, a banked reset, the plugin asks
// Quota Cache to poll that account now, then every other one, and says so in
// its answer (refreshRequested), with the instant it asked
// (refreshRequestedAtEpoch). The page then marks the account's rows "Checking
// the new limits…" and reads the summary every five seconds instead of every
// minute, so the new figures appear as soon as Quota Cache has them. It stops
// at the first summary carrying an observation of the account from that
// instant on, or after three minutes, whichever comes first: a poll that fails
// brings no new observation, and must not hold the page at five seconds.
//
// Two clocks, kept apart. The instant asked and the observations are both by
// CPA's clock, so they are compared with each other; this browser's clock can
// be minutes off CPA's, and only ever measures the three minutes, from when
// the page started the check. The instant is CPA's own, not the press's: it
// comes after the provider answered, and a copy of a press whose answer was
// lost is answered with the first copy's, so it names the request Quota Cache
// may already have acted on.
//
// This is the page's own state. The summary gains no field for it, so the
// menu bar's readout, which reads only the summary, never sees it.
//
// It imports nothing, so node's tests drive it directly; checking.ts hands it
// to React.

/** How often the summary is read while an account is being checked. */
export const CHECK_POLL_MS = 5_000

/** The longest an account is checked, in seconds from when the check started. */
export const CHECK_SECONDS = 180

/** What the account's rows say meanwhile. */
export const CHECKING_LABEL = "Checking the new limits…"

/** An account being checked, after the press that asked. */
export interface Check {
  readonly id: string
  /**
   * When Quota Cache was asked to poll the account, in Unix seconds by CPA's
   * clock, from the plugin's answer.
   */
  readonly since: number
  /** When the page started the check, in Unix seconds by this browser's clock. */
  readonly startedAt: number
}

/** The part of a summary credential a check reads. */
export interface Observed {
  readonly id: string
  readonly lastObservedEpoch: number
}

let checks: readonly Check[] = []
const listeners = new Set<() => void>()

/**
 * Starts checking `id` at `now`, by this browser's clock, for an observation
 * from `since`, by CPA's, on. A check already running on the account is
 * replaced, and checks past their three minutes are dropped on the way, so the
 * list holds only what could still be waiting.
 */
export function startCheck(id: string, since: number, now: number): void {
  checks = [...checks.filter((check) => check.id !== id && now - check.startedAt < CHECK_SECONDS), { id, since, startedAt: now }]
  for (const listener of listeners) listener()
}

/** For useSyncExternalStore: calls `listener` each time a check starts. */
export function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/** Every check started, for useSyncExternalStore; a new array each time one starts. */
export function getChecks(): readonly Check[] {
  return checks
}

/**
 * Whether `check` is still waiting at `now`, by this browser's clock: inside
 * its three minutes, and the account has no observation from the second
 * Quota Cache was asked in or later. That second counts because the poll
 * asked for can begin in it. The price is that a scheduled poll begun in the
 * same second, just before the provider answered, counts too. An account the
 * summary no longer lists, or has not loaded yet, has none.
 */
function waiting(check: Check, credential: Observed | undefined, now: number): boolean {
  return now - check.startedAt < CHECK_SECONDS && !(credential !== undefined && credential.lastObservedEpoch >= check.since)
}

/** Whether `credential`'s rows say "Checking the new limits…" at `now`. */
export function isChecking(list: readonly Check[], credential: Observed | undefined, now: number): boolean {
  if (credential === undefined) return false
  const check = list.find((one) => one.id === credential.id)
  return check !== undefined && waiting(check, credential, now)
}

/** Whether any check is still waiting at `now`, against the summary's credentials. */
export function anyChecking(list: readonly Check[], credentials: readonly Observed[] | undefined, now: number): boolean {
  return list.some((check) => waiting(check, credentials?.find((one) => one.id === check.id), now))
}

/**
 * How often to read the summary: `usual`, the page's own interval or false for
 * none, and at least every CHECK_POLL_MS while a check is waiting. A page with
 * no way in still reads nothing, check or no check.
 */
export function checkedInterval(usual: number | false, checking: boolean): number | false {
  return usual === false || !checking ? usual : Math.min(usual, CHECK_POLL_MS)
}
