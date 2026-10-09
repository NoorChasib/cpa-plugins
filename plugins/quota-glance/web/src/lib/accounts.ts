// What a provider's Accounts card says about each account: whether CPA has
// parked it, when its requests last failed, and where its renewal date comes
// from.
//
// Imports nothing but types, so node loads it directly in test/.

import type { FoldFlag } from "./pool"
import type { Activity, Credential, RenewalOrphan } from "./types"

/**
 * CPA will not route to this credential right now. Not a reading's state: the
 * reading beside it is perfectly good. This says the credential is parked.
 */
export const ROUTING_LABELS: Record<string, string> = {
  unavailable: "cooldown",
  disabled: "off",
}

/**
 * When the newest failed request landed, as near as the ring can say: `live`
 * when it is in the bucket in progress, else an upper bound on the instant,
 * else null when there is nothing to date it by.
 *
 * The ring counts per bucket, so the only instant the document carries is
 * `lastRequestAtEpoch`, the end of the newest busy bucket. A failure in an
 * older bucket ended a whole number of buckets before that one, which is all
 * this subtracts: the server's own instant, moved back by the server's own
 * bucket width. Null when the window holds no failure at all.
 */
export function lastFailure(
  activity: Pick<Activity, "buckets" | "bucketSeconds" | "failed" | "lastRequestAtEpoch">,
): { live: boolean; atEpoch: number | null } | null {
  if (activity.failed <= 0) return null
  let failedAt = -1
  let busyAt = -1
  activity.buckets.forEach((bucket, index) => {
    if (bucket.failed > 0) failedAt = index
    if (bucket.success > 0 || bucket.failed > 0) busyAt = index
  })
  // The totals say there were failures and the ring shows none: a document
  // this bundle cannot reconcile, so it says when without guessing.
  if (failedAt < 0) return { live: false, atEpoch: null }
  if (failedAt === activity.buckets.length - 1) return { live: true, atEpoch: activity.lastRequestAtEpoch }
  if (activity.lastRequestAtEpoch === null) return { live: false, atEpoch: null }
  return { live: false, atEpoch: activity.lastRequestAtEpoch - (busyAt - failedAt) * activity.bucketSeconds }
}

/**
 * The shut Accounts card's flags, in catalog order: each account whose
 * requests have been failing — how many, and how long ago the newest landed —
 * and each CPA has parked, in the word its row uses. A parked account is
 * named on the shut card because nothing else on a page of shut cards says
 * CPA has stopped sending it requests.
 *
 * `names` runs parallel to `held`; `ago` prints how long ago an instant was.
 */
export function accountFlags(
  held: Pick<Credential, "id" | "status" | "activity">[],
  names: string[],
  ago: (epoch: number) => string,
): FoldFlag[] {
  return held.flatMap((credential, index): FoldFlag[] => {
    const activity = credential.activity
    const failed = activity ? lastFailure(activity) : null
    const parked = ROUTING_LABELS[credential.status]
    if (!failed && !parked) return []
    const name = names[index] ?? credential.id
    if (!failed || !activity) return [{ id: credential.id, name, figure: "", word: parked!, tone: "" }]
    const when = failed.live ? "now" : failed.atEpoch !== null ? ago(failed.atEpoch) : ""
    const tail = [when, parked].filter(Boolean).join(" · ")
    return [
      {
        id: credential.id,
        name,
        figure: `${activity.failed} failed`,
        word: tail ? `· ${tail}` : "",
        tone: "critical",
      },
    ]
  })
}

/**
 * A renewal date as a row prints it: the instant, whether it is an estimate
 * ("~Oct 29") or was set on the dashboard (a dot after it), and which clock
 * it is read on. A date set here is 00:00 UTC on the day the reader picked,
 * so it is printed as UTC sees it; west of Greenwich the reader's own zone
 * would put it on the evening before. Null with no renewal to show.
 */
export interface RenewalMark {
  atEpoch: number
  source: "reported" | "dashboard" | "estimated"
  estimated: boolean
  setHere: boolean
  utc: boolean
}

export function renewalMark(
  credential: Pick<Credential, "renewalAtEpoch" | "renewalEstimated" | "renewalSource">,
): RenewalMark | null {
  // Checked by type rather than against null: a document from a plugin that
  // predates these fields has neither, and must render as it always did.
  if (typeof credential.renewalAtEpoch !== "number") return null
  const source =
    credential.renewalSource === "dashboard"
      ? "dashboard"
      : credential.renewalSource === "estimated" || credential.renewalEstimated === true
        ? "estimated"
        : "reported"
  return {
    atEpoch: credential.renewalAtEpoch,
    source,
    estimated: source === "estimated",
    setHere: source === "dashboard",
    utc: source === "dashboard",
  }
}

/**
 * Whether an estimated renewal is a yearly plan's. A monthly estimate is the
 * next monthly anniversary, never more than a month off; one further away can
 * only be an annual plan's.
 */
export function yearlyEstimate(
  credential: Pick<Credential, "renewalAtEpoch" | "renewalSource" | "renewalEstimated">,
  now: number,
): boolean {
  const mark = renewalMark(credential)
  return mark !== null && mark.estimated && mark.atEpoch - now > 31 * 86400
}

/** Claude keeps the renewal editor even with one account, or only saved orphans. */
export function accountsCardNeeded(provider: string, count: number, orphanCount: number): boolean {
  return count > 1 || (provider === "claude" && (count > 0 || orphanCount > 0))
}

/** A CPA auth index as an orphan line names it: "0123…cdef". */
export function shortCredentialId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 4)}…${id.slice(-4)}` : id
}

/** The editor's last line for a renewal date kept for a credential CPA no longer lists. */
export function renewalOrphanText(orphan: Pick<RenewalOrphan, "id">): string {
  return `Saved renewal date for an account no longer in CPA: ${shortCredentialId(orphan.id)}`
}

/**
 * Whether the Accounts card has anything to say about renewal dates in its
 * foot: an estimate to explain, a date set here to explain, or one the reader
 * may set.
 */
export function renewalFoot(held: Pick<Credential, "renewalAtEpoch" | "renewalEstimated" | "renewalSource" | "renewalEditable">[]): {
  estimated: boolean
  setHere: boolean
  editable: boolean
} {
  const marks = held.map(renewalMark)
  return {
    estimated: marks.some((mark) => mark?.estimated === true),
    setHere: marks.some((mark) => mark?.setHere === true),
    editable: held.some((credential) => credential.renewalEditable === true),
  }
}
