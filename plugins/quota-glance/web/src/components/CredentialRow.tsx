import { useNowSeconds } from "../lib/now"
import { formatAgo, formatDate, formatDuration, formatReset } from "../lib/time"
import type { Activity, Credential, Credits, RowEntry } from "../lib/types"

/**
 * The address, with the domain in muted ink so the distinguishing word reads
 * first. Never initials: at 390px the full address still fits, and a column of
 * initials is unreadable when four of them begin with the same letter.
 *
 * min-w-0 because this sits in a flex row beside the plan and routing chips,
 * and a flex item defaults to min-width:auto — which refuses to shrink below
 * its content and would push the chips off the card at 390px rather than
 * truncate the address.
 */
function Email({ address }: { address: string }) {
  const at = address.indexOf("@")
  const [local, domain] = at > 0 ? [address.slice(0, at), address.slice(at)] : [address, ""]
  return (
    <span className="qg-email" title={address}>
      {local}
      {domain && <span className="text-ink-3">{domain}</span>}
    </span>
  )
}

/**
 * The account's own bar, in its identity shade, so the row and its slice of
 * the pool above read as the same thing.
 *
 * Sized from the fraction and labelled from the integer percent, both of which
 * the server sends, so the bar and the number beside it cannot disagree. The
 * level is carried by the number's ink and, for assistive technology, by the
 * label — not by the bar, whose colour already means "which account".
 */
function Bar({ fraction, percent, slot }: { fraction: number; percent: number; slot: number }) {
  const width = Math.max(0, Math.min(1, fraction)) * 100
  return (
    <span className="qg-abar" role="img" aria-label={`${percent}% left`}>
      <i className={`qg-id-${slot}`} style={{ width: `${width}%` }} />
    </span>
  )
}

/**
 * The traffic strip: one block per bucket, oldest at the left, the bucket in
 * progress at the right.
 *
 * A block is inked from the server's intensity, never from its own count, so
 * the credential carrying the pool towers over the one taking a trickle instead
 * of every row looking equally busy. Any failure in a bucket colours it red
 * whatever its volume — a single failure inside a busy ten minutes is the thing
 * most worth not losing.
 *
 * Empty buckets are drawn, not skipped. The gaps are half of what the strip
 * says, and a row with its quiet buckets removed would read as continuous
 * traffic.
 */
function TrafficStrip({ activity, span }: { activity: Activity; span: string }) {
  return (
    <span className="act" role="img" aria-label={`request activity over the last ${span}`}>
      {activity.buckets.map((bucket, index) => (
        <i
          key={index}
          className={bucket.failed > 0 ? "act-f" : `act-${Math.min(bucket.intensity, 3)}`}
        />
      ))}
    </span>
  )
}

/**
 * The count beside the strip.
 *
 * Successes and failures are named separately rather than summed: "3 req" over
 * a strip that is two thirds red describes the same numbers and none of the
 * situation.
 */
function TrafficCount({ activity }: { activity: Activity }) {
  const parts: string[] = []
  if (activity.success > 0) parts.push(`${activity.success} req`)
  return (
    <>
      {parts.join("")}
      {activity.failed > 0 && (
        <>
          {parts.length > 0 && " · "}
          <span className="text-crit">{activity.failed} failed</span>
        </>
      )}
    </>
  )
}

/**
 * The strip, its count, and when the last request landed.
 *
 * "now" comes from the server's own `live` flag rather than from comparing
 * `lastRequestAtEpoch` to the clock: only the server knows which bucket is the
 * one in progress, and a request 30 seconds old and one 9 minutes old are both
 * in it. Everything older is an age this page ticks itself, in the same
 * vocabulary as every other countdown on it.
 */
function Traffic({ activity }: { activity: Activity }) {
  const now = useNowSeconds()
  const span = formatDuration(activity.windowSeconds)
  const idle = activity.success === 0 && activity.failed === 0

  return (
    <span className="qg-traffic">
      <TrafficStrip activity={activity} span={span} />
      <span
        className={`num min-w-0 truncate text-[10px] ${
          idle ? "text-ink-4" : activity.live ? "text-accent" : "text-ink-3"
        }`}
        title={idle ? `no requests in the last ${span}` : undefined}
      >
        {idle ? (
          "idle"
        ) : (
          <>
            {/* The count goes first, and goes first at 390px, where the line
              * is half as wide: the strip beside it already shows how much,
              * roughly, and only this can say when. */}
            <span className="max-[640px]:hidden">
              <TrafficCount activity={activity} />
              {" · "}
            </span>
            {activity.live
              ? "now"
              : activity.lastRequestAtEpoch !== null
                ? formatAgo(activity.lastRequestAtEpoch, now)
                : span}
          </>
        )}
      </span>
    </span>
  )
}

/**
 * A credit balance as the account line prints it. The figure is the server's,
 * already formatted; this only names what it is a figure of. "Unlimited" is a
 * word, not a figure, so it is not set in the figures' monospace.
 */
function creditsText(credits: Credits): { figure: string; unit: string; numeric: boolean } {
  switch (credits.unit) {
    case "credits":
      if (credits.unlimited) return { figure: "unlimited", unit: "credits", numeric: false }
      return { figure: credits.display, unit: credits.display === "1" ? "credit" : "credits", numeric: true }
    case "usd":
      return credits.unlimited
        ? { figure: "unlimited", unit: "credit", numeric: false }
        : { figure: credits.display, unit: "prepaid", numeric: true }
    default:
      // A unit this bundle does not know yet. The server's figure and its unit
      // are still the truth, so they are printed as sent.
      return { figure: credits.display, unit: credits.unit, numeric: !credits.unlimited }
  }
}

function CreditsFigure({ credits }: { credits: { figure: string; unit: string; numeric: boolean } }) {
  return (
    <span className="shrink-0">
      <b className={credits.numeric ? "num" : ""}>{credits.figure}</b> {credits.unit}
    </span>
  )
}

/**
 * When the subscription renews: the date, so it can be checked against a
 * receipt, and the distance, ticking like every other countdown here.
 */
function renewalText(renewalAtEpoch: number, now: number): string {
  const remaining = renewalAtEpoch - now
  return remaining > 0
    ? `renews ${formatDate(renewalAtEpoch)} · in ${formatDuration(remaining)}`
    : "renewal date passed"
}

/** Why a row is dimmed, in the words the contract uses. */
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

/**
 * CPA will not route to this credential right now.
 *
 * It is deliberately not a `state`: the reading beside it is perfectly good and
 * is still drawn as a bar. This says the credential is parked, which is the one
 * thing a full-looking row would otherwise fail to mention.
 */
const ROUTING_LABELS: Record<string, string> = {
  unavailable: "cooldown",
  disabled: "off",
}

/** The row's figure ink, from the level the server computed for this entry. */
const LEVEL_INK: Record<string, string> = {
  low: "text-warn",
  critical: "text-crit",
}

/**
 * One account inside a window card: the pool's surround, in miniature.
 *
 * Who it is and its plan on the first line, its own bar under that, then the
 * two figures a reader came for — what is left, bold under-left, and when it
 * resets, bold under-right — with the traffic strip between them. What the
 * provider says about the account rather than the window (renewal, credit)
 * runs muted beneath, the way the reference puts the secondary figures under
 * the primary ones.
 */
export function CredentialRow({
  entry,
  credential,
  slot,
  solo,
}: {
  entry: RowEntry
  credential: Credential | undefined
  slot: number
  /** The only account on the card, whose bar and figure the pool already shows. */
  solo: boolean
}) {
  const now = useNowSeconds()
  const reset = formatReset(entry.resetDisplayHint, entry.resetAtEpoch, now)
  // An unknown state is still a state: print it in the bar's place rather
  // than drawing a confident bar over a reading the server would not vouch for.
  const degraded = entry.state !== "ok"
  const parked = credential ? ROUTING_LABELS[credential.status] : undefined
  // The pool bar is this account's bar when it is the only one, unless the
  // reading needs saying again with its caveat beside it.
  const compact = solo && !degraded

  // Checked by type rather than against null: a document from a plugin that
  // predates these fields has neither, and must render as it always did.
  const renewal =
    credential && typeof credential.renewalAtEpoch === "number" ? renewalText(credential.renewalAtEpoch, now) : null
  const credits = credential?.credits ? creditsText(credential.credits) : null

  // What the figure line carries, piece by piece, so a line with nothing to
  // say is left out rather than drawn as a dash at each end.
  //
  // The percentage: not on a solo card, whose pool figure is this account's,
  // and not on a degraded row with no reading, where the state word above
  // already says there is none. Anywhere else an absent reading is a dash,
  // never 0% — an absent reading and an exhausted credential are the same
  // zero in the document and opposite facts on a capacity dashboard.
  const percent = !compact && (entry.hasReading || !degraded)
  // The reset: nothing at all when there is no reset instant. "resets —"
  // told the reader only that this space had been reserved.
  const resets = entry.resetAtEpoch !== null
  // A solo card with no reset to count down to has the line's right end free,
  // and the credit balance takes it — beside the traffic strip, the way the
  // account reads on one line — rather than a line of its own under an
  // empty one.
  const creditsInline = compact && !resets && credits !== null
  const figureLine = percent || resets || credential?.activity || creditsInline

  // The swatch says what the row's slice in the bar looks like: stippled for a
  // reading the server will not vouch for, hollow for none.
  const swatch = !entry.hasReading ? "is-hollow" : degraded ? "is-faded" : ""

  return (
    <div className={`qg-acct ${degraded ? "is-degraded" : ""}`}>
      <div className="qg-aline">
        <span className="qg-addr">
          <i className={`qg-sw qg-id-${slot} ${swatch}`} aria-hidden="true" />
          <Email address={credential?.email || entry.credentialId} />
        </span>
        {/* Full names, never truncated: "SuperGrok Heavy" and "Enterprise"
          * are what the account is sold as, and the address beside them is
          * the part that gives way when the line runs short. A plan the
          * provider did not report is left out rather than shown as an empty
          * pill, which read as a placeholder never filled in. */}
        {(credential?.plan || parked) && (
          <span className="qg-achips">
            {credential?.plan && <span className="qg-chip">{credential.plan}</span>}
            {parked && <span className="qg-chip qg-chip-route">{parked}</span>}
          </span>
        )}
      </div>

      {!compact &&
        (degraded ? (
          // The figures stay: they were true when they were taken. What goes
          // is the bar, because its whole job is to be read at a glance and
          // this reading has not earned that.
          <span className="qg-astate">{STATE_LABELS[entry.state] ?? entry.state}</span>
        ) : (
          <Bar fraction={entry.remainingFraction} percent={entry.remainingPercent} slot={slot} />
        ))}

      {figureLine && (
        <div className="qg-afig">
          {percent && (
            <span className="qg-ap">
              <b className={`num ${LEVEL_INK[entry.level] ?? ""}`}>
                {entry.hasReading ? `${entry.remainingPercent}%` : "—"}
              </b>
              {entry.hasReading && " left"}
            </span>
          )}
          {/* Routing, not quota: the same strip on every card the credential
            * appears in, because a request is made against a credential and
            * not against one of its windows. Absent entirely when the host
            * reports no counter. */}
          {credential?.activity && <Traffic activity={credential.activity} />}
          {resets && (
            <span className="qg-ar">
              {reset.resetting ? (
                <b className="num">{reset.text}</b>
              ) : (
                <>
                  resets <b className="num">{reset.text}</b>
                </>
              )}
            </span>
          )}
          {/* At the line's right end beside a strip; at its start when it is
            * the line's only content, where it reads straight on from the
            * address above instead of hanging alone at the far edge. */}
          {creditsInline && (
            <span className={credential?.activity ? "qg-ar" : "qg-ap"}>
              <CreditsFigure credits={credits} />
            </span>
          )}
        </div>
      )}

      {/* The account line, absent entirely when the provider reports neither
        * renewal nor credit. It repeats on every card the credential appears
        * in for the same reason the traffic strip does: it belongs to the
        * credential. */}
      {(renewal || (credits && !creditsInline)) && (
        <div className="qg-amuted">
          <span className="truncate">{renewal}</span>
          {credits && !creditsInline && <CreditsFigure credits={credits} />}
        </div>
      )}
    </div>
  )
}
