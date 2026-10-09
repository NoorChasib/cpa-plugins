import { useId } from "react"

import { Flags, FoldButton } from "./Fold"
import { accountFlags, ROUTING_LABELS } from "../lib/accounts"
import { useNowSeconds } from "../lib/now"
import { type AccountName, nameText, shortName } from "../lib/pool"
import { calendarDaysUntil, formatAgo, formatDate, formatDuration } from "../lib/time"
import type { Activity, Credential, Credits } from "../lib/types"

/**
 * What each of a provider's accounts is, said once for the provider: its plan,
 * when its subscription renews, what it holds in credit, and what CPA has
 * routed to it. None of it belongs to a window, so none of it repeats on the
 * window cards above.
 */

/**
 * The address, with the domain in muted ink so the distinguishing word reads
 * first. Never initials: a column of initials is unreadable when four of them
 * begin with the same letter. `short` drops the domain for a line that has no
 * room for it; the whole address stays in the hover.
 */
function Email({ address, short = false }: { address: string; short?: boolean }) {
  const at = address.indexOf("@")
  const [local, domain] = at > 0 ? [address.slice(0, at), address.slice(at)] : [address, ""]
  return (
    <span className="qg-aname" title={address}>
      {local}
      {domain && <span className={`text-ink-3 ${short ? "qg-long" : ""}`}>{domain}</span>}
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
        <i key={index} className={bucket.failed > 0 ? "act-f" : `act-${Math.min(bucket.intensity, 3)}`} />
      ))}
    </span>
  )
}

/**
 * The strip's words: how many, how many failed, and when the last landed.
 *
 * Successes and failures are named separately rather than summed: "3 req" over
 * a strip that is two thirds red describes the same numbers and none of the
 * situation. "now" comes from the server's own `live` flag rather than from
 * comparing `lastRequestAtEpoch` to the clock: only the server knows which
 * bucket is the one in progress. The success count is the part that gives way
 * on a narrow line (`qg-long`) — beside failures, or on a line that must also
 * hold the renewal (`brief`); the strip already shows roughly how much.
 */
function TrafficText({ activity, span, brief = false }: { activity: Activity; span: string; brief?: boolean }) {
  const now = useNowSeconds()
  if (activity.success === 0 && activity.failed === 0) {
    return (
      <span className="qg-atxt num text-ink-4" title={`no requests in the last ${span}`}>
        idle
      </span>
    )
  }
  const when = activity.live
    ? "now"
    : activity.lastRequestAtEpoch !== null
      ? formatAgo(activity.lastRequestAtEpoch, now)
      : span
  return (
    <span className={`qg-atxt num ${activity.live && activity.failed === 0 ? "text-accent" : "text-ink-3"}`}>
      {activity.success > 0 && (
        <span className={activity.failed > 0 || brief ? "qg-long" : ""}>
          {activity.success} req
          {" · "}
        </span>
      )}
      {activity.failed > 0 && (
        <>
          <span className="text-crit">{activity.failed} failed</span>
          {" · "}
        </>
      )}
      {when}
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

function CreditsFigure({ credits }: { credits: Credits }) {
  const text = creditsText(credits)
  return (
    <span>
      <b className={text.numeric ? "num" : ""}>{text.figure}</b> {text.unit}
    </span>
  )
}

const DAY_SECONDS = 86400

/**
 * When the subscription renews: the date, so it can be checked against a
 * receipt, and the distance, ticking like every other countdown here.
 *
 * An estimate carries "~" on its date, which the card's foot explains once,
 * and a hover that explains it in full. Its distance is calendar days, so it
 * agrees with the date beside it: a date that can be days out, if the billing
 * day has moved, has no business being counted down to the hour. One that comes round while the page is open is
 * due rather than passed, since the next document will already carry the
 * anniversary after it.
 */
function Renewal({ credential, now }: { credential: Credential; now: number }) {
  // Checked by type rather than against null: a document from a plugin that
  // predates these fields has neither, and must render as it always did.
  if (typeof credential.renewalAtEpoch !== "number") return null
  const estimated = credential.renewalEstimated === true
  const remaining = credential.renewalAtEpoch - now
  const date = `${estimated ? "~" : ""}${formatDate(credential.renewalAtEpoch)}`
  if (remaining <= 0) return <span>{estimated ? "renewal due" : "renewal date passed"}</span>
  const distance =
    estimated && remaining >= DAY_SECONDS ? `${calendarDaysUntil(credential.renewalAtEpoch, now)}d` : formatDuration(remaining)
  return (
    <span title={estimated ? ESTIMATED_RENEWAL_TITLE : undefined}>
      renews <b>{date}</b>
      <span className="qg-long"> · in {distance}</span>
    </span>
  )
}

/** What an estimated renewal is, for the reader who wonders. */
const ESTIMATED_RENEWAL_TITLE =
  "Estimated from when the subscription started. Anthropic does not report the renewal date, so this can be off if the billing date has moved."

/**
 * Who the account is, its plan and whether CPA has parked it. Full plan
 * names, never truncated: "SuperGrok Heavy" and "Enterprise" are what the
 * account is sold as, and the address beside them is the part that gives way.
 * A plan the provider did not report is left out rather than shown as an
 * empty pill.
 */
function Who({ credential, short }: { credential: Credential; short: boolean }) {
  const parked = ROUTING_LABELS[credential.status]
  return (
    <span className="qg-awho">
      <Email address={credential.email || credential.id} short={short} />
      {credential.plan && <span className="qg-chip">{credential.plan}</span>}
      {parked && <span className="qg-chip qg-chip-route">{parked}</span>}
    </span>
  )
}

/** One account in a provider's Accounts card. */
function AccountRow({ credential, now }: { credential: Credential; now: number }) {
  const activity = credential.activity
  const span = activity ? formatDuration(activity.windowSeconds) : ""
  return (
    <div className="qg-arow qg-arow-acct">
      <Who credential={credential} short />
      <span className="qg-asub">
        {typeof credential.renewalAtEpoch === "number" ? (
          <Renewal credential={credential} now={now} />
        ) : (
          <span className="text-ink-4">no renewal date</span>
        )}
        {credential.credits && (
          <>
            {" · "}
            <CreditsFigure credits={credential.credits} />
          </>
        )}
      </span>
      {/* Absent entirely when the host reports no counter: null is not idle. */}
      {activity && (
        <>
          <TrafficStrip activity={activity} span={span} />
          <TrafficText activity={activity} span={span} />
        </>
      )}
    </div>
  )
}

/**
 * A provider's accounts, once: a card shut by default whose fold line names
 * any account whose requests are failing or that CPA has parked. Open, a
 * line per account.
 *
 * The fold sits in the card's heading, so the card is found by heading like
 * every other; the button inside it is what opens it.
 */
export function AccountsCard({
  held,
  names,
  open,
  onToggle,
}: {
  held: Credential[]
  names: Map<string, AccountName>
  open: boolean
  onToggle: () => void
}) {
  const now = useNowSeconds()
  const bodyID = useId()
  const lineID = useId()
  const ring = held.find((credential) => credential.activity)?.activity
  const flags = accountFlags(
    held,
    held.map((credential) =>
      nameText(names.get(credential.id) ?? { local: shortName(credential, credential.id), qualifier: "" }),
    ),
    (epoch) => formatAgo(epoch, now),
  )
  // The strip's column, as wide as the longest ring among the accounts, so a
  // row without a counter keeps the same columns as one with: each row is a
  // grid of its own, and an `auto` track would be as wide as its own strip.
  const buckets = Math.max(0, ...held.map((credential) => credential.activity?.buckets.length ?? 0))
  const estimated = held.some(
    (credential) => credential.renewalEstimated === true && typeof credential.renewalAtEpoch === "number",
  )
  return (
    <article
      className="qg-win qg-roster"
      aria-label="Accounts"
      style={{ "--qg-act-n": buckets } as React.CSSProperties}
    >
      <h3 className="qg-fold-h">
        <FoldButton open={open} controls={bodyID} labelledBy={lineID} onToggle={onToggle} className="qg-fold-head">
          <span id={lineID} className="qg-fold-line">
            <span className="qg-rtitle">Accounts</span>
            <span className="qg-rnote qg-long">
              {ring ? `plan, renewal and requests · last ${formatDuration(ring.windowSeconds)}` : "plan and renewal"}
            </span>
            <Flags flags={flags} />
          </span>
        </FoldButton>
      </h3>
      <div id={bodyID} hidden={!open} className="qg-rows">
        {open && (
          <>
            {/* The catalog's order, which every window card above repeats. */}
            {held.map((credential) => (
              <AccountRow key={credential.id} credential={credential} now={now} />
            ))}
            {estimated && (
              <p className="qg-cfoot">
                Dates with <b>~</b> are estimated from when the subscription started.
              </p>
            )}
          </>
        )}
      </div>
    </article>
  )
}

/**
 * A provider's only account, as one slim line: no fold, no visible title,
 * because there is nothing to choose between. Who, its plan, its requests,
 * and its renewal and credit at the far end — in the popover, the requests
 * and the renewal share the second line, the strip saying how many.
 *
 * The heading is for assistive technology alone, so the card is found by
 * heading like every other.
 */
export function SoloAccount({ credential, title }: { credential: Credential; title: string }) {
  const now = useNowSeconds()
  const activity = credential.activity
  const span = activity ? formatDuration(activity.windowSeconds) : ""
  const renews = typeof credential.renewalAtEpoch === "number"
  return (
    <div className="qg-win qg-solo" role="group" aria-label={`${title} account`}>
      <h3 className="sr-only">{title} account</h3>
      <Who credential={credential} short={false} />
      {activity && (
        <span className="qg-traffic">
          <TrafficStrip activity={activity} span={span} />
          <TrafficText activity={activity} span={span} brief />
        </span>
      )}
      {(renews || credential.credits) && (
        <span className="qg-ameta">
          {renews && <Renewal credential={credential} now={now} />}
          {renews && credential.credits && " · "}
          {credential.credits && <CreditsFigure credits={credential.credits} />}
        </span>
      )}
    </div>
  )
}
