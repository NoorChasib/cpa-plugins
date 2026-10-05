// Spending one banked rate-limit reset — a Codex reset credit or a Claude
// reset grant. The page asks the same way for both; the plugin knows which
// provider the credential is on and speaks to it.
//
// This is the only request this page makes that changes anything, and it
// cannot be undone: the reset is gone the moment the provider accepts it. So
// it is never retried, never made without the dialog's answer, and never made
// twice from one press — `confirmed` is carried with the press so a request
// that somehow arrives without one is refused by the plugin rather than
// assumed.
//
// A press goes through one door, chosen before it is sent (access.ts
// spendThrough). With the dashboard password it is a GET to the spend route,
// carrying the press in a header, because CPA dispatches nothing but GET to a
// resource route; with a console session it is a POST to the management
// redeem route. Either way it carries a press id, and the plugin's ledger
// answers a second copy of the same press with the first copy's answer rather
// than spending again. The page sends the same id again only after an answer
// it never got, so pressing again after a dropped connection asks what the
// first press came to instead of making a second one.
//
// A second press after an unknown outcome reported by the plugin is the
// reader's decision, not this page's, and it is safe only while the plugin
// makes it so: until the deadline it names, it replays the same claim rather
// than starting a fresh one, so the provider sees one request twice instead of
// two requests. Past that deadline a press is a new claim, and the page says
// so rather than promising a replay.
//
// "Nothing was spent" is printed only where an answer proves it. An answer the
// page cannot read — a gateway's error page, CPA's own plain-text 502 — proves
// only that the answer was lost, and the plugin may have finished the spend
// after it was.

import { type RefusalCode, spendThrough } from "./access"
import { managementPath, resourcePath } from "./cpa-auth"
import { accessContext, openDoors } from "./session"
import { formatClock } from "./time"

/**
 * What the plugin reports back. Outcomes widen additively, like every enum.
 *
 * Only "reset" and "nothingToReset" spent anything this time. The four
 * refusals — "notLimited", "cooldown", "paused", "ineligible" — are the
 * provider declining before anything was spent, and "alreadyUsed" is the
 * provider saying the reset this press named had been spent before it.
 */
export type RedeemOutcome =
  | "reset"
  | "nothingToReset"
  | "noCredit"
  | "failed"
  | "notLimited"
  | "cooldown"
  | "paused"
  | "ineligible"
  | "alreadyUsed"
  | (string & {})

export interface RedeemResult {
  outcome: RedeemOutcome
  windowsReset: number
  remainingCount: number
  /** The card's count comes from quota-cache and lags until its next poll. */
  snapshotPending: boolean
  /** "codex" or "claude"; empty from a plugin too old to say. */
  provider: string
  /**
   * The plugin answered from its press ledger: this is what an earlier copy of
   * the same press came to, not something this request did.
   */
  replayed: boolean
}

/**
 * A failure with something worth printing.
 *
 * `code` is one of the plugin's own fixed codes — never provider text, which
 * the plugin deliberately does not forward. Anything unrecognized falls through
 * to a generic sentence rather than putting a raw code on screen.
 */
export class RedeemError extends Error {
  constructor(
    readonly code: string,
    readonly status: number,
    /**
     * With `outcome_unknown`: the instant, in epoch seconds, until which a
     * press repeats the same claim. Null when the plugin did not say, which
     * promises nothing.
     */
    readonly retryUntilEpoch: number | null = null,
    /** As RedeemResult.replayed: the plugin's answer to an earlier copy. */
    readonly replayed: boolean = false,
    /**
     * The press id is still pending: this answer was lost, or this press
     * repeated one whose answer was lost and was turned away before the
     * plugin could say what that one came to. See SpendAnswer.kept.
     */
    readonly kept: boolean = false,
  ) {
    super(code)
    this.name = "RedeemError"
  }
}

/**
 * Whether this browser can spend a reset at all: some door is open.
 *
 * Closed doors are closed for a reason the page already knows — no saved
 * password and no usable console session, or every credential it had refused —
 * so a press could only be refused, and a control that is certain to fail is
 * worse than no control. Above all, a console key CPA has refused is never
 * offered again: each press would be another failed management sign-in
 * counted against this address.
 */
export function canRedeemHere(): boolean {
  return openDoors().length > 0
}

/**
 * Carries ?redeem= through to the dev fixture route, which is how each ending
 * is reviewed. Stripped from the production bundle, where the only thing on the
 * other end is the plugin.
 */
function devEnding(url: string): string {
  if (!import.meta.env.DEV) return url
  const ending = new URLSearchParams(window.location.search).get("redeem")
  return ending === null ? url : `${url}?redeem=${encodeURIComponent(ending)}`
}

export async function redeemReset(credentialId: string): Promise<RedeemResult> {
  const answer = await spendThrough(accessContext(), credentialId, {
    tokenURL: devEnding(resourcePath("/spend")),
    consoleURL: devEnding(managementPath("/redeem")),
  })
  if (!answer.sent) throw new RedeemError(answer.code, answer.status)
  // The request never completed. Whether it landed is genuinely unknown, and
  // the caller says so rather than inviting a second press.
  if (!answer.answered) throw new RedeemError("unreachable", 0)
  const { status, replayed, kept } = answer
  // Turned away before the plugin acted: by CPA, by something in front of it,
  // or by the plugin's own gate. Named, because what to do next differs.
  if (answer.refusal) throw new RedeemError(answer.refusal.code, status, null, replayed, kept)

  const body = (answer.json && answer.body !== null && typeof answer.body === "object" ? answer.body : {}) as {
    error?: unknown
    retryUntilEpoch?: unknown
  } & Partial<RedeemResult>
  if (!answer.ok) {
    if (typeof body.error === "string" && body.error !== "") {
      const until = typeof body.retryUntilEpoch === "number" ? body.retryUntilEpoch : null
      throw new RedeemError(body.error, status, until, replayed, kept)
    }
    throw new RedeemError("unknown", status, null, replayed, kept)
  }
  // The plugin answers 200 only with an outcome, and the commonest outcome is
  // a reset spent. An unreadable 200 is that answer lost on the way back, not
  // a press that did nothing.
  if (!answer.json) throw new RedeemError("unreadable", status, null, false, kept)
  return {
    outcome: typeof body.outcome === "string" ? body.outcome : "reset",
    windowsReset: typeof body.windowsReset === "number" ? body.windowsReset : 0,
    remainingCount: typeof body.remainingCount === "number" ? body.remainingCount : 0,
    snapshotPending: body.snapshotPending !== false,
    provider: typeof body.provider === "string" ? body.provider : "",
    replayed,
  }
}

/**
 * The provider's name as a sentence uses it, from the credential's own
 * provider id.
 *
 * Only the two providers that bank resets are named. Anything else reads as
 * "the provider" rather than as a raw id, which keeps every sentence below a
 * sentence; the plugin refuses such a credential before contacting anyone, so
 * the fallback is a guard rather than a path.
 */
const PROVIDER_NAMES: Record<string, string> = { codex: "Codex", claude: "Claude" }

function nameOf(provider: string): string {
  return PROVIDER_NAMES[provider] ?? "the provider"
}

/** The same name, opening a sentence. */
function opening(name: string): string {
  return name.charAt(0).toUpperCase() + name.slice(1)
}

/**
 * Whether the press ended without the reset it asked for.
 *
 * It sets the colour of the line, not its words. A refusal is printed in the
 * failure ink even though nothing was lost, because the reader's windows are
 * exactly where they were and that is the fact they pressed the button to
 * change.
 */
export function fellShort(result: RedeemResult): boolean {
  switch (result.outcome) {
    case "noCredit":
    case "failed":
    case "notLimited":
    case "cooldown":
    case "paused":
    case "ineligible":
      return true
    default:
      return false
  }
}

/**
 * What to tell the reader, in whole sentences, for each ending.
 *
 * `provider` is the credential's own provider; the plugin's answer names one
 * too, and that is preferred when present because it is the one that was
 * actually spoken to.
 */
export function describeOutcome(result: RedeemResult, provider: string): string {
  return fromEarlier(result.replayed, outcomeSentence(result, provider))
}

/**
 * Marks an answer the plugin's ledger replayed: what an earlier copy of this
 * press came to. Without it a reader who pressed again after a dropped
 * connection would read "Reset applied" as a second reset spent.
 */
function fromEarlier(replayed: boolean | undefined, text: string): string {
  return replayed ? `From your earlier press: ${text}` : text
}

function outcomeSentence(result: RedeemResult, provider: string): string {
  const who = opening(nameOf(result.provider || provider))
  const left =
    result.remainingCount === 0
      ? "No banked resets left."
      : `${result.remainingCount} banked reset${result.remainingCount === 1 ? "" : "s"} left.`
  const lag = result.snapshotPending ? " The card updates at the next quota-cache poll." : ""
  switch (result.outcome) {
    case "reset":
      return `Reset applied. ${left}${lag}`
    case "nothingToReset":
      // The reset is gone either way. Saying otherwise would be a lie the
      // reader acts on when they check their windows.
      return `The reset was spent, but no window needed clearing. ${left}${lag}`
    case "noCredit":
      // Usually a count that was spent elsewhere since the poll; on Claude it is
      // also a grant the provider will not let be used, without saying why.
      return `${who} reported no reset this account can spend right now — the count on this card may be out of date. Nothing was spent.`
    case "alreadyUsed":
      // Most often the answer to a second press after an unknown outcome: the
      // claim being replayed is one the provider had already honoured. Either
      // way this press spent nothing, and that is the half the reader needs.
      return `${who} reports that reset as already used, so this press spent nothing. If an earlier press did not finish, it went through. ${left}${lag}`
    // The four refusals. The provider declined before spending anything, and
    // each says what would have to change for the next press to work.
    case "notLimited":
      return `${who} allows a reset only once the account has hit a usage limit, and this one has not. Nothing was spent.`
    case "cooldown":
      return `${who} has this account in a cooldown after its last reset. Nothing was spent — try again once the cooldown ends.`
    case "paused":
      return `${who} has paused resets on this account. Nothing was spent.`
    case "ineligible":
      return `${who} says this account is not eligible to use a reset right now. Nothing was spent.`
    case "failed":
      // Deliberately not "nothing was spent": this is the plugin's word for a
      // refusal it could not classify, and it does not promise that.
      return `${who} did not apply the reset. Check this account’s usage before pressing again.`
    default:
      return `Done. ${left}${lag}`
  }
}

/**
 * The sentence for an answer that says only that the answer was lost: the
 * request may have landed, and the plugin may have finished the spend after
 * this page stopped hearing from it. Deliberately not "nothing was spent", and
 * not "press again" — nothing here promises a second press would replay the
 * first.
 */
function lostAnswer(lead: string, name: string): string {
  return `${lead}, so whether ${name} applied the reset is unknown. Check this account’s usage before pressing again.`
}

/**
 * What to tell the reader when it failed, per the plugin's own codes and the
 * refusals access.ts names.
 */
export function describeError(error: unknown, provider: string): string {
  const name = nameOf(provider)
  if (!(error instanceof RedeemError)) return lostAnswer("Something went wrong", name)
  const text = fromEarlier(error.replayed, errorSentence(error, name))
  // A repeat of a press whose answer was lost, turned away before the plugin
  // ran. Its own sentence is true of this request, and says nothing of the
  // press it repeated, which may still have spent the reset — and it is the
  // notice that takes the place of the one saying so.
  return error.kept && !codeMayHaveSpent(error)
    ? `${text} It repeated an earlier press whose answer was lost, and whether ${name} applied that reset is still unknown. Check this account’s usage before pressing again.`
    : text
}

function errorSentence(error: RedeemError, name: string): string {
  const who = opening(name)
  switch (error.code) {
    case "no_session":
      return "This browser is not signed in, so nothing was sent. Sign in, then try again."
    // Refusals before the plugin ran. Each is proven to have spent nothing:
    // the plugin never answers 401 or 403 on the console door, and on the
    // token door answers them before it looks at the press.
    case "console_refused":
      return "CPA refused the console session before the plugin ran. Nothing was spent. This page has stopped using that session; sign in to the console again or use the dashboard password."
    case "token_refused":
      return `The dashboard password was refused before anything reached ${name}. Nothing was spent. Sign in again to use resets.`
    case "ip_banned":
      return "CPA is refusing management requests from this address for up to 30 minutes after repeated failed sign-ins. Nothing was spent, and nothing here will retry. The dashboard password still works."
    case "remote_disabled":
      return "CPA accepts console sessions only on the machine it runs on. Nothing was spent. Use the dashboard password here."
    case "management_off":
      return "CPA’s management API is switched off on this server. Nothing was spent. Use the dashboard password here."
    case "refused_other":
      return `Something between this page and CPA refused the request (HTTP ${error.status}). Nothing was spent.`
    case "cross_site":
      return "The plugin refused this request because it did not come from this page. Nothing was spent."
    case "too_early":
      return "The request arrived before the secure connection was confirmed. Nothing was spent; press Use one again."
    case "route_missing":
      return "This server did not offer the reset route, so nothing was spent. Reload the page; if this persists, check that Quota Glance 0.5.0 or newer is loaded."
    case "not_found":
      return "Redeeming is switched off for this dashboard."
    case "unreachable":
      return lostAnswer("The request did not complete", name)
    case "outcome_unknown": {
      // The plugin reached the provider and could not tell what happened. The
      // reset may be gone, and the reader is told so — but this is also the
      // one failure where pressing again can be safe, because the plugin
      // replays the same claim instead of making a new one. Only until the
      // deadline it names, though: that is the claim's, fixed when it was
      // first made, so it is printed as a time of day rather than restated as
      // ten fresh minutes on every answer. And only while the copy of the
      // plugin that made the claim is still the one running, because it holds
      // the claim in memory: a CPA restart ends that copy, and so does
      // updating Quota Glance or switching it off and on, which CPA does by
      // loading a fresh copy without restarting itself.
      const until = error.retryUntilEpoch
      const repeat =
        until !== null && until > Date.now() / 1000
          ? ` Until ${formatClock(until)}, pressing Use one again retries this same claim and cannot spend a second reset. After that, or if CPA or Quota Glance restarts or is updated first, a press is a new claim.`
          : " Pressing Use one again may make a new claim, which spends a second reset if this one went through."
      return `${who} did not confirm the reset, so it may have been applied. Check this account’s usage first.${repeat}`
    }
    case "retry_window_closed":
      // An earlier press left a claim unresolved and its window has closed —
      // possibly while this press was repeating it, so this does not say
      // nothing was sent. The next press is a new claim.
      return `Whether ${name} applied an earlier reset is still unknown, and that claim can no longer be retried. Check this account’s usage before pressing again: the next press is a new claim, and spends a second reset if the earlier one went through.`
    case "provider_rate_limited":
      return `${who} is rate-limiting requests right now. Nothing was spent — wait a few minutes before trying again.`
    case "provider_refused":
      return `${who} refused the request. Nothing was spent.`
    case "provider_unavailable":
      return `${who} could not be reached. Nothing was spent.`
    case "already_in_flight":
      // Three causes share the code: a press on this credential still under
      // way, from this page or another, a second copy of this press that
      // outwaited the first, and a press ledger holding as many recent presses
      // as it may. None of them sent anything for this request. The first two
      // can still spend a reset, which the card will not show until the next
      // poll, so this does not invite another press: one made once the earlier
      // finishes is a new press, and spends a second reset if the account
      // holds one.
      return "The plugin is still busy with an earlier press on this account, from here or another device — or with so many in the last ten minutes that it is taking no new ones yet. This request spent nothing itself, but an earlier press may still spend a reset, and the card will not show it until the next quota-cache poll. Check this account’s usage before pressing again."
    case "not_redeemable":
      return "This credential has nothing to redeem right now."
    case "credential_unusable":
      // Two causes share the code: a credential with no usable sign-in, and the
      // press after an unknown outcome finding the credential now signed in to
      // a different account. The second leaves the earlier press as unknown as
      // it was, so this says only what this press did.
      return "This credential cannot redeem a reset: it has no usable sign-in, or it now signs in to a different account than an earlier press that did not finish. This press sent nothing."
    case "confirmation_required":
      return "The confirmation did not reach the plugin. Nothing was spent."
    default:
      // A code this page does not know, or no readable code at all. A 4xx is a
      // request turned away before the plugin acted — CPA's sign-in gate, the
      // plugin's own checks — so that much is proven. Anything else is not: a
      // reverse proxy's 502 or 504 page, CPA's plain-text 502, an unreadable
      // 200 all mean the answer was lost, not that nothing happened.
      if (error.status === 404) return "Redeeming is switched off for this dashboard."
      if (error.status >= 400 && error.status < 500) return "The reset could not be applied. Nothing was spent."
      return lostAnswer("The answer did not come back in a form this page can read", name)
  }
}

/** Every refusal access.ts names, each of which spent nothing. */
const REFUSALS: ReadonlySet<string> = new Set<RefusalCode>([
  "console_refused",
  "ip_banned",
  "remote_disabled",
  "management_off",
  "refused_other",
  "token_refused",
  "cross_site",
  "too_early",
  "not_found",
  "route_missing",
])

/**
 * Whether a failed press may still have spent the reset — the answers whose
 * sentence above says "unknown" or "may have been applied" rather than
 * "nothing was spent". The same partition describeError draws, kept beside it
 * so the two cannot drift: a code it treats as proof that nothing was sent is
 * false here, and every answer it calls lost is true. So is a repeat turned
 * away while the press it repeated is still unknown.
 */
export function mayHaveSpent(error: unknown): boolean {
  if (!(error instanceof RedeemError)) return true
  return error.kept || codeMayHaveSpent(error)
}

/** mayHaveSpent by the answer's own code and status alone. */
function codeMayHaveSpent(error: RedeemError): boolean {
  if (REFUSALS.has(error.code)) return false
  switch (error.code) {
    case "unreachable":
    case "outcome_unknown":
    case "retry_window_closed":
    // An earlier press on the account may still spend one.
    case "already_in_flight":
      return true
    case "no_session":
    case "provider_rate_limited":
    case "provider_refused":
    case "provider_unavailable":
    case "not_redeemable":
    case "credential_unusable":
    case "confirmation_required":
      return false
    default:
      // describeError's own fallback: a 4xx is a request turned away before
      // the plugin acted; anything else is an answer that was lost.
      return !(error.status >= 400 && error.status < 500)
  }
}
