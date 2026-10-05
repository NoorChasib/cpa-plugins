import { useNowSeconds } from "../lib/now"
import { formatDuration, secondsUntil } from "../lib/time"
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
function observedPhrase(summary: Summary, now: number): React.ReactNode {
  if (summary.observedAtEpoch === null) return "never observed"
  return (
    <>
      observed <span className="num">{formatDuration(now - summary.observedAtEpoch)}</span> ago
    </>
  )
}

function attemptPhrase(summary: Summary, now: number): React.ReactNode {
  if (summary.nextAttemptEpoch === null) return null
  const remaining = secondsUntil(summary.nextAttemptEpoch, now)
  // The server does not filter this to the future, so a poller that has fallen
  // behind reads as overdue here rather than quietly disappearing. At 400px —
  // the menu bar's popover — "attempt" goes, so the line stays beside the name
  // instead of wrapping under it.
  const word = <span className="max-[640px]:hidden">attempt </span>
  return remaining <= 0 ? (
    <>next {word}due</>
  ) : (
    <>
      next {word}in <span className="num">{formatDuration(remaining)}</span>
    </>
  )
}

/** The gauge mark beside the name: the same inline SVG the menu bar uses. */
function Mark() {
  return (
    <svg viewBox="0 0 16 16" fill="none" aria-hidden="true" className="qg-mark">
      <path d="M2.3 11.6a5.7 5.7 0 1 1 11.4 0" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
      <path d="M8 11.6 10.9 6.9" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
      <circle cx="8" cy="11.6" r="1.4" fill="currentColor" />
    </svg>
  )
}

export function Header({
  summary,
  offline,
  actions,
}: {
  summary: Summary | undefined
  offline: boolean
  /**
   * The header's control slot, at its far right. Empty today; it is where a
   * settings or access control belongs when there is one, so the line it sits
   * on is already laid out to take it.
   */
  actions?: React.ReactNode
}) {
  const now = useNowSeconds()
  const attempt = summary ? attemptPhrase(summary, now) : null
  // The dot repeats what the banners say in words — fresh, stale, or out of
  // touch — so it is a glance, never the only place that fact appears.
  const tone = offline ? "is-off" : summary?.stale ? "is-stale" : "is-live"

  return (
    <header className="qg-top">
      <div className="qg-brand">
        <Mark />
        <h1>Quota Glance</h1>
      </div>
      <div className="qg-top-end">
        {summary && (
          // A sentence in the page's text face, its durations in the figures'
          // monospace, the way every other sentence here is set.
          <div className="qg-meta">
            <span className={`qg-live ${tone}`} aria-hidden="true" />
            {observedPhrase(summary, now)}
            {attempt && <> · {attempt}</>}
          </div>
        )}
        {actions && <div className="qg-actions">{actions}</div>}
      </div>
    </header>
  )
}
