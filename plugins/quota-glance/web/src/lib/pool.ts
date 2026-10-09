// Naming the accounts around a pooled bar.
//
// Nothing here takes a share of anything. Every width the bar draws is a
// field the server wrote — the aggregate's fractions — and this module only
// decides what to call each account in a sentence or on a tile, and what to
// say when the pool counts an account as less than its own reading.

import type { Aggregate, Credential, RowEntry } from "./types"

/** The part of an address before the "@", which is how a sentence names an account. */
export function shortName(credential: Credential | undefined, fallback: string): string {
  const address = credential?.email || fallback
  const at = address.indexOf("@")
  return at > 0 ? address.slice(0, at) : address
}

/**
 * An account's name in two parts: the local part, and what tells it apart
 * from another account with the same one — "" when nothing has to.
 *
 * Two parts rather than one string so a tile too narrow for both can shorten
 * the local part and keep the qualifier, which is the half that differs.
 */
export type AccountName = { local: string; qualifier: string }

/** The name as one string, for a legend or a sentence. */
export const nameText = (name: AccountName): string => name.local + name.qualifier

/**
 * What each of a provider's accounts is called on its tiles, in its legends
 * and in its recovery lines: the local part, unless another account of the
 * same provider shares it. Then the domain's first label is added —
 * "noor@gmail", "noor@agency" — or the whole domain where even that is shared.
 *
 * Per provider, because that is the set a reader has to tell apart: a tile is
 * the way into an irreversible spend, and "noor · 1 reset" beside another
 * "noor · 1 reset" leaves the choice to a hover the menu bar's popover and a
 * phone do not have. Computed over every credential of the provider, not one
 * card's entries, so an account is called the same thing everywhere it shows.
 */
export function accountNames(credentials: Credential[]): Map<string, AccountName> {
  const split = credentials.map((credential) => {
    const address = credential.email || credential.id
    const at = address.indexOf("@")
    const local = at > 0 ? address.slice(0, at) : address
    const domain = at > 0 ? address.slice(at + 1) : ""
    return { id: credential.id, local, domain, label: domain.split(".")[0] ?? "" }
  })
  const count = (values: string[]) => {
    const seen = new Map<string, number>()
    for (const value of values) seen.set(value, (seen.get(value) ?? 0) + 1)
    return seen
  }
  const locals = count(split.map((item) => item.local))
  const labelled = count(split.map((item) => `${item.local}@${item.label}`))
  return new Map(
    split.map((item) => {
      if ((locals.get(item.local) ?? 0) < 2 || item.domain === "") {
        return [item.id, { local: item.local, qualifier: "" }]
      }
      const short = (labelled.get(`${item.local}@${item.label}`) ?? 0) < 2
      return [item.id, { local: item.local, qualifier: `@${short ? item.label : item.domain}` }]
    }),
  )
}

/**
 * Who the next recovery is, as a sentence names them: one account by name, two
 * joined, more by count. From `resetsNext`, never from the server's subtext,
 * which is a sentence for printing whole.
 */
export function recoveryNames(names: string[]): { who: string; plural: boolean } | null {
  if (names.length === 0) return null
  if (names.length === 1) return { who: names[0]!, plural: false }
  if (names.length === 2) return { who: `${names[0]} and ${names[1]}`, plural: true }
  return { who: `${names.length} accounts`, plural: true }
}

/**
 * Why the pool counts an account as less than its own figure, in a word or
 * two for a chip beside its address. `spent` when what it counts as is
 * nothing at all, which is what mutes its bar.
 */
export type WeeklyNote = { text: string; spent: boolean }

/**
 * What an account's row says about its weekly limit, or null when the pool
 * counts it at its own reading.
 *
 * On Claude's session row an account whose weekly is spent is held out of the
 * mean; on its Fable row an account counts as no more than its weekly has
 * left. Either way the row keeps the account's real figure, because that is
 * what an expanded card is for, and this says why the pool above disagrees.
 * Read from the server's fields alone, so a document from a plugin that
 * predates them has no note on any row.
 */
export function weeklyNote(entry: Pick<RowEntry, "remainingPercent" | "heldOut" | "pooledPercent">): WeeklyNote | null {
  if (entry.heldOut === true) return { text: "weekly spent", spent: true }
  if (typeof entry.pooledPercent === "number" && entry.pooledPercent < entry.remainingPercent) {
    return entry.pooledPercent === 0
      ? { text: "weekly spent", spent: true }
      : { text: `weekly caps at ${entry.pooledPercent}%`, spent: false }
  }
  return null
}

/**
 * The accounts a row's mean leaves out, by reason, as the fold prints them:
 * those held out because their weekly is spent, then those with no reading.
 * Empty when the mean covers every account.
 *
 * Split because the two explain different things: no reading is a gap in what
 * the page knows, a spent weekly is a fact about the account. A plugin older
 * than `heldOutCount` held nothing out, so all of its exclusions are missing
 * readings.
 */
export function foldNotes(aggregate: Pick<Aggregate, "excludedCount" | "heldOutCount">): string[] {
  const heldOut = typeof aggregate.heldOutCount === "number" ? aggregate.heldOutCount : 0
  const missing = aggregate.excludedCount - heldOut
  const notes: string[] = []
  if (heldOut > 0) notes.push(`${heldOut} weekly spent`)
  if (missing > 0) notes.push(`${missing} without a reading`)
  return notes
}

/**
 * How many accounts a row's mean counts at their weekly rather than their own
 * figure — on Claude's Fable row, every account whose weekly has less left
 * than its Fable. Zero on every other row, and from a plugin that predates
 * `pooledPercent`.
 */
export function cappedByWeekly(entries: Pick<RowEntry, "remainingPercent" | "heldOut" | "pooledPercent">[]): number {
  return entries.filter(
    (entry) =>
      entry.heldOut !== true && typeof entry.pooledPercent === "number" && entry.pooledPercent < entry.remainingPercent,
  ).length
}

/**
 * One account a shut fold names, because it is the reason to open it: its
 * figure and what that figure means in a word — "24% low", "0% out" — so
 * colour is never the only thing saying it. `tone` picks the chip's colour.
 */
export type FoldFlag = { id: string; name: string; figure: string; word: string; tone: "low" | "critical" | "" }

/** The reading's state, as a fold chip says it. */
const FLAG_STATE: Record<string, string> = { error: "failed", stale: "stale" }

/**
 * The accounts a shut window card names on its fold line, in entry order:
 * those running low or out, by their own reading, and those whose reading the
 * server no longer vouches for. An account with no reading is not named — the
 * fold already counts those (foldNotes) — and nor is a healthy one, so a card
 * whose accounts are all fine has a fold line that is just its count.
 *
 * `names` runs parallel to `entries`.
 */
export function foldFlags(
  entries: Pick<RowEntry, "credentialId" | "hasReading" | "remainingPercent" | "level" | "state">[],
  names: string[],
): FoldFlag[] {
  const flags: FoldFlag[] = []
  entries.forEach((entry, index) => {
    if (!entry.hasReading) return
    const name = names[index] ?? entry.credentialId
    const state = FLAG_STATE[entry.state]
    const level = entry.level === "low" ? "low" : entry.level === "critical" ? "critical" : null
    if (level) {
      const word = entry.remainingPercent === 0 ? "out" : level
      flags.push({
        id: entry.credentialId,
        name,
        figure: `${entry.remainingPercent}%`,
        word: state ? `${word} · ${state}` : word,
        tone: level,
      })
    } else if (state) {
      flags.push({ id: entry.credentialId, name, figure: "", word: state, tone: "" })
    }
  })
  return flags
}
