// What the Monthly API Credit card says about each Console organization, in
// words.
//
// Every amount printed is the server's text. Nothing here adds, subtracts or
// rounds money: the contract does that on exact rationals and hands over
// `leftText`, `overageText` and the rest, and a figure re-derived in floating
// point here could read a cent off the one beside it. The only numbers this
// compares are the server's own, against zero, to pick a word.
//
// Imports nothing but types, so node loads it directly in test/. Durations
// are therefore returned as instants (`since`) and printed by the card.

import type { FoldFlag } from "./pool"
import type { APICreditAccount, APICredits } from "./types"

/**
 * The colour of a mark: a level's ("low", "critical"), a reading that needs a
 * look ("warn"), one that cannot be used until someone acts ("bad"), or a fact
 * with nothing wrong in it ("quiet").
 */
export type CreditTone = "low" | "critical" | "warn" | "bad" | "quiet"

/**
 * One thing worth saying about an account: an optional figure, the word for
 * it, and, for a reading that has aged, the instant it was taken — printed as
 * "2h 14m old" by the card, which owns the clock. An aged mark's word is ""
 * when its age is all there is to say.
 */
export interface CreditMark {
  figure: string
  word: string
  tone: CreditTone
  since?: number
}

/** A sentence about an account or the pool. `{age}` stands for how long ago `since` was. */
export interface CreditNote {
  text: string
  tone: "bad" | "warn" | "quiet"
  since?: number
}

/** What a row calls its account: the configured label, else quota-cache's id for the item. */
export function creditName(account: Pick<APICreditAccount, "id" | "label">): string {
  return account.label || account.id
}

/**
 * How much of its credit an account has left, as a word, or null when that is
 * unremarkable or unknown: "over" once spend has passed the credit, "out" at
 * nothing left, else the server's level. The figure is the amount that word
 * is about — the overage for "over", what is left otherwise.
 */
export function creditLevel(
  account: Pick<APICreditAccount, "hasReading" | "level" | "left" | "leftText" | "overage" | "overageText">,
): CreditMark | null {
  if (!account.hasReading) return null
  if (account.overage > 0) return { figure: account.overageText, word: "over", tone: "critical" }
  if (account.level === "critical") {
    return { figure: account.leftText, word: account.left > 0 ? "critical" : "out", tone: "critical" }
  }
  if (account.level === "low") return { figure: account.leftText, word: "low", tone: "low" }
  return null
}

/** Why a read failed, from the issue the server attached. */
const READ_FAILURES: [issue: string, word: string, tone: CreditTone][] = [
  ["keyRejected", "key rejected", "bad"],
  ["keyForbidden", "key not allowed", "bad"],
  ["costReportUnavailable", "no cost report", "bad"],
  ["rateLimited", "rate limited", "warn"],
  ["unsupportedCurrency", "not in USD", "bad"],
  ["amountInvalid", "unreadable report", "bad"],
]

function readFailure(dataIssues: string[]): { word: string; tone: CreditTone } | null {
  const known = READ_FAILURES.find(([issue]) => dataIssues.includes(issue))
  return known ? { word: known[1], tone: known[2] } : null
}

/**
 * What the account's state says beside its name, or null when the reading is
 * current and counted. A reading that is counted but old carries its age
 * (`since`); one that is not counted says why in two words.
 *
 * A read that failed for a reason only someone can fix — a rejected key, one
 * that may not read the report, an organization with no report — is said as
 * that even while the last good reading is still counted: it will not come
 * right on the next poll, and "40m old" alone reads as a wait.
 */
export function creditCondition(
  account: Pick<APICreditAccount, "hasReading" | "state" | "dataIssues" | "observedAtEpoch">,
): CreditMark | null {
  const aged = account.hasReading && (account.state === "stale" || account.state === "error")
  if (aged) {
    const failure = account.state === "error" ? readFailure(account.dataIssues) : null
    const refused = failure?.tone === "bad" ? failure : null
    if (account.observedAtEpoch !== null) {
      return refused
        ? { figure: "", word: refused.word, tone: "bad", since: account.observedAtEpoch }
        : { figure: "", word: "", tone: "warn", since: account.observedAtEpoch }
    }
    if (refused) return { figure: "", word: refused.word, tone: "bad" }
    return { figure: "", word: account.state === "error" ? "read failed" : "stale", tone: "warn" }
  }
  switch (account.state) {
    case "ok":
    case "stale":
      return null
    case "misconfigured":
      return { figure: "", word: "not set up", tone: "warn" }
    case "duplicate":
      return { figure: "", word: "duplicate", tone: "quiet" }
    case "pending":
      return { figure: "", word: "no reading yet", tone: "quiet" }
    case "error": {
      const failure = readFailure(account.dataIssues)
      return failure ? { figure: "", ...failure } : { figure: "", word: "read failed", tone: "bad" }
    }
    default:
      // A state this bundle does not know. It is not counted as fine.
      return account.hasReading ? null : { figure: "", word: "no reading", tone: "quiet" }
  }
}

/**
 * A mark's words, with an aged reading's age as the card printed it: "40m
 * old", "key rejected · 40m old", or the word alone.
 */
export function markText(mark: CreditMark, age: string): string {
  if (mark.since === undefined) return mark.word
  return mark.word ? `${mark.word} · ${age} old` : `${age} old`
}

/** A mark's tone, as a shut fold's chip can colour it. */
const FLAG_TONE: Record<CreditTone, FoldFlag["tone"]> = {
  low: "low",
  warn: "low",
  critical: "critical",
  bad: "critical",
  quiet: "",
}

/**
 * The shut card's flags: each organization worth opening the card for, by
 * label, with what is left or past and its state in words — "noor $0.00
 * out", "chasibnoor $7.00 critical · 40m old". A duplicate or one not read
 * yet is not named: nothing is wrong with either, and the pool's line already
 * says it is not counted. `age` prints how long ago an instant was.
 */
export function creditFlags(accounts: APICreditAccount[], age: (since: number) => string): FoldFlag[] {
  return accounts.flatMap((account) => {
    const marks = [creditLevel(account), creditCondition(account)].filter(
      (mark): mark is CreditMark => mark !== null && mark.tone !== "quiet",
    )
    if (marks.length === 0) return []
    const worst = marks.find((mark) => mark.tone === "critical" || mark.tone === "bad") ?? marks[0]!
    return [
      {
        id: account.id,
        name: creditName(account),
        figure: marks[0]!.figure,
        word: marks.map((mark) => markText(mark, mark.since !== undefined ? age(mark.since) : "")).join(" · "),
        tone: FLAG_TONE[worst.tone],
      },
    ]
  })
}

/**
 * What stands in the bar's place on a row with no reading: the state in a few
 * words on a dashed rule, so an absent figure is never drawn as an empty bar.
 * A misconfigured row says which value to fix where the document shows it —
 * the credit text or the refill instant it could not make — beside the chip
 * that already says "not set up".
 */
export function creditNoBar(account: Pick<APICreditAccount, "state" | "monthlyCreditText" | "renewsAtEpoch">): string {
  switch (account.state) {
    case "misconfigured": {
      const credit = account.monthlyCreditText === ""
      const renews = account.renewsAtEpoch === null
      if (credit && renews) return "credit and refill date need fixing"
      if (credit) return "monthly credit needs fixing"
      if (renews) return "refill date needs fixing"
      return "configuration needs fixing"
    }
    case "duplicate":
      return "counted under another account"
    case "pending":
      return "waiting for a reading"
    default:
      return "no reading"
  }
}

/**
 * The lines a row earns under itself when something about it needs saying:
 * spend past the credit, and the server's own sentence about the account,
 * with the age of a reading that has gone old. Empty for a healthy account.
 */
export function creditNotes(
  account: Pick<
    APICreditAccount,
    "hasReading" | "overage" | "overageText" | "monthlyCreditText" | "issue" | "state" | "dataIssues" | "observedAtEpoch"
  >,
): CreditNote[] {
  const notes: CreditNote[] = []
  if (account.hasReading && account.overage > 0) {
    notes.push({
      text: `${account.overageText} spent past its ${account.monthlyCreditText} monthly credit.`,
      tone: "bad",
    })
  }
  if (account.issue) {
    const aged =
      account.hasReading && (account.state === "stale" || account.state === "error") && account.observedAtEpoch !== null
    // A failure someone has to fix is red with or without a reading; a wait,
    // or a failure the next poll may clear while a reading stands, is amber.
    const failure = account.state === "error" ? readFailure(account.dataIssues) : null
    const tone: CreditNote["tone"] =
      account.state === "error"
        ? failure
          ? failure.tone === "bad"
            ? "bad"
            : "warn"
          : account.hasReading
            ? "warn"
            : "bad"
        : account.state === "misconfigured" || account.state === "stale"
          ? "warn"
          : "quiet"
    notes.push(
      aged
        ? { text: `${account.issue} These figures are from {age} ago.`, tone, since: account.observedAtEpoch! }
        : { text: account.issue, tone },
    )
  }
  return notes
}

/** Why a read failed, as the pool's line says it, or null when the server did not say. */
function failureReason(dataIssues: string[]): string | null {
  if (dataIssues.includes("keyRejected")) return "its admin key was rejected"
  if (dataIssues.includes("keyForbidden")) return "its admin key cannot read the cost report"
  if (dataIssues.includes("costReportUnavailable")) return "Anthropic has no cost report for it"
  if (dataIssues.includes("rateLimited")) return "Anthropic is rate limiting its reads"
  return null
}

/** Why an uncounted account is left out of the pool, as the pool's line says it. */
function leftOutReason(account: Pick<APICreditAccount, "state" | "dataIssues">): string {
  switch (account.state) {
    case "misconfigured":
      return "its configuration needs fixing"
    case "pending":
      return "it has no reading yet"
    case "error":
      return failureReason(account.dataIssues) ?? "its report could not be read"
    default:
      return "it has no current reading"
  }
}

/** Names as a sentence lists them: "a", "a and b", "a, b and c". */
function listNames(names: string[]): string {
  return names.length <= 1 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`
}

/**
 * The lines under the pool's figure when it is not the whole story: which
 * accounts it leaves out and why, which it does not count twice, and which it
 * counts on a reading that has gone old. Empty when every account is counted
 * on a current reading.
 *
 * A duplicate is not left out — its organization is counted, under the
 * account listed first — so it is said apart from those that are, as the
 * contract keeps `missingCount` and `duplicateCount` apart.
 */
export function poolNotes(credits: Pick<APICredits, "pool" | "accounts">): CreditNote[] {
  const notes: CreditNote[] = []
  const { pool, accounts } = credits
  const out = accounts.filter((account) => !account.hasReading && account.state !== "duplicate")
  const twice = accounts.filter((account) => account.state === "duplicate")
  if (out.length > 0 || twice.length > 0) {
    const clauses = [`counts ${pool.countedCount} of ${pool.accountCount} accounts`]
    if (out.length === 1) clauses.push(`${creditName(out[0]!)} is left out: ${leftOutReason(out[0]!)}`)
    else if (out.length > 1 && out.length <= 3) clauses.push(`${listNames(out.map(creditName))} are left out`)
    else if (out.length > 3) clauses.push(`${out.length} are left out`)
    if (twice.length === 1) clauses.push(`${creditName(twice[0]!)} is a duplicate, counted under another`)
    else if (twice.length > 1) clauses.push(`${twice.length} duplicates are counted under others`)
    notes.push({
      text: clauses.join(" · "),
      tone: out.some((account) => account.state === "error") ? "bad" : out.length > 0 ? "warn" : "quiet",
    })
  }
  const aged = accounts.filter(
    (account) => account.hasReading && (account.state === "stale" || account.state === "error"),
  )
  if (aged.length === 1) {
    const account = aged[0]!
    const reason = account.state === "error" ? failureReason(account.dataIssues) : null
    const failed = account.state === "error" ? (reason ? `; ${reason}` : ", its latest read failed") : ""
    // Red when the reason is one nobody but the reader can fix.
    const tone = readFailure(account.dataIssues)?.tone === "bad" && account.state === "error" ? "bad" : "warn"
    notes.push(
      account.observedAtEpoch !== null
        ? { text: `${creditName(account)}'s figures are from {age} ago${failed}`, tone, since: account.observedAtEpoch }
        : { text: `${creditName(account)}'s figures are out of date${failed}`, tone },
    )
  } else if (aged.length > 1) {
    // Name only the accounts that will not come right on the next poll; the
    // rest are a wait, and the count says that.
    const refused = aged.filter(
      (account) => account.state === "error" && readFailure(account.dataIssues)?.tone === "bad",
    )
    const clauses = [`${aged.length} accounts' figures are out of date`]
    if (refused.length === 1) {
      const reason = failureReason(refused[0]!.dataIssues)
      clauses.push(`${creditName(refused[0]!)} needs attention${reason ? `: ${reason}` : ""}`)
    } else if (refused.length > 1) {
      clauses.push(`${listNames(refused.map(creditName))} need attention`)
    }
    notes.push({ text: clauses.join(" · "), tone: refused.length > 0 ? "bad" : "warn" })
  }
  return notes
}

/**
 * When an account's credit next refills, in the vocabulary the card prints:
 * "refilled today" while the cycle that began at 00:00 UTC today is still its
 * first day, else the calendar days to go, else — inside the last day — the
 * span itself, which the card ticks. Days because the refill is a date the
 * reader configured; a refill inside the day is an hour that matters.
 *
 * Calendar days, counted between UTC dates, so the count agrees with the
 * date printed beside it: on Oct 9 a refill on Oct 28 is "in 19d", however
 * far into the 9th it is. Elapsed time rounded down would say 18 for most of
 * the day.
 */
export type RefillWhen =
  | { kind: "none" }
  | { kind: "refilledToday" }
  | { kind: "due" }
  | { kind: "within"; seconds: number }
  | { kind: "days"; days: number }

const DAY = 86400

export function refillWhen(renewsAtEpoch: number | null, cycleStartEpoch: number | null, now: number): RefillWhen {
  if (cycleStartEpoch !== null && now >= cycleStartEpoch && now - cycleStartEpoch < DAY) return { kind: "refilledToday" }
  if (renewsAtEpoch === null) return { kind: "none" }
  const remaining = renewsAtEpoch - now
  if (remaining <= 0) return { kind: "due" }
  if (remaining < DAY) return { kind: "within", seconds: remaining }
  return { kind: "days", days: Math.floor(renewsAtEpoch / DAY) - Math.floor(now / DAY) }
}

/**
 * When the figures were last read: the newest reading among the accounts the
 * pool counts, or null when it counts none.
 */
export function lastRead(accounts: Pick<APICreditAccount, "hasReading" | "observedAtEpoch">[]): number | null {
  let newest: number | null = null
  for (const account of accounts) {
    if (!account.hasReading || account.observedAtEpoch === null) continue
    if (newest === null || account.observedAtEpoch > newest) newest = account.observedAtEpoch
  }
  return newest
}
