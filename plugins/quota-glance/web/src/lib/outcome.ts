// How a press that the plugin answered is told to the reader: which outcomes
// fell short, which spent a reset with CPA possibly still skipping the
// account, and the sentence for each.
//
// Kept apart from redeem.ts, which makes the request, so it imports nothing
// at all and Node can load it for its tests by stripping the types alone.

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
  /**
   * What became of CPA's own routing cooldown on the credential after a reset:
   * "cleared"; "failed" or "unsupported", where CPA may keep skipping the
   * account until its cooldown ends; or "unconfirmed", where the provider's
   * answer did not confirm the reset and the cooldown was left alone. Empty on
   * every other outcome, and from a plugin too old to clear one.
   */
  cooldown: CooldownState
}

export type CooldownState = "" | "cleared" | "failed" | "unsupported" | "unconfirmed" | (string & {})

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

export function nameOf(provider: string): string {
  return PROVIDER_NAMES[provider] ?? "the provider"
}

/** The same name, opening a sentence. */
export function opening(name: string): string {
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
 * Whether a reset was spent but CPA may still be skipping the account: the
 * clear of its routing cooldown failed, this CPA cannot clear one for a
 * plugin, or the provider's answer was not enough to ask it to.
 *
 * Its own tone, because it is neither of the others. The reset is spent and
 * must not be spent again, which a failure's red would invite; and the account
 * may not be back in use yet, which a success's green would hide.
 */
export function partlyApplied(result: RedeemResult): boolean {
  return NOT_CLEARED.has(result.cooldown)
}

/**
 * The cooldown states that leave a spent reset with CPA possibly still
 * skipping the account. Listed rather than inferred, so a state a newer plugin
 * adds is not shown as a warning with no sentence to explain it.
 */
const NOT_CLEARED: ReadonlySet<string> = new Set(["failed", "unsupported", "unconfirmed"])

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
export function fromEarlier(replayed: boolean | undefined, text: string): string {
  return replayed ? `From your earlier press: ${text}` : text
}

function outcomeSentence(result: RedeemResult, provider: string): string {
  const who = opening(nameOf(result.provider || provider))
  if (result.outcome === "reset" && result.cooldown === "unconfirmed") return unconfirmedSentence(result, who)
  const text = providerSentence(result, provider)
  const after = cooldownSentence(result)
  return after === "" ? text : `${text} ${after}`
}

/**
 * A spend the provider accepted without saying the reset was applied. The
 * credit is gone, which is what the reader must not be talked out of, but
 * "Reset applied" would claim what the answer did not.
 */
function unconfirmedSentence(result: RedeemResult, who: string): string {
  return `${who} took the reset but did not confirm what it cleared, so CPA’s cooldown on this credential was left as it was. ${leftOf(result)}${lagOf(result)} The reset is spent: don’t press Use one again. If CPA keeps skipping this account, use Clear cooldown on it in the CPA console.`
}

/**
 * What became of CPA's cooldown on the credential, after the provider's own
 * sentence, which has already said the reset is spent. Every case that leaves
 * the account possibly still skipped points at the one remedy that costs
 * nothing, CPA's Clear cooldown, rather than at another press, which would
 * spend another.
 */
function cooldownSentence(result: RedeemResult): string {
  const remedy =
    "CPA may keep skipping this account until that cooldown ends: use Clear cooldown on this credential in the CPA console. Don’t press Use one again — that would spend another reset."
  switch (result.cooldown) {
    case "cleared":
      return "CPA’s cooldown on this credential was cleared, so it is back in use."
    case "failed":
      // Not "could not be cleared": a clear that timed out, or whose answer
      // named another credential, may have happened all the same.
      return `But CPA did not confirm that it cleared its cooldown on this credential. ${remedy}`
    case "unsupported":
      // The console's own Clear cooldown predates the plugin callback, so
      // the remedy holds on this CPA too.
      return `But this CPA, older than v8.0.12, does not let a plugin clear a credential’s cooldown. ${remedy}`
    default:
      return ""
  }
}

/** What the account still holds, as a sentence. */
function leftOf(result: RedeemResult): string {
  return result.remainingCount === 0
    ? "No banked resets left."
    : `${result.remainingCount} banked reset${result.remainingCount === 1 ? "" : "s"} left.`
}

/** Why the card's count has not moved yet, when it has not. */
function lagOf(result: RedeemResult): string {
  return result.snapshotPending ? " The card updates at the next quota-cache poll." : ""
}

function providerSentence(result: RedeemResult, provider: string): string {
  const who = opening(nameOf(result.provider || provider))
  const left = leftOf(result)
  const lag = lagOf(result)
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
