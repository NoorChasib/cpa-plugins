// The only arithmetic this app performs on the document.
//
// Every percentage, level, ordering and phrase arrives precomputed; the one
// thing a server cannot precompute is how long is left *now*, because "now"
// keeps moving after the response is cached. So: `epoch - now`, and the
// formatting needed to print the result.
//
// formatDuration deliberately reproduces humanDuration in
// internal/aggregate/build.go. The server writes "resets in 1h 15m" into
// aggregate.subtext and this app ticks the matching countdown beside it; if the
// two rendered the same span differently, one card would contradict itself.

/** Whole seconds from now until an instant. Negative once it has passed. */
export function secondsUntil(epoch: number, nowSeconds: number): number {
  return epoch - nowSeconds
}

/**
 * A span as the design prints it: "1d 2h", "1h 15m", "45m", "<1m".
 *
 * Mirrors humanDuration in internal/aggregate/build.go, including its floor at
 * zero and its "<1m" for anything under a minute.
 */
export function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds))
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  if (days > 0 && hours > 0) return `${days}d ${hours}h`
  if (days > 0) return `${days}d`
  if (hours > 0 && minutes > 0) return `${hours}h ${minutes}m`
  if (hours > 0) return `${hours}h`
  if (minutes > 0) return `${minutes}m`
  return "<1m"
}

/** How long ago an instant was, in the same vocabulary: "4m ago". */
export function formatAgo(epoch: number, nowSeconds: number): string {
  return `${formatDuration(nowSeconds - epoch)} ago`
}

/**
 * The countdown in a credential's reset column.
 *
 * `none` means the server has already decided not to run a clock here, and the
 * two cases behind it read differently: no reset instant at all is a dash,
 * while an instant that has passed is a window mid-turnover. A `countdown` that
 * runs out while the page is open lands in the same place rather than counting
 * on into negative numbers.
 */
export function formatReset(
  hint: string,
  resetAtEpoch: number | null,
  nowSeconds: number,
): { text: string; resetting: boolean } {
  if (resetAtEpoch === null) return { text: "—", resetting: false }
  const remaining = secondsUntil(resetAtEpoch, nowSeconds)
  if (hint !== "countdown" || remaining <= 0) return { text: "resetting…", resetting: true }
  return { text: formatDuration(remaining), resetting: false }
}
