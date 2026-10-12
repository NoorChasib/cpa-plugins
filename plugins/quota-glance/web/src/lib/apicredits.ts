// What the Monthly API Credit card says about each Console organization, in
// words.
//
// Every figure on the card is an estimate: Quota Cache prices the tokens CPA
// metered for each organization at Anthropic's list prices, from a Console
// reading entered on its page or from the monthly credit since the last
// refill, and publishes the result. The card says so once ("est." in the
// column heads, ≈ on the pool's figure) and marks a figure that is only a
// bound — some spend may be missing — with ≤ on what is left and ≥ on what is
// used.
//
// Every amount printed is the server's text. Nothing here adds, subtracts or
// rounds money: Quota Cache does that on exact rationals and hands over
// `leftText`, `usedText` and the rest, and a figure re-derived in floating
// point here could read a cent off the one beside it. The only numbers this
// compares are the server's own, against zero or an instant, to pick a word.
//
// Nothing on the card is edited here. Quota Cache's page is where its values
// are entered, and a line that asks for one ends with a link there when this
// browser may follow one (quotaCacheLink.ts); the card draws the link.
//
// Imports nothing but types, so node loads it directly in test/. Durations
// are therefore returned as instants (`since`, `at`) and printed by the card.

import type { FoldFlag } from "./pool"
import type { APICreditAccount, APICreditMeter, APICredits } from "./types"

/**
 * The colour of a mark: a level's ("low", "critical"), a reading that needs a
 * look ("warn"), one that cannot be used until someone acts ("bad"), or a fact
 * with nothing wrong in it ("quiet").
 */
export type CreditTone = "low" | "critical" | "warn" | "bad" | "quiet"

/**
 * One thing worth saying about an account: an optional figure, the word for
 * it, and, for figures that have aged, the instant they were last saved —
 * printed as "2h old" by the card, which owns the clock. An aged mark's word
 * is "" when its age is all there is to say.
 */
export interface CreditMark {
  figure: string
  word: string
  tone: CreditTone
  since?: number
}

/**
 * What a line's link to Quota Cache's page asks for: a Console reading, or
 * the setting the row lacks. "Enter Console reading in Quota Cache" and "Set
 * in Quota Cache".
 */
export type CreditLink = "reading" | "settings"

/**
 * A sentence about an account or the pool. `{age}` stands for how long ago
 * `since` was; `at` is an instant the card prints after the sentence, as a
 * date and time; `link` asks for a link to Quota Cache's page after it.
 */
export interface CreditNote {
  text: string
  tone: "bad" | "warn" | "quiet"
  since?: number
  at?: number
  link?: CreditLink
}

/** A stop shorter than this is a restart or an update, not a gap: client.MeterBriefGap. */
export const BRIEF_GAP_SECONDS = 5 * 60

/** What a row calls its account: the configured label, else quota-cache's id for the item. */
export function creditName(account: Pick<APICreditAccount, "id" | "label">): string {
  return account.label || account.id
}

const has = (account: Pick<APICreditAccount, "dataIssues">, issue: string) => account.dataIssues.includes(issue)

/**
 * How much of its credit an account has left, as a word, or null when that is
 * unremarkable or unknown: "out" once Anthropic refused it for low credit,
 * "past credit" once spend has passed the credit, else the server's level.
 * The figure is the amount left.
 */
export function creditLevel(
  account: Pick<APICreditAccount, "state" | "hasEstimate" | "level" | "leftText" | "overage" | "dataIssues">,
): CreditMark | null {
  if (account.state === "out") return { figure: account.leftText, word: "out", tone: "critical" }
  if (!account.hasEstimate || has(account, "zeroCredit")) return null
  // Said as a word: the figure would be $0.00, and the overage is in the
  // note under the row.
  if (account.overage > 0 && has(account, "overCredit")) return { figure: "", word: "past credit", tone: "low" }
  if (account.level === "critical") return { figure: account.leftText, word: "critical", tone: "critical" }
  if (account.level === "low") return { figure: account.leftText, word: "low", tone: "low" }
  return null
}

/**
 * What the account's state says beside its name, or null when it is counted
 * and complete. Figures that have gone stale carry their age (`since`, the
 * meter's last save); a bound says "incomplete"; one not counted says why in
 * a word or two.
 */
export function creditCondition(
  account: Pick<APICreditAccount, "state" | "lowerBound" | "counted">,
  meter: Pick<APICreditMeter, "updatedAtEpoch"> | null,
): CreditMark | null {
  switch (account.state) {
    case "stale":
      return meter
        ? { figure: "", word: "", tone: "warn", since: meter.updatedAtEpoch }
        : { figure: "", word: "stale", tone: "warn" }
    case "ok":
      return account.lowerBound ? { figure: "", word: "incomplete", tone: "warn" } : null
    case "out":
      // The level already says out, which no missing spend can change.
      return null
    case "needsSettings":
      return { figure: "", word: "not set", tone: "warn" }
    case "pending":
      return { figure: "", word: "not counted yet", tone: "quiet" }
    case "misconfigured":
      return { figure: "", word: "not set up", tone: "warn" }
    case "cacheTooOld":
      // Neutral: the row's own sentence says why, which differs by cause.
      return { figure: "", word: "no figures", tone: "warn" }
    default:
      // A state this bundle does not know. It is not passed off as fine.
      return account.counted ? null : { figure: "", word: "not counted", tone: "quiet" }
  }
}

/**
 * A mark's words, with aged figures' age as the card printed it: "2h old", or
 * the word alone.
 */
export function markText(mark: CreditMark, age: string): string {
  if (mark.since === undefined) return mark.word
  return mark.word ? `${mark.word} · ${age} old` : `${age} old`
}

/**
 * A condition as one word, whoever carries it: its word, or "stale" for a
 * figure that has aged, whose own chip says only how long ago.
 */
export function conditionKey(mark: CreditMark): string {
  return mark.word || (mark.since !== undefined ? "stale" : "")
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
 * label, with what is left and its state in words — "charlie $0.00 out",
 * "bravo 2h old", "echo incomplete". One not counted yet is not named:
 * nothing is wrong with it, and the pool's line already says it is not
 * counted. `age` prints how long ago an instant was.
 *
 * A level is always its account's own chip. A condition two or more
 * accounts share is one chip with a count — "5 incomplete" — after the
 * accounts' own chips, and a level chip whose account is among them no
 * longer repeats it.
 */
export function creditFlags(
  accounts: APICreditAccount[],
  meter: Pick<APICreditMeter, "updatedAtEpoch"> | null,
  age: (since: number) => string,
): FoldFlag[] {
  const loud = (mark: CreditMark | null): mark is CreditMark => mark !== null && mark.tone !== "quiet"
  const rows = accounts.map((account) => ({ account, level: creditLevel(account), condition: creditCondition(account, meter) }))
  const shared = new Map<string, { mark: CreditMark; count: number }>()
  for (const { condition } of rows) {
    if (!loud(condition)) continue
    const key = conditionKey(condition)
    const held = shared.get(key)
    if (held) held.count++
    else shared.set(key, { mark: condition, count: 1 })
  }
  const merged = (mark: CreditMark) => (shared.get(conditionKey(mark))?.count ?? 0) >= 2

  const own = rows.flatMap(({ account, level, condition }): FoldFlag[] => {
    const marks = [level, condition].filter(loud).filter((mark) => mark === level || !merged(mark))
    if (marks.length === 0) return []
    const worst = marks.find((mark) => mark.tone === "critical" || mark.tone === "bad") ?? marks[0]!
    return [
      {
        id: account.id,
        name: creditName(account),
        figure: marks[0]!.figure,
        word: marks
          .map((mark) => markText(mark, mark.since !== undefined ? age(mark.since) : ""))
          .filter(Boolean)
          .join(" · "),
        tone: FLAG_TONE[worst.tone],
      },
    ]
  })
  const counted = [...shared.entries()]
    .filter(([, { count }]) => count >= 2)
    .map(([key, { mark, count }]): FoldFlag => ({
      id: `condition:${key}`,
      name: "",
      figure: String(count),
      word: key,
      tone: FLAG_TONE[mark.tone],
    }))
  return [...own, ...counted]
}

/** What stands in a row's bar column: the bar itself, or the reason there is none. */
export type CreditBar =
  | {
      kind: "bar"
      fraction: number
      level: string
      /** Empty, on a red track: Anthropic refused it, or spend passed the credit. */
      empty: "" | "out" | "over"
    }
  | { kind: "none"; text: string }

/**
 * The bar, or the words in its place on a dashed rule, so an absent figure is
 * never drawn as an empty bar. A settings gap says which value to set.
 */
export function creditBar(
  account: Pick<APICreditAccount, "state" | "hasEstimate" | "remainingFraction" | "level" | "overage" | "dataIssues">,
): CreditBar {
  switch (account.state) {
    case "misconfigured":
      return { kind: "none", text: "configuration needs fixing" }
    case "cacheTooOld":
      return { kind: "none", text: "no figures from Quota Cache" }
    case "pending":
      return { kind: "none", text: "waiting for the meter" }
    case "needsSettings": {
      const credit = has(account, "needsCredit")
      const refill = has(account, "needsRefillDate")
      if (credit && refill) return { kind: "none", text: "set credit and refill date" }
      if (refill) return { kind: "none", text: "set the refill date" }
      return { kind: "none", text: credit ? "set the monthly credit" : "set credit and refill date" }
    }
  }
  if (has(account, "zeroCredit")) return { kind: "none", text: "no credit this month" }
  if (!account.hasEstimate) return { kind: "none", text: "no estimate" }
  const fraction = Math.max(0, Math.min(1, account.remainingFraction))
  if (account.state === "out") return { kind: "bar", fraction: 0, level: "critical", empty: "out" }
  if (account.overage > 0) return { kind: "bar", fraction: 0, level: "critical", empty: "over" }
  return { kind: "bar", fraction, level: account.level, empty: "" }
}

/** What a row prints in its two figure columns. A null figure is a dash, never $0.00. */
export interface CreditFigures {
  left: string | null
  /** "≤" on a bound: there may be less left. */
  leftMark: "" | "≤"
  /** "of $200.00", "" when the credit is not known. */
  of: string
  /** The figure stands alone, with no credit to be a share of: "$106.00 left". */
  leftAlone: boolean
  used: string | null
  /** "≥" on a bound: there may be more used. */
  usedMark: "" | "≥"
  /** "used" of the credit, or "spent" when there is no credit for it to be a share of. */
  usedWord: "used" | "spent"
  /** How far spend passed the credit, "" when it has not. */
  past: string
}

/**
 * The row's figures. Without the card's ≈ — the column heads say "est." once
 * — and with ≤ and ≥ on a bound. An account Anthropic refused prints what it
 * is out of, without either: a refusal is not an estimate.
 */
export function creditFigures(account: APICreditAccount): CreditFigures {
  const none: CreditFigures = {
    left: null,
    leftMark: "",
    of: "",
    leftAlone: false,
    used: null,
    usedMark: "",
    usedWord: "used",
    past: "",
  }
  const bound = account.lowerBound && account.state !== "out"
  const marks = { leftMark: bound ? ("≤" as const) : ("" as const), usedMark: bound ? ("≥" as const) : ("" as const) }
  switch (account.state) {
    case "misconfigured":
    case "cacheTooOld":
    case "pending":
      return none
    case "needsSettings":
      return {
        ...none,
        ...marks,
        left: account.hasEstimate ? account.leftText : null,
        leftAlone: account.hasEstimate && account.monthlyCreditText === "",
        of: account.hasEstimate ? account.monthlyCreditText : "",
        used: account.spentSinceEpoch !== null && account.spentText !== "" ? account.spentText : null,
        usedWord: "spent",
      }
  }
  if (!account.hasEstimate) return none
  if (has(account, "zeroCredit")) {
    return { ...none, ...marks, left: account.leftText, of: account.monthlyCreditText, used: account.spentText, usedWord: "spent" }
  }
  return {
    ...none,
    ...marks,
    left: account.leftText,
    of: account.monthlyCreditText,
    used: account.usedText,
    past: account.overage > 0 ? account.overageText : "",
  }
}

/** Each issue's tone when it is the one the server's sentence is about. */
const ISSUE_TONE: Record<string, CreditNote["tone"]> = {
  misconfigured: "warn",
  cacheTooOld: "warn",
  meterMissing: "quiet",
  orgNotCounted: "quiet",
  needsCredit: "warn",
  needsRefillDate: "warn",
  refused: "bad",
  refusedNearlySpent: "bad",
  stale: "warn",
  meterStartedLate: "warn",
  meterGap: "warn",
  meterDropped: "warn",
  meterFull: "warn",
  meterUnattributed: "warn",
  meterRejected: "warn",
  unpricedModel: "warn",
  zeroCredit: "quiet",
  overCredit: "warn",
  readingAboveCredit: "warn",
  readingOnRefillDay: "quiet",
  readingUnused: "quiet",
  claudeCodeRefused: "warn",
  noTraffic: "quiet",
  configMonthlyUsdInvalid: "warn",
  configRenewsInvalid: "warn",
  adminKeyIgnored: "quiet",
}

/** The issues a Console reading would settle, which earn the row an "Enter Console reading" link. */
const READING_SETTLES = ["meterStartedLate", "meterGap", "readingOnRefillDay", "claudeCodeRefused"]

/**
 * Whether a Console reading would settle what the row says: it is out, it is
 * counted but incomplete for want of one, or it lacks only a refill date,
 * which a reading stands in for. A row that lacks its monthly credit needs
 * that first, whatever else it says.
 */
export function wantsReading(account: Pick<APICreditAccount, "state" | "counted" | "dataIssues">): boolean {
  if (account.state === "out") return true
  if (account.state === "needsSettings") return has(account, "needsRefillDate") && !has(account, "needsCredit")
  return account.counted && READING_SETTLES.some((issue) => has(account, issue))
}

/**
 * Whether Quota Cache's page can edit this row: nothing but the card's own
 * reason keeps it from it. A row no page could edit — no organization, a
 * duplicate, past the sixteenth, a 0.1.13 entry — keeps its reason, and so
 * does every row while Quota Cache cannot use its saved settings.
 */
export function editsInQuotaCache(account: Pick<APICreditAccount, "settings">): boolean {
  return account.settings.notEditableReason === "quotaCache"
}

/**
 * The link a row's line ends with, or none: what the row lacks, for one that
 * needs a setting, else a Console reading, for one a reading would settle.
 * Only for a row Quota Cache's page can edit.
 */
function linkOf(account: APICreditAccount): CreditLink | undefined {
  if (!editsInQuotaCache(account)) return undefined
  if (account.state === "needsSettings") return "settings"
  return wantsReading(account) ? "reading" : undefined
}

/**
 * The server's own sentence about the account, toned by the issue it is
 * about; null when it has none. An account Anthropic refused has the
 * refusal's time after its sentence.
 */
function issueNote(account: APICreditAccount): CreditNote | null {
  if (!account.issue) return null
  const first = account.dataIssues[0] ?? ""
  const note: CreditNote = { text: account.issue, tone: ISSUE_TONE[first] ?? "quiet" }
  if (account.state === "out") {
    const at = has(account, "refusedNearlySpent") ? account.refusals.claudeCodeLastAtEpoch : account.refusals.lastAtEpoch
    if (at !== null) note.at = at
  }
  const link = linkOf(account)
  if (link) note.link = link
  return note
}

/**
 * A sentence two or more rows carry word for word, said once among the
 * pool's lines instead of under each of them.
 */
export interface SharedIssue {
  /** The line as each of its rows would have shown it, without its link. */
  note: CreditNote
  /** The issue the server wrote it for: the first of its rows' issues. */
  issue: string
  /** The accounts that carry it, in the card's order. */
  ids: string[]
  /** The link its line ends with: the first of them a row's line would have ended with, else null. */
  link: CreditLink | null
}

/**
 * The rows' sentences that more than one row carries, grouped by exact
 * string, in the order of the first row to carry each. A refusal's time is
 * part of what its row says, so two refusals group only at the same instant.
 * A sentence one row carries stays on that row, and so does one about an
 * organization Quota Cache has no figures for: it speaks of that
 * organization, and the card without Quota Cache's figures lists each one
 * with its own sentence.
 */
export function sharedIssues(accounts: APICreditAccount[]): SharedIssue[] {
  const groups = new Map<string, SharedIssue>()
  for (const account of accounts) {
    if (account.state === "cacheTooOld") continue
    const note = issueNote(account)
    if (note === null) continue
    const key = `${note.at ?? ""}|${note.text}`
    let group = groups.get(key)
    if (!group) {
      // The sentence alone: its link is the group's, from whichever row offers one.
      const said: CreditNote = { text: note.text, tone: note.tone }
      if (note.at !== undefined) said.at = note.at
      group = { note: said, issue: account.dataIssues[0] ?? "", ids: [], link: null }
      groups.set(key, group)
    }
    group.ids.push(account.id)
    group.link ??= note.link ?? null
  }
  return [...groups.values()].filter((group) => group.ids.length >= 2)
}

/**
 * Who a shared sentence is about, ahead of it: every account on the card,
 * every one the pool counts when some are left out, else their names —
 * "alpha and bravo", "alpha, bravo and charlie".
 */
export function sharedWho(ids: string[], accounts: Pick<APICreditAccount, "id" | "label" | "counted">[]): string {
  const every = (count: number, what: string) => (count === 2 ? `Both ${what}` : `All ${count} ${what}`)
  const carries = new Set(ids)
  if (accounts.length === carries.size && accounts.every((account) => carries.has(account.id))) return every(carries.size, "accounts")
  const counted = accounts.filter((account) => account.counted)
  if (counted.length === carries.size && counted.every((account) => carries.has(account.id))) {
    return every(carries.size, "counted accounts")
  }
  return listNames(accounts.filter((account) => carries.has(account.id)).map(creditName))
}

/**
 * The conditions a row's chip need not repeat: those every counted account
 * carries — or every account, when none is counted — and whose every
 * carrier's sentence the pool says once. The ≤ and ≥ beside each figure,
 * the aged ink, or the words in the bar's place already mark the row; the
 * pool's line says why. A condition only some rows carry keeps its chips, so
 * the reader can see which.
 */
export function conditionsSaidOnce(
  accounts: APICreditAccount[],
  meter: Pick<APICreditMeter, "updatedAtEpoch"> | null,
  shared: SharedIssue[],
): Set<string> {
  const said = new Set(shared.flatMap((group) => group.ids))
  const carriers = new Map<string, string[]>()
  for (const account of accounts) {
    const mark = creditCondition(account, meter)
    if (mark === null) continue
    const key = conditionKey(mark)
    carriers.set(key, [...(carriers.get(key) ?? []), account.id])
  }
  const counted = accounts.filter((account) => account.counted).map((account) => account.id)
  const scope = counted.length > 0 ? counted : accounts.map((account) => account.id)
  const keys = new Set<string>()
  for (const [key, ids] of carriers) {
    const sameRows = ids.length === scope.length && scope.every((id) => ids.includes(id))
    if (ids.length >= 2 && sameRows && ids.every((id) => said.has(id))) keys.add(key)
  }
  return keys
}

/**
 * The lines a row earns under itself: the server's own sentence about the
 * account, toned by the issue it is about, unless the pool says it once for
 * several rows (`shared`); then the cache-write note when the 1-hour rate
 * would add a cent or more.
 */
export function creditNotes(account: APICreditAccount, shared: SharedIssue[] = []): CreditNote[] {
  const notes: CreditNote[] = []
  const note = issueNote(account)
  if (note !== null && !shared.some((group) => group.ids.includes(account.id))) notes.push(note)
  if (account.cacheWriteExtraText) {
    notes.push({
      text: `Cache writes are priced at the 5-minute rate; at the 1-hour rate this would be ${account.cacheWriteExtraText} more.`,
      tone: "quiet",
    })
  }
  return notes
}

/** Where the estimate starts from, for the bar's hover: a Console reading, or the monthly credit. */
export type CreditBasis =
  | { kind: "reading"; remainingText: string; atEpoch: number; spentSinceText: string }
  | { kind: "credit"; monthlyCreditText: string; spentText: string }

export function creditBasis(account: APICreditAccount): CreditBasis | null {
  if (account.basis === "reading" && account.reading) {
    return {
      kind: "reading",
      remainingText: account.reading.remainingText,
      atEpoch: account.reading.atEpoch,
      spentSinceText: account.reading.spentSinceText,
    }
  }
  if (account.basis === "credit" && account.monthlyCreditText) {
    return { kind: "credit", monthlyCreditText: account.monthlyCreditText, spentText: account.spentText }
  }
  return null
}

/**
 * Which of a row's values were entered on Quota Cache's page rather than read
 * from its config: for each, what the config has instead — "" when it has
 * nothing usable — or null for a value that was not entered there.
 */
export function setHere(
  account: Pick<APICreditAccount, "monthlyCreditSource" | "renewsSource" | "settings">,
): { credit: string | null; refill: string | null } {
  const { settings } = account
  return {
    credit:
      account.monthlyCreditSource === "dashboard" ? (settings.configMonthlyUsdInvalid ? "" : settings.configMonthlyUsd) : null,
    refill: account.renewsSource === "dashboard" ? (settings.configRenewsInvalid ? "" : settings.configRenews) : null,
  }
}

/**
 * The hover on each value entered on Quota Cache's page, naming the config's
 * value beside it, so the card still tells the two apart; null for a value
 * that was not. `day` prints a UTC calendar day, as the refill dates beside
 * it are printed.
 */
export function setHereTitles(
  account: Pick<APICreditAccount, "monthlyCreditSource" | "renewsSource" | "settings">,
  day: (epoch: number) => string,
): { credit: string | null; refill: string | null } {
  const config = setHere(account)
  const title = (value: string | null, shown: (value: string) => string) =>
    value === null ? null : value ? `Entered in Quota Cache. Its config says ${shown(value)}.` : "Entered in Quota Cache. Not in its config."
  return {
    credit: title(config.credit, moneyText),
    refill: title(config.refill, (value) => day(dateEpoch(value))),
  }
}

/** What a mark beside a value entered on Quota Cache's page is called, and what the foot's legend says. */
export const ENTERED_HERE = "entered in Quota Cache"

/** A valid amount as the page prints money: `1200.5` is `$1,200.50`. String work only; no float touches it. */
export function moneyText(value: string): string {
  const [whole = "0", cents = ""] = value.split(".")
  return `$${whole.replace(/^0+(?=\d)/, "").replace(/\B(?=(\d{3})+(?!\d))/g, ",")}.${cents.padEnd(2, "0")}`
}

/** 00:00 UTC of a YYYY-MM-DD date, in epoch seconds. */
export function dateEpoch(value: string): number {
  const [year, month, day] = value.split("-").map(Number)
  return Date.UTC(year!, month! - 1, day!) / 1000
}

/** Why an uncounted account is left out of the pool, as the pool's line says it. */
function leftOutReason(account: Pick<APICreditAccount, "state" | "dataIssues">): string {
  switch (account.state) {
    case "needsSettings": {
      const credit = has(account, "needsCredit")
      const refill = has(account, "needsRefillDate")
      if (credit && refill) return "set its monthly credit and refill date"
      if (refill) return "set its refill date"
      return "set its monthly credit"
    }
    case "pending":
      return "it is not counted yet"
    case "misconfigured":
      return "its configuration needs fixing"
    case "cacheTooOld":
      return "Quota Cache has no figures for it"
    default:
      return "it is not counted"
  }
}

/** Names as a sentence lists them: "a", "a and b", "a, b and c". */
function listNames(names: string[]): string {
  return names.length <= 1 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`
}

/** A Console organization id as a line names it: its first 4 and last 4 hex digits, "0000…000f". */
export function shortOrganization(id: string): string {
  const hex = id.replace(/-/g, "")
  return hex.length > 8 ? `${hex.slice(0, 4)}…${hex.slice(-4)}` : hex
}

/**
 * Where an account's estimate counts from: its Console reading, the start of
 * its cycle, else whatever its spend counts from; null when nothing.
 */
export function creditAnchor(
  account: Pick<APICreditAccount, "basis" | "reading" | "cycleStartEpoch" | "spentSinceEpoch">,
): number | null {
  if (account.basis === "reading" && account.reading) return account.reading.atEpoch
  if (account.basis === "credit" && account.cycleStartEpoch !== null) return account.cycleStartEpoch
  return account.spentSinceEpoch
}

/** Whether the meter has been stopped long enough to count as a gap: 5 minutes or more. */
export function meterStopped(meter: Pick<APICreditMeter, "stoppedAtEpoch"> | null, now: number): boolean {
  return meter !== null && meter.stoppedAtEpoch !== null && now - meter.stoppedAtEpoch >= BRIEF_GAP_SECONDS
}

/** The card's source line, after "Estimated from CPA traffic". */
export type MeterLine =
  | { kind: "none" }
  | { kind: "updated"; since: number }
  | { kind: "stale"; since: number }
  | { kind: "stopped"; since: number }

/**
 * How current the meter is: stopped for 5 minutes or more, else not saved for
 * longer than the server allows, else when it last saved. A stop shorter than
 * that is a restart or an update, and says nothing yet.
 */
export function meterLine(meter: APICreditMeter | null, now: number): MeterLine {
  if (meter === null) return { kind: "none" }
  if (meterStopped(meter, now)) return { kind: "stopped", since: meter.stoppedAtEpoch! }
  if (meter.stale) return { kind: "stale", since: meter.updatedAtEpoch }
  return { kind: "updated", since: meter.updatedAtEpoch }
}

/** The most organizations the pool names on lines of their own; the rest are counted. */
const UNLINKED_LINES = 3

/**
 * The lines under the pool's figure when it is not the whole story, in this
 * order: which accounts it leaves out and why; whether the values saved in
 * Quota Glance have reached Quota Cache (`editing.migration`); each sentence
 * several rows share (`shared`), once, after whom it is about; each Console
 * organization CPA sent traffic from that no counted item names; a stretch
 * the meter was not counting; and records it dropped, could not match, or
 * could not read. The last two only when they fall after the oldest anchor
 * the pool counts from, where they can have touched its figures, and not when
 * a shared sentence already says so. Empty when there is nothing to add.
 */
export function poolNotes(
  credits: Pick<APICredits, "pool" | "accounts" | "unlinked" | "meter"> & Partial<Pick<APICredits, "editing">>,
  now: number,
  shared: SharedIssue[] = sharedIssues(credits.accounts),
): CreditNote[] {
  const notes: CreditNote[] = []
  const { pool, accounts, unlinked, meter } = credits

  const out = accounts.filter((account) => !account.counted)
  if (out.length > 0) {
    const clauses = [`counts ${pool.countedCount} of ${pool.accountCount} accounts`]
    if (out.length === 1) clauses.push(`${creditName(out[0]!)} is left out: ${leftOutReason(out[0]!)}`)
    else if (out.length <= 3) clauses.push(`${listNames(out.map(creditName))} are left out`)
    else clauses.push(`${out.length} are left out`)
    notes.push({ text: clauses.join(" · "), tone: "warn" })
  }

  const migration = migrationNote(credits.editing?.migration ?? "")
  if (migration !== null) notes.push(migration)

  for (const group of shared) {
    const note: CreditNote = { text: `${sharedWho(group.ids, accounts)}: ${group.note.text}`, tone: group.note.tone }
    if (group.note.at !== undefined) note.at = group.note.at
    // One link for them all, as each row's was.
    if (group.link !== null) note.link = group.link
    notes.push(note)
  }
  const sharedSays = (issue: string) => shared.some((group) => group.issue === issue)

  for (const org of unlinked.slice(0, UNLINKED_LINES)) {
    const sent = `CPA sent traffic from Console organization ${shortOrganization(org.organizationId)} (${org.requests} ${
      org.requests === 1 ? "request" : "requests"
    }, last {age} ago)`
    notes.push({
      text:
        org.reason === "overLimit"
          ? `${sent}, which is in an item past the first 16 in claude-api-credits, so it is not counted.`
          : `${sent}, which no organization in Quota Cache names. If it is yours, add it on Quota Cache's page.`,
      tone: "warn",
      since: org.lastSeenEpoch,
    })
  }
  if (unlinked.length > UNLINKED_LINES) {
    const more = unlinked.length - UNLINKED_LINES
    notes.push({ text: `and ${more} more ${more === 1 ? "organization" : "organizations"}`, tone: "warn" })
  }

  const anchors = accounts
    .filter((account) => account.counted)
    .map(creditAnchor)
    .filter((anchor): anchor is number => anchor !== null)
  if (meter !== null && anchors.length > 0) {
    const oldest = Math.min(...anchors)
    const after = (epoch: number | null) => epoch !== null && epoch > oldest
    const gap = meter.gaps.some((gap) => gap.toEpoch - gap.fromEpoch >= BRIEF_GAP_SECONDS && gap.toEpoch > oldest)
    if ((gap || meterStopped(meter, now)) && !sharedSays("meterGap")) {
      notes.push({ text: "Quota Cache was not counting for part of this period, so some spend may be missing.", tone: "warn" })
    }
    const lost: [string, string] | null = after(meter.lastDroppedEpoch)
      ? ["meterDropped", "Quota Cache dropped usage records it could not keep up with, so some spend is missing."]
      : after(meter.lastUnattributedEpoch)
        ? ["meterUnattributed", "Some failed requests could not be matched to an organization, so some spend may be missing."]
        : after(meter.lastRejectedEpoch)
          ? ["meterRejected", "Quota Cache could not read some usage records from CPA, so some spend may be missing."]
          : null
    if (lost && !sharedSays(lost[0])) notes.push({ text: lost[1], tone: "warn" })
  }
  return notes
}

/** Whether any figure on the card was entered on Quota Cache's page: the foot then explains the dot. */
export function anySetHere(accounts: APICreditAccount[]): boolean {
  return accounts.some((account) => account.monthlyCreditSource === "dashboard" || account.renewsSource === "dashboard")
}

/**
 * When an account's credit next refills, in the vocabulary the card prints:
 * "refilled today" while the cycle that began at 00:00 UTC today is still its
 * first day, else the calendar days to go, else — inside the last day — the
 * span itself, which the card ticks. Days because the refill is a date the
 * reader set; a refill inside the day is an hour that matters.
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
 * Why Quota Cache's page cannot edit a row, for a reason no page could lift;
 * null for any other, which is the card's (editingOffText).
 */
export function notEditableText(reason: string): string | null {
  switch (reason) {
    case "noOrganization":
      return "Add this organization's organization-id in Quota Cache's config to edit it."
    case "duplicateOrganization":
      return "Another item in Quota Cache's config has the same organization-id; edit that one."
    case "overLimit":
      return "Only the first 16 items in claude-api-credits can be edited."
    case "cacheTooOld":
      return "Update Quota Cache to edit this organization."
    default:
      return null
  }
}

/**
 * Why nothing on the card is edited here, in words; null while it is, which
 * only a plugin older than 0.8.0 says. `quotaCache` is the ordinary case;
 * the other two say Quota Cache cannot use its saved settings, and which file
 * to look at.
 */
export function editingOffText(editing: Pick<APICredits["editing"], "available" | "reason">): string | null {
  if (editing.available) return null
  switch (editing.reason) {
    case "settingsUnreadable":
      return "Quota Cache cannot read its saved settings, so only config values apply. See Quota Cache's page."
    case "legacyUnreadable":
      return "Quota Glance's settings.json could not be read, so Quota Cache imported nothing and only config values apply. Fix it or move it aside."
    default:
      return "Change these on Quota Cache's page in CPA's management console."
  }
}

/**
 * Whether the card has more to say about editing than where it happens: Quota
 * Cache cannot use its saved settings, so the values entered there do not
 * apply. The foot then says why under its own line.
 */
export function editingTrouble(editing: Pick<APICredits["editing"], "available" | "reason">): string | null {
  return editing.available || editing.reason === "quotaCache" ? null : editingOffText(editing)
}

/** The line for `editing.migration`: whether the values saved in Quota Glance have reached Quota Cache. */
export function migrationText(migration: string): string | null {
  switch (migration) {
    case "notImported":
      return "Quota Cache has not imported the settings saved in Quota Glance. Import them on Quota Cache's page; its README says what to do if the page does not offer it."
    case "changedSinceImport":
      return "Quota Glance's saved settings changed after Quota Cache imported them; open Quota Cache's page."
    case "awaitingImport":
      return "Your monthly credits, refill dates, Console readings and renewal dates saved in Quota Glance are kept, and Quota Cache 0.2.0 imports them when it starts."
    default:
      return null
  }
}

/**
 * The migration line among the pool's: amber while values saved in Quota
 * Glance are not in Quota Cache, quiet while they are kept for its first
 * start.
 */
function migrationNote(migration: string): CreditNote | null {
  const text = migrationText(migration)
  if (text === null) return null
  return { text, tone: migration === "awaitingImport" ? "quiet" : "warn" }
}
