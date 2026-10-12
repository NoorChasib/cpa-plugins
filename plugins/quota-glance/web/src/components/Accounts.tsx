import { useId } from "react"

import { QuotaCacheLink, SetHereDot } from "./Edit"
import { Flags, FoldButton } from "./Fold"
import { accountFlags, RENEWAL_ENTERED_TITLE, renewalFoot, renewalMark, renewalTrouble, ROUTING_LABELS } from "../lib/accounts"
import { ENTERED_HERE } from "../lib/apicredits"
import { useNowSeconds } from "../lib/now"
import { type AccountName, nameText, shortName } from "../lib/pool"
import { calendarDaysUntil, formatAgo, formatDate, formatDuration, formatUTCDate } from "../lib/time"
import type { Activity, APICreditEditing, Credential, Credits } from "../lib/types"

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

/** Calendar days between two instants' UTC dates, for a date set here and printed as UTC sees it. */
const utcDaysUntil = (epoch: number, now: number) => Math.floor(epoch / DAY_SECONDS) - Math.floor(now / DAY_SECONDS)

/**
 * When the subscription renews: the date, so it can be checked against a
 * receipt, and the distance, ticking like every other countdown here.
 *
 * An estimate carries "~" on its date, and a date entered on Quota Cache's
 * page a dot after it; the card's foot explains both once, and a hover in
 * full. Either one's distance is calendar days, so it agrees with the date
 * beside it — an entered date on the UTC calendar it was entered on. One that
 * comes round while the page is open is due rather than passed, since the
 * next document will already carry the anniversary after it.
 */
function Renewal({ credential, now }: { credential: Credential; now: number }) {
  const mark = renewalMark(credential)
  if (mark === null) return null
  const remaining = mark.atEpoch - now
  const date = `${mark.estimated ? "~" : ""}${mark.utc ? formatUTCDate(mark.atEpoch) : formatDate(mark.atEpoch)}`
  if (remaining <= 0) return <span>{mark.estimated || mark.setHere ? "renewal due" : "renewal date passed"}</span>
  const distance =
    mark.setHere && remaining >= DAY_SECONDS
      ? `${utcDaysUntil(mark.atEpoch, now)}d`
      : mark.estimated && remaining >= DAY_SECONDS
        ? `${calendarDaysUntil(mark.atEpoch, now)}d`
        : formatDuration(remaining)
  return (
    <span title={mark.estimated ? ESTIMATED_RENEWAL_TITLE : undefined}>
      renews <b>{date}</b>
      {mark.setHere && <SetHereDot title={RENEWAL_ENTERED_TITLE} />}
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
 * On Claude the foot links to Quota Cache's page (`link`), where renewal
 * dates are entered, while this browser may follow one. The dates themselves
 * are read-only here.
 *
 * The fold sits in the card's heading, so the card is found by heading like
 * every other; the button inside it is what opens it.
 */
export function AccountsCard({
  held,
  names,
  open,
  onToggle,
  link = null,
  editing = null,
}: {
  held: Credential[]
  names: Map<string, AccountName>
  open: boolean
  onToggle: () => void
  /** Quota Cache's page at its renewal section, or null for no link. */
  link?: string | null
  /** The API credit card's editing state, which says whether Quota Cache can use its saved renewal dates. */
  editing?: APICreditEditing | null
}) {
  const now = useNowSeconds()
  const bodyID = useId()
  const lineID = useId()

  const ring = held.find((credential) => credential.activity)?.activity
  const nameOf = (credential: Credential) =>
    nameText(names.get(credential.id) ?? { local: shortName(credential, credential.id), qualifier: "" })
  const flags = accountFlags(held, held.map(nameOf), (epoch) => formatAgo(epoch, now))
  // The strip's column, as wide as the longest ring among the accounts, so a
  // row without a counter keeps the same columns as one with: each row is a
  // grid of its own, and an `auto` track would be as wide as its own strip.
  const buckets = Math.max(0, ...held.map((credential) => credential.activity?.buckets.length ?? 0))
  const foot = renewalFoot(held)
  // Renewal dates live in the same saved settings as the API credits, so
  // when Quota Cache cannot read those, its entered dates do not apply either
  // and the rows show estimates, which the foot says in the card's own words.
  const trouble = editing ? renewalTrouble(editing) : null

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
            {(foot.estimated || foot.setHere || link !== null || trouble) && (
              <div className="qg-afoot">
                <span>
                  {foot.estimated && (
                    <>
                      <b>~</b> estimated from when the subscription started
                    </>
                  )}
                  {foot.estimated && foot.setHere && " · "}
                  {foot.setHere && (
                    <>
                      <SetHereDot lead />
                      <b>{ENTERED_HERE}</b>
                    </>
                  )}
                  {trouble && (
                    <>
                      {(foot.estimated || foot.setHere) && <br />}
                      {trouble}
                    </>
                  )}
                </span>
                <QuotaCacheLink url={link}>Edit in Quota Cache</QuotaCacheLink>
              </div>
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
