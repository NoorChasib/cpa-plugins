// Spending one banked Codex rate-limit reset.
//
// This is the only request this page makes that changes anything, and it
// cannot be undone: the credit is gone the moment the provider accepts it. So
// it is never retried, never made without the dialog's answer, and never made
// twice from one press — `confirmed` is carried in the body so a request that
// somehow arrives without one is refused by the plugin rather than assumed.

import { authHeaders, managementKey, managementPath } from "./cpa-auth"
import * as token from "./token"

/** What the plugin reports back. Outcomes widen additively, like every enum. */
export type RedeemOutcome = "reset" | "nothingToReset" | "noCredit" | (string & {})

export interface RedeemResult {
  outcome: RedeemOutcome
  windowsReset: number
  remainingCount: number
  /** The card's count comes from quota-cache and lags until its next poll. */
  snapshotPending: boolean
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
  ) {
    super(code)
    this.name = "RedeemError"
  }
}

const RESOURCE_FALLBACK = "/v0/resource/plugins/quota-glance/redeem"

function fallbackURL(): string {
  const path = window.location.pathname.replace(/\/+$/, "")
  return path.endsWith("/app") ? `${path.slice(0, -"/app".length)}/redeem` : RESOURCE_FALLBACK
}

/**
 * Where to send it, best first — the same order, and the same reasoning, as
 * reading the document: the console's key costs the reader nothing, and the
 * saved password is the only thing a reader without a console session has.
 *
 * Unlike a read, this is not retried down the list on rejection. A POST that
 * was refused may still have spent the credit, and trying the second door would
 * risk spending a second one to find out.
 */
function target(): { url: string; headers: Record<string, string>; viaConsole: boolean } | null {
  const headers = { "Content-Type": "application/json", Accept: "application/json" }
  if (managementKey() !== null) {
    return { url: managementPath("/redeem"), headers: authHeaders(headers), viaConsole: true }
  }
  const saved = token.read()
  if (saved) {
    return {
      url: fallbackURL(),
      headers: { ...headers, Authorization: ["Bearer", saved].join(" ") },
      viaConsole: false,
    }
  }
  return null
}

/**
 * Whether this browser can redeem at all.
 *
 * CPA dispatches only GET to a resource route — verified against a real CPA by
 * scripts/quota-glance-smoke.py, which reports a 404 on the public redeem path
 * — so the `web-token` door can read the document but cannot spend anything.
 * The count and the expiry still show there; the button does not, because a
 * control that is certain to fail is worse than no control.
 */
export function canRedeemHere(): boolean {
  return managementKey() !== null
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

async function codeOf(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: unknown }
    if (typeof body.error === "string" && body.error !== "") return body.error
  } catch {
    /* no body, or not JSON */
  }
  return "unknown"
}

export async function redeemReset(credentialId: string): Promise<RedeemResult> {
  const one = target()
  if (one === null) throw new RedeemError("no_session", 0)

  let response: Response
  try {
    response = await fetch(devEnding(one.url), {
      method: "POST",
      headers: one.headers,
      // The plugin refuses a body without this, so the dialog's answer travels
      // with the request rather than being implied by it having been sent.
      body: JSON.stringify({ credentialId, confirmed: true }),
      signal: AbortSignal.timeout(35_000),
      cache: "no-store",
      credentials: "same-origin",
    })
  } catch {
    // The request never completed. Whether it landed is genuinely unknown, and
    // the caller says so rather than inviting a second press.
    throw new RedeemError("unreachable", 0)
  }

  if (!response.ok) {
    // A 404 means two different things by door: from the console it is the
    // route being switched off, and from the token door it is CPA never having
    // dispatched the POST. Naming the right one is the difference between the
    // reader changing a setting and the reader signing in.
    if (response.status === 404 && !one.viaConsole) throw new RedeemError("needs_console", 404)
    throw new RedeemError(await codeOf(response), response.status)
  }
  const body = (await response.json()) as Partial<RedeemResult>
  return {
    outcome: typeof body.outcome === "string" ? body.outcome : "reset",
    windowsReset: typeof body.windowsReset === "number" ? body.windowsReset : 0,
    remainingCount: typeof body.remainingCount === "number" ? body.remainingCount : 0,
    snapshotPending: body.snapshotPending !== false,
  }
}

/** What to tell the reader, in whole sentences, for each ending. */
export function describeOutcome(result: RedeemResult): string {
  const left =
    result.remainingCount === 0
      ? "No banked resets left."
      : `${result.remainingCount} banked reset${result.remainingCount === 1 ? "" : "s"} left.`
  const lag = result.snapshotPending ? " The card updates at the next quota-cache poll." : ""
  switch (result.outcome) {
    case "reset":
      return `Reset applied. ${left}${lag}`
    case "nothingToReset":
      // The credit is gone either way. Saying otherwise would be a lie the
      // reader acts on when they check their windows.
      return `The credit was spent, but no window needed clearing. ${left}${lag}`
    case "noCredit":
      return "There was nothing left to spend — the count on this card was out of date."
    default:
      return `Done. ${left}${lag}`
  }
}

/** What to tell the reader when it failed, per the plugin's own codes. */
export function describeError(error: unknown): string {
  if (!(error instanceof RedeemError)) return "Something went wrong. Nothing was spent."
  switch (error.code) {
    case "no_session":
      return "You are signed out. Sign in again before redeeming."
    case "unreachable":
      // Deliberately not "nothing was spent": the request may have landed.
      return "The request did not complete, so whether the reset was applied is unknown. Check your Codex usage before trying again."
    case "provider_refused":
      return "Codex refused the request. Nothing was spent."
    case "provider_unavailable":
      return "Codex could not be reached. Nothing was spent."
    case "already_in_flight":
      return "A redemption is already running for this credential."
    case "not_redeemable":
      return "This credential has nothing to redeem right now."
    case "credential_unusable":
      return "This credential cannot redeem a reset — an API-key login has none."
    case "confirmation_required":
      return "The confirmation did not reach the plugin. Nothing was spent."
    case "needs_console":
      return "Redeeming needs a CPA console session; this browser is signed in with the fallback password. Nothing was spent."
    default:
      return error.status === 404
        ? "Redeeming is switched off for this dashboard."
        : "The reset could not be applied. Nothing was spent."
  }
}
