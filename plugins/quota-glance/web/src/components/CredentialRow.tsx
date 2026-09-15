import { useNowSeconds } from "../lib/now"
import { formatAgo, formatDuration, formatReset } from "../lib/time"
import type { Activity, Credential, RowEntry } from "../lib/types"

/**
 * The address, with the domain in muted ink so the distinguishing word reads
 * first. Never initials: at 390px the full address still fits, and a column of
 * initials is unreadable when four of them begin with the same letter.
 *
 * min-w-0 because this sits in a flex row beside the routing marker, and a flex
 * item defaults to min-width:auto — which refuses to shrink below its content
 * and would push the marker off the card at 390px rather than truncate.
 */
function Email({ address }: { address: string }) {
  const at = address.indexOf("@")
  const [local, domain] = at > 0 ? [address.slice(0, at), address.slice(at)] : [address, ""]
  return (
    <div className="min-w-0 truncate text-[12.5px] text-ink">
      {local}
      {domain && <span className="text-ink-3">{domain}</span>}
    </div>
  )
}

function Bar({ fraction, critical }: { fraction: number; critical: boolean }) {
  // Sized from the fraction and labelled from the integer percent, both of
  // which the server sends, so the bar and the number beside it cannot
  // disagree. Red is the one exception to the single accent colour, and it
  // follows the server's level rather than a threshold repeated here.
  const width = Math.max(0, Math.min(1, fraction)) * 100
  return (
    <span className="relative block h-[6px] overflow-hidden rounded-[3px] bg-track">
      <span
        className={`absolute inset-y-0 left-0 rounded-[3px] ${critical ? "bg-crit" : "bg-accent"}`}
        style={{ width: `${width}%` }}
      />
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
    <div className="mt-[5px] flex items-center gap-[9px]">
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
            {/* The count goes first and goes first at 390px, where the column
              * is half as wide: the strip beside it already shows how much,
              * roughly, and only this line can say when. */}
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
    </div>
  )
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

export function CredentialRow({ entry, credential }: { entry: RowEntry; credential: Credential | undefined }) {
  const now = useNowSeconds()
  const reset = formatReset(entry.resetDisplayHint, entry.resetAtEpoch, now)
  // An unknown state is still a state: dim the row and print it rather than
  // drawing a confident bar over a reading the server would not vouch for.
  const degraded = entry.state !== "ok"
  const parked = credential ? ROUTING_LABELS[credential.status] : undefined

  return (
    <div className={`cred ${degraded ? "opacity-60" : ""}`}>
      <div className="cred-mail min-w-0">
        <div className="flex min-w-0 items-baseline gap-[6px]">
          <Email address={credential?.email ?? entry.credentialId} />
          {parked && (
            <span className="shrink-0 text-[10.5px] text-ink-3">{parked}</span>
          )}
        </div>
        {/* Routing, not quota: the same strip on every card the credential
          * appears in, because a request is made against a credential and not
          * against one of its windows. Absent entirely when the host reports no
          * counter, which leaves the row exactly as it was. */}
        {credential?.activity && <Traffic activity={credential.activity} />}
      </div>

      {/* Fixed column, so the bars line up down the card whatever the plans are
        * called. A long tier name — "SuperGrok Heavy" is a real one — truncates
        * rather than growing the column or spilling over the bar beside it; the
        * full string stays available on hover and to a screen reader. */}
      <span
        className="cred-plan max-w-full justify-self-start truncate rounded-[5px] border border-line
          bg-card-2 px-[7px] py-[2px] text-[10.5px] text-ink-2"
        title={credential?.plan || undefined}
      >
        {credential?.plan || "—"}
      </span>

      <span className="cred-bar">
        {degraded ? (
          // The figures stay: they were true when they were taken. What goes is
          // the bar, because its whole job is to be read at a glance and this
          // reading has not earned that.
          <span className="num text-[10.5px] text-ink-3">{STATE_LABELS[entry.state] ?? entry.state}</span>
        ) : (
          <Bar fraction={entry.remainingFraction} critical={entry.level === "critical"} />
        )}
      </span>

      {/* A dash, never 0%. An absent reading and an exhausted credential are the
        * same zero in the document and opposite facts on a capacity dashboard. */}
      <span className="cred-pct num text-right text-[12px] text-ink-2">
        {entry.hasReading ? `${entry.remainingPercent}%` : "—"}
      </span>

      <span className={`cred-eta num text-right text-[11px] ${reset.resetting ? "text-ink-2" : "text-ink-3"}`}>
        {reset.text}
      </span>
    </div>
  )
}
