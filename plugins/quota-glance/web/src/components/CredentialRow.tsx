import { ROUTING_LABELS } from "../lib/accounts"
import { useNowSeconds } from "../lib/now"
import { weeklyNote } from "../lib/pool"
import { formatReset } from "../lib/time"
import type { Credential, RowEntry } from "../lib/types"

/**
 * The account's own bar, in the colour of its own level: the accent while it
 * has room, amber when it runs low, red when it is critical — the same three
 * the pool bar uses, so a red row is what makes a pool turn.
 *
 * Sized from the fraction and labelled from the integer percent, both of which
 * the server sends, so the bar and the number beside it cannot disagree.
 *
 * `spent` keeps the width and takes the colour: the reading is real, but an
 * account whose weekly is spent can use none of it, and a full bar in the
 * accent would read as room. `cap`, on Fable, is what the pool counts the
 * account as: the bar is drawn to its own figure in the dimmest ink and to the
 * cap in its level's colour, so "weekly caps at 24%" can be read off the bar.
 */
function Bar({
  fraction,
  percent,
  level,
  spent,
  cap,
}: {
  fraction: number
  percent: number
  level: string
  spent: boolean
  cap: { fraction: number; percent: number } | null
}) {
  const width = (value: number) => `${Math.max(0, Math.min(1, value)) * 100}%`
  const label = cap ? `${percent}% left; the weekly limit caps it at ${cap.percent}%` : `${percent}% left`
  return (
    <span className={`qg-abar qg-lvl-${level} ${spent ? "is-spent" : ""}`} role="img" aria-label={label}>
      {cap ? (
        <>
          <i className="is-capped" style={{ width: width(fraction) }} />
          <i style={{ width: width(cap.fraction) }} />
        </>
      ) : (
        <i style={{ width: width(fraction) }} />
      )}
    </span>
  )
}

/** Why a row has no bar, in the words the contract uses. */
const STATE_LABELS: Record<string, string> = {
  error: "failed",
  stale: "stale",
  // The three that carry no reading at all. They say what is missing rather
  // than what is wrong, because in none of them is anything wrong: a window
  // this plan does not have, a credential CPA has only just learned about, a
  // provider quota-cache does not poll.
  noData: "not reported",
  pending: "not polled yet",
  unsupported: "no poller",
}

/** The row's figure ink, from the level the server computed for this entry. */
const LEVEL_INK: Record<string, string> = {
  low: "text-warn",
  critical: "text-crit",
}

/**
 * One account inside a window card, on one line: who, its own bar, what is
 * left, and when it resets — two lines in the menu bar's popover, the bar
 * under the figures.
 *
 * Only what is about this window. The plan, the renewal, the credit and the
 * traffic are about the account, and are said once in the provider's
 * Accounts card rather than on every card the account appears in. The one
 * exception is CPA parking the account: a full bar on an account nothing is
 * routed to reads as room the pool does not have, so the row says so.
 */
export function CredentialRow({
  entry,
  credential,
  name,
}: {
  entry: RowEntry
  credential: Credential | undefined
  /** What the provider's section calls this account; see accountNames. */
  name: string
}) {
  const now = useNowSeconds()
  const reset = formatReset(entry.resetDisplayHint, entry.resetAtEpoch, now)
  // An unknown state is still a state: print it in the bar's place rather
  // than drawing a confident bar over a reading the server would not vouch for.
  const degraded = entry.state !== "ok"
  // Why the pool counts this account as less than its own figure, if it does.
  const weekly = weeklyNote(entry)
  const parked = credential ? ROUTING_LABELS[credential.status] : undefined
  const cap =
    weekly && !weekly.spent && typeof entry.pooledFraction === "number" && typeof entry.pooledPercent === "number"
      ? { fraction: entry.pooledFraction, percent: entry.pooledPercent }
      : null

  return (
    <div className={`qg-arow ${degraded ? "is-degraded" : ""}`}>
      <span className="qg-awho">
        <span className="qg-aname" title={credential?.email || entry.credentialId}>
          {name}
        </span>
        {weekly && <span className="qg-chip qg-chip-weekly">{weekly.text}</span>}
        {parked && <span className="qg-chip qg-chip-route">{parked}</span>}
      </span>

      {degraded ? (
        // The figures stay: they were true when they were taken. What goes
        // is the bar, because its whole job is to be read at a glance and
        // this reading has not earned that.
        <span className="qg-astate">{STATE_LABELS[entry.state] ?? entry.state}</span>
      ) : (
        <Bar
          fraction={entry.remainingFraction}
          percent={entry.remainingPercent}
          level={entry.level}
          spent={weekly?.spent === true}
          cap={cap}
        />
      )}

      {/* Never 0% for an absent reading: an absent reading and an exhausted
        * credential are the same zero in the document and opposite facts on
        * a capacity dashboard. */}
      <span className={`qg-apct num ${LEVEL_INK[entry.level] ?? ""}`}>
        {entry.hasReading ? `${entry.remainingPercent}%` : "—"}
      </span>

      {/* Nothing at all when there is no reset instant: "resets —" told the
        * reader only that this space had been reserved. */}
      <span className="qg-arst">
        {entry.resetAtEpoch !== null &&
          (reset.resetting ? (
            <b className="num">{reset.text}</b>
          ) : (
            <>
              resets <b className="num">{reset.text}</b>
            </>
          ))}
      </span>
    </div>
  )
}
