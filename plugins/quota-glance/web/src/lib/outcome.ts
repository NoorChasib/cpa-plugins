// How a press that the plugin answered is told to the reader: which outcomes
// fell short, which spent a reset with CPA possibly still skipping the
// account, and the sentence for each — and, since Claude's banked resets
// come in kinds, the words for each kind wherever the page names one: its
// group on the card, its button, the dialog that asks, and what it came to.
// One vocabulary for all of them, so the card can never call a reset one
// thing and the outcome line another.
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
  /**
   * The plugin asked Quota Cache to poll this account now, then every other
   * one, so the card catches up within a minute rather than at its next poll.
   * False when it could not ask, when no Quota Cache that acts on the request
   * is running, when Quota Cache cannot poll the account within about a
   * minute, after an outcome that spent nothing, and from a plugin too old to
   * ask.
   */
  refreshRequested: boolean
  /**
   * When the plugin asked, in Unix seconds by CPA's clock; null whenever
   * refreshRequested is false. What the page's check waits for (checks.ts).
   */
  refreshRequestedAtEpoch: number | null
  /** "codex" or "claude"; empty from a plugin too old to say. */
  provider: string
  /**
   * The plugin answered from its press ledger: this is what an earlier copy of
   * the same press came to, not something this request did.
   */
  replayed: boolean
  /**
   * The kind of Claude reset this press spent, or tried to — "full", "fable",
   * "weekly" or "session" — as the plugin names it. Null for Codex, whose
   * resets are all one sort, and from a plugin older than the kinds, which
   * spent whichever grant it chose; the sentence then names no kind rather
   * than guessing one.
   */
  kind: string | null
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
 * Everything the page says about one kind of Claude banked reset.
 *
 * The meanings are Claude Code's, and go no further: it says a Fable reset
 * "returns what Fable used to your weekly limit", and so does this, without
 * claiming either way what it does to the 5-hour limit, which Claude Code
 * does not say. "Not a full reset" is said outright on Fable's line because
 * that is the confusion this exists to prevent.
 */
export interface KindWords {
  /** In a sentence: "full reset", "Fable reset". */
  one: string
  /** Its plural: "full resets". */
  many: string
  /** Its group's heading on the card: "Full resets". */
  heading: string
  /** What spending one does, in one line, under the heading and in the dialog. */
  effect: string
  /** Its button, and the dialog's: "Use full reset". Empty for a kind never spent here. */
  button: string
  /**
   * What a spend of it came to, in Claude Code's own done line: "Limits
   * reset", "Weekly limits reset" — prefixed with the kind, so the outcome
   * names it.
   */
  done: string
}

/** The four kinds a press can name. Keys match resetKinds.ts SPENDABLE_KINDS; a test holds them together. */
export const KIND_WORDS: Readonly<Record<string, KindWords>> = {
  full: {
    one: "full reset",
    many: "full resets",
    heading: "Full resets",
    effect: "Clears the 5-hour and weekly limits.",
    button: "Use full reset",
    done: "Full reset used — limits reset.",
  },
  fable: {
    one: "Fable reset",
    many: "Fable resets",
    heading: "Fable resets",
    effect: "Gives back to the weekly limit what Fable used this week. Not a full reset.",
    button: "Use Fable reset",
    done: "Fable reset used — what Fable used is back in the weekly limit.",
  },
  weekly: {
    one: "weekly reset",
    many: "weekly resets",
    heading: "Weekly resets",
    effect: "Clears the weekly limits, not the 5-hour limit.",
    button: "Use weekly reset",
    done: "Weekly reset used — weekly limits reset.",
  },
  session: {
    one: "5-hour reset",
    many: "5-hour resets",
    heading: "5-hour resets",
    effect: "Clears the 5-hour limit, not the weekly limits.",
    button: "Use 5-hour reset",
    done: "5-hour reset used — 5-hour limit reset.",
  },
}

/**
 * A kind this bundle cannot name — "unknown", or one newer than it. Counted
 * and shown, never offered: there is no button to name, and the way to spend
 * one is the tool that knows what it is.
 */
export const UNRECOGNISED_WORDS: KindWords = {
  one: "unrecognised reset",
  many: "unrecognised resets",
  heading: "Unrecognised resets",
  effect: "A kind of reset this version doesn’t recognise — use it from Claude Code with /limit-reset.",
  button: "",
  done: "Reset used.",
}

/** The words for a kind, any kind: its own, or the unrecognised kind's. */
export function kindWords(kind: string): KindWords {
  return KIND_WORDS[kind] ?? UNRECOGNISED_WORDS
}

/** The words for a kind a press can name, or null for "" (Codex) and anything unrecognised. */
function namedKind(kind: string | null | undefined): KindWords | null {
  return (kind && KIND_WORDS[kind]) || null
}

/**
 * The button a sentence tells the reader to press, or not to: "Use one" on
 * Codex, as it always was, and the kind's own button, quoted, on Claude — so
 * "press it again" can never be read as the button of the other kind.
 */
export function pressName(kind: string | null | undefined): string {
  const words = namedKind(kind)
  return words ? `“${words.button}”` : "Use one"
}

/** "1 full reset", "2 Fable resets", "No Fable resets". */
export function countOf(count: number, kind: string, none = "No"): string {
  const words = kindWords(kind)
  if (count === 0) return `${none} ${words.many}`
  return `${count} ${count === 1 ? words.one : words.many}`
}

/**
 * What Claude asks of an account before it lets a reset be spent, in the
 * dialog that asks. Claude Code's "usable once you reach a limit it refills",
 * and the cooldown between resets that a refusal may cite.
 */
export const CLAUDE_RULE =
  "Claude allows a reset only once the account has reached a limit it refills, and makes it wait out a cooldown between resets."

/**
 * Said in place of a button on a Claude account whose kinds Quota Cache did
 * not report. The plugin will not spend one either: a press has to name a
 * kind, and nothing here can say which these are.
 */
export const KINDS_UNREPORTED =
  "Update Quota Cache to 0.5.0 or newer to see which kind each reset is — this page won’t spend one until it can tell full and Fable resets apart."

/**
 * The dialog's opening, for one kind: what it spends, from whom. A count of
 * one is "the only" one, as on Codex.
 */
export function spendsSentence(kind: string, count: number, who: string): string {
  const words = kindWords(kind)
  const which = count === 1 ? `the only ${words.one}` : `one of the ${count} ${words.many}`
  return `This spends ${which} that ${who} holds, straight away.`
}

/**
 * What the dialog says of the account's other kinds, which the press leaves
 * alone: "Its 1 full reset stays banked — this is not one of them." Empty
 * when it holds no other kind. Said because the reader who means a full reset
 * and opened the Fable dialog by mistake has exactly this line to catch it.
 */
export function othersSentence(others: readonly { kind: string; count: number }[]): string {
  const kept = others.filter((other) => other.count > 0)
  if (kept.length === 0) return ""
  const parts = kept.map((other) => countOf(other.count, other.kind))
  const listed = parts.length === 1 ? parts[0]! : `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]!}`
  const verb = kept.length === 1 && kept[0]!.count === 1 ? "stays" : "stay"
  return `Its ${listed} ${verb} banked — this is not one of them.`
}

/**
 * The codes kindRefusal words. Each is the plugin's answer before it sent
 * anything, and each sentence says "Nothing was spent", so a notice for one
 * takes a refusal's tone, never the unknown's (redeem.ts mayHaveSpent).
 *
 * That includes other_kind_unresolved, though it is about a claim whose
 * outcome is unknown. That claim is the earlier press's, of another kind, and
 * the notice for this press carries the kind just pressed: marked unknown, it
 * would say that kind may be spent, when it is the one certainly still banked.
 * The sentence names the earlier kind, and that press's own notice carries
 * the unknown.
 */
export const KIND_REFUSALS: ReadonlySet<string> = new Set(["kind_required", "invalid_kind", "other_kind_unresolved"])

/**
 * The plugin's three refusals about a kind, each of which spent nothing:
 * the press named none on Claude, named one the plugin cannot spend, or met
 * an earlier press of another kind on the same account whose outcome is still
 * unknown. Null for any other code.
 *
 * The last is the one that matters. The plugin will not start a claim of one
 * kind while a claim of another may still be landing, and the way out is the
 * earlier kind's own button, which repeats that claim rather than making a
 * new one — or the end of its retry window. `clock` prints that instant as a
 * time of day (time.ts formatClock), passed in so this file stays import-free.
 */
export function kindRefusal(
  code: string,
  detail: {
    kind: string
    pendingKind: string | null
    retryUntilEpoch: number | null
    nowSeconds: number
    clock: (epoch: number) => string
  },
): string | null {
  switch (code) {
    case "kind_required":
      return "The plugin needs to be told which kind of reset to use, and this press did not say. Nothing was spent. Reload the page, then press the button for the kind you mean."
    case "invalid_kind":
      return "The plugin cannot spend the kind of reset this press named on this account. Nothing was spent. Reload the page; if this persists, the page and the plugin may be from different versions."
    case "other_kind_unresolved": {
      const pending = namedKind(detail.pendingKind)
      const pressed = namedKind(detail.kind)
      const using = pressed ? `using a ${pressed.one}` : "using another kind"
      const until = detail.retryUntilEpoch
      const wait = until !== null && until > detail.nowSeconds ? `, or wait until ${detail.clock(until)}` : ""
      if (!pending) {
        return `An earlier reset of another kind on this account has an unknown outcome. Check this account’s usage before ${using}${wait}. Nothing was spent.`
      }
      return `An earlier ${pending.one} on this account has an unknown outcome. Press ${pressName(detail.pendingKind)} again to settle it before ${using}${wait}. Nothing was spent.`
    }
    default:
      return null
  }
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
  const reset = namedKind(result.kind)?.one ?? "reset"
  return `${who} took the ${reset} but did not confirm what it cleared, so CPA’s cooldown on this credential was left as it was. ${leftOf(result)}${lagOf(result)} The reset is spent: don’t press ${pressName(result.kind)} again. If CPA keeps skipping this account, use Clear cooldown on it in the CPA console.`
}

/**
 * What became of CPA's cooldown on the credential, after the provider's own
 * sentence, which has already said the reset is spent. Every case that leaves
 * the account possibly still skipped points at the one remedy that costs
 * nothing, CPA's Clear cooldown, rather than at another press, which would
 * spend another — of the same kind, on Claude, which the remedy says.
 */
function cooldownSentence(result: RedeemResult): string {
  const another = namedKind(result.kind)?.one ?? "reset"
  const remedy = `CPA may keep skipping this account until that cooldown ends: use Clear cooldown on this credential in the CPA console. Don’t press ${pressName(result.kind)} again — that would spend another ${another}.`
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

/**
 * What the account still holds, as a sentence: of the kind spent, on Claude,
 * because that is all the plugin counts there — "1 full reset left" — and of
 * every reset, as before, on Codex.
 */
function leftOf(result: RedeemResult): string {
  if (namedKind(result.kind)) return `${countOf(result.remainingCount, result.kind!)} left.`
  return result.remainingCount === 0
    ? "No banked resets left."
    : `${result.remainingCount} banked reset${result.remainingCount === 1 ? "" : "s"} left.`
}

/**
 * Why the card's count has not moved yet, when it has not, and how soon it
 * will.
 *
 * A replayed answer is from a press that may be minutes old, whose request
 * Quota Cache may long since have acted on, so it says the request was made
 * then rather than that the poll is happening now. The account's rows say
 * whether it still is.
 */
function lagOf(result: RedeemResult): string {
  if (result.refreshRequested) return ` ${result.replayed ? REFRESH_ASKED : REFRESHING}`
  return result.snapshotPending ? " The card updates at the next quota-cache poll." : ""
}

/** What the page says while Quota Cache polls the account it was asked to. */
const REFRESHING = "Quota Cache is checking this account now, then every other one. The card updates within a minute."

/** The same, for an answer to an earlier press. */
const REFRESH_ASKED = "Quota Cache was asked then to check this account, then every other one."

function providerSentence(result: RedeemResult, provider: string): string {
  const who = opening(nameOf(result.provider || provider))
  const left = leftOf(result)
  const lag = lagOf(result)
  // On Claude every sentence names the kind the plugin says it spent or
  // tried to, in the card's own words, so the line under the Fable group
  // cannot be read as news of a full reset. Codex, and a plugin too old to
  // name one, keep the sentences they always had.
  const kind = namedKind(result.kind)
  const reset = kind?.one ?? "reset"
  switch (result.outcome) {
    case "reset":
      return `${kind ? kind.done : "Reset applied."} ${left}${lag}`
    case "nothingToReset":
      // The reset is gone either way. Saying otherwise would be a lie the
      // reader acts on when they check their windows.
      return kind
        ? `The ${reset} was spent, but no limit needed clearing. ${left}${lag}`
        : `The reset was spent, but no window needed clearing. ${left}${lag}`
    case "noCredit":
      // Usually a count that was spent elsewhere since the poll; on Claude it is
      // also a grant the provider will not let be used, without saying why.
      return `${who} reported no ${reset} this account can spend right now — the count on this card may be out of date. Nothing was spent.`
    case "alreadyUsed":
      // Most often the answer to a second press after an unknown outcome: the
      // claim being replayed is one the provider had already honoured. Either
      // way this press spent nothing, and that is the half the reader needs.
      return `${who} reports that ${reset} as already used, so this press spent nothing. If an earlier press did not finish, it went through. ${left}${lag}`
    // The four refusals. The provider declined before spending anything, and
    // each says what would have to change for the next press to work.
    case "notLimited":
      // Claude Code's rule, "usable once you reach a limit it refills", which
      // is per kind: a weekly reset waits for a weekly limit.
      return kind
        ? `${who} allows a ${reset} only once the account has reached a limit it refills, and this one has not. Nothing was spent.`
        : `${who} allows a reset only once the account has hit a usage limit, and this one has not. Nothing was spent.`
    case "cooldown":
      return `${who} has this account in a cooldown after its last reset. Nothing was spent — try again once the cooldown ends.`
    case "paused":
      return kind
        ? `${who} has paused this account’s ${kind.many}. Nothing was spent.`
        : `${who} has paused resets on this account. Nothing was spent.`
    case "ineligible":
      return `${who} says this account is not eligible to use a reset right now. Nothing was spent.`
    case "failed":
      // Deliberately not "nothing was spent": this is the plugin's word for a
      // refusal it could not classify, and it does not promise that.
      return `${who} did not apply the ${reset}. Check this account’s usage before pressing again.`
    default:
      return `Done. ${left}${lag}`
  }
}
