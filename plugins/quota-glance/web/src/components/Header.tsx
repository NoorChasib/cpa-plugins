import { useNowSeconds } from "../lib/now"
import { formatAgo, formatDuration, secondsUntil } from "../lib/time"
import type { Summary } from "../lib/types"

/**
 * "observed 4m ago · next attempt in 11m".
 *
 * There is no refresh button anywhere in this design, and this line is the
 * reason it is not missed: nothing the reader can press would make the data
 * fresher, because quota-cache owns the polling schedule. What they actually
 * want to know is how old this is and when it changes, so the header says both.
 *
 * Both instants arrive precomputed — the newest observation and the soonest
 * scheduled poll across every credential. All that happens here is subtraction
 * against the current second.
 */
function observedPhrase(summary: Summary, now: number): string {
  return summary.observedAtEpoch === null ? "never observed" : `observed ${formatAgo(summary.observedAtEpoch, now)}`
}

function attemptPhrase(summary: Summary, now: number): string | null {
  if (summary.nextAttemptEpoch === null) return null
  const remaining = secondsUntil(summary.nextAttemptEpoch, now)
  // The server does not filter this to the future, so a poller that has fallen
  // behind reads as overdue here rather than quietly disappearing.
  return remaining <= 0 ? "next attempt due" : `next attempt in ${formatDuration(remaining)}`
}

export function Header({ summary }: { summary: Summary | undefined }) {
  const now = useNowSeconds()
  const phrases = summary ? [observedPhrase(summary, now), attemptPhrase(summary, now)].filter(Boolean) : []

  return (
    <header className="mb-[26px] flex flex-wrap items-baseline justify-between gap-[14px]">
      <h1 className="text-[19px] font-[650] tracking-[-0.015em]">Capacity</h1>
      {phrases.length > 0 && <div className="num text-[11.5px] text-ink-3">{phrases.join(" · ")}</div>}
    </header>
  )
}
