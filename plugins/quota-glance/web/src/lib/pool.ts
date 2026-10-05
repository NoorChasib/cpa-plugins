// Naming and colouring the pieces of a pooled bar.
//
// Nothing here takes a share of anything. Every width the bar draws is a
// field the server wrote — `poolShare`, `recoveryShare`, the aggregate's
// fractions — and this module only decides which account a slice belongs to
// and what to call it.

import type { Credential, RowEntry } from "./types"

/**
 * How many identity shades there are before they repeat.
 *
 * Six shades of the accent's cool family (index.css says how they were
 * chosen), ordered so that neighbours — which touch on the bar — differ in
 * both lightness and hue. Past six the cycle repeats, by which point the
 * legend and the row swatches are doing the identifying anyway.
 */
export const IDENTITY_STEPS = 6

/**
 * An account's shade within its provider, from its place in the entries.
 *
 * Every row lists every one of the provider's credentials in the catalog's
 * order, so a credential's index is the same on every card: the same account
 * is the same shade in the bar, the legend and its row, on each card it is on.
 */
export function identitySlot(index: number): number {
  return index % IDENTITY_STEPS
}

/** The part of an address before the "@", which is how a bar labels a slice. */
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
 * Whether this document carries the pool's slices.
 *
 * A plugin older than the pooled bar sends none. Then the bar draws the row's
 * own fraction as one undivided slice and no recovery mark — both straight
 * from the aggregate — rather than this page dividing the mean itself.
 */
export function hasSlices(entries: RowEntry[]): boolean {
  return entries.every((entry) => typeof entry.poolShare === "number" && typeof entry.recoveryShare === "number")
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
