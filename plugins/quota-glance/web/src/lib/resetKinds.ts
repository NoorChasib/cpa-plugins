// Which banked resets an account holds, sorted by what spending one does.
//
// Claude's banked resets are not all alike. A full reset clears the 5-hour and
// weekly limits; a Fable reset only gives back to the weekly limit what Fable
// used; and the plugin spends whichever kind the press names. Mistaking one
// for the other is the error this page has to make impossible, so the card
// shows one group per kind, each with its own mark, words and button, and a
// press always carries the kind of the group it was made in.
//
// The kinds are the plugin's, which are Quota Cache's, which are Claude
// Code's: nothing here classifies a grant. This only reads what the document
// says, defensively — an older plugin sends no kinds at all, and a newer one
// may name a kind this bundle has never seen — and lays the rows out.
//
// Only type imports, so node loads it directly in test/. The words for each
// kind are in outcome.ts, beside the sentences that use them.

import type { Credential, ResetCredits, ResetKindCredits } from "./types"

/**
 * The kinds a press may name: the four Claude Code offers. Anything else —
 * "unknown", or a kind newer than this bundle — is shown and never spent,
 * because a reset the page cannot name is one it cannot promise the effect of.
 */
export const SPENDABLE_KINDS: readonly string[] = ["full", "fable", "weekly", "session"]

/** The group a kind is shown in: its own, or "unknown" for anything unrecognised. */
export const UNKNOWN_KIND = "unknown"

export function isSpendableKind(kind: string): boolean {
  return SPENDABLE_KINDS.includes(kind)
}

/**
 * Where a kind sorts: full first, then Fable, weekly, 5-hour, and anything
 * unrecognised last. The same order as Quota Cache's ResetKindRank, which the
 * document's own `kinds` already follows; sorted again here so the groups
 * stay in that order whatever an older or odder document sends.
 */
export function kindRank(kind: string): number {
  const at = SPENDABLE_KINDS.indexOf(kind)
  return at === -1 ? SPENDABLE_KINDS.length : at
}

/** The group a kind is shown in. */
export function groupOf(kind: string): string {
  return isSpendableKind(kind) ? kind : UNKNOWN_KIND
}

/**
 * A credential's resets by kind, as the page shows them: one entry per group,
 * in kindRank order, each holding at least one.
 *
 * Defensive on every count. A missing `kinds` — an older plugin — is none,
 * and so is anything that is not a list. An entry that is not an object with
 * a string kind, or that holds fewer than one, is dropped. Kinds this bundle
 * does not know are merged into one "unknown" entry, which is never
 * redeemable whatever the document says. And should a document ever list a
 * kind twice, which the plugin promises it will not, the two are merged
 * rather than shown as two rows of the same kind.
 */
export function kindsOf(credits: ResetCredits | null | undefined): ResetKindCredits[] {
  const raw: unknown = credits?.kinds
  if (!Array.isArray(raw)) return []
  const merged = new Map<string, ResetKindCredits>()
  for (const item of raw as unknown[]) {
    if (item === null || typeof item !== "object") continue
    const entry = item as Partial<ResetKindCredits>
    if (typeof entry.kind !== "string") continue
    const count = typeof entry.availableCount === "number" ? entry.availableCount : 0
    if (!(count >= 1)) continue
    const kind = groupOf(entry.kind)
    const next: ResetKindCredits = {
      kind,
      availableCount: count,
      expiresAtEpoch: typeof entry.expiresAtEpoch === "number" ? entry.expiresAtEpoch : null,
      expiresInSeconds: typeof entry.expiresInSeconds === "number" ? entry.expiresInSeconds : null,
      redeemable: entry.redeemable === true && isSpendableKind(kind),
      hold: typeof entry.hold === "string" ? entry.hold : "",
      holdUntilEpoch: typeof entry.holdUntilEpoch === "number" ? entry.holdUntilEpoch : null,
    }
    const held = merged.get(kind)
    merged.set(kind, held ? merge(held, next) : next)
  }
  return [...merged.values()].sort((a, b) => kindRank(a.kind) - kindRank(b.kind))
}

/**
 * Two entries of one group as one: the counts summed, the sooner deadline,
 * spendable only if both are, and held only if both are — a hold says none of
 * the kind can be spent, which one spendable entry disproves.
 */
function merge(a: ResetKindCredits, b: ResetKindCredits): ResetKindCredits {
  const sooner = a.expiresAtEpoch === null || (b.expiresAtEpoch !== null && b.expiresAtEpoch < a.expiresAtEpoch) ? b : a
  const hold = a.hold === "" || b.hold === "" ? { hold: "", holdUntilEpoch: null } : a
  return {
    kind: a.kind,
    availableCount: a.availableCount + b.availableCount,
    expiresAtEpoch: sooner.expiresAtEpoch,
    expiresInSeconds: sooner.expiresInSeconds,
    redeemable: a.redeemable && b.redeemable,
    hold: hold.hold,
    holdUntilEpoch: hold.holdUntilEpoch,
  }
}

/** One account's resets of one kind: a row in that kind's group. */
export interface KindRow {
  credential: Credential
  credits: ResetKindCredits
}

/** Every account holding one kind, in the catalog's order. */
export interface KindGroup {
  kind: string
  rows: KindRow[]
}

/** How a provider's banked resets are laid out on its card. */
export interface ResetLayout {
  /**
   * Accounts whose resets are all one sort — Codex — each a row as it always
   * was, spent with "Use one".
   */
  plain: Credential[]
  /** One group per kind any account holds, in kindRank order. */
  groups: KindGroup[]
  /**
   * Claude accounts holding resets whose kinds were not reported: Quota Cache
   * is older than the kinds. Shown with their count and no button, because a
   * press has to name a kind and nothing here can say which these are.
   */
  unsorted: Credential[]
}

/** The provider whose resets come in kinds, and must not be spent without one. */
const KINDED_PROVIDER = "claude"

/**
 * Sorts every credential holding a reset into the card's layout. Every one of
 * them appears exactly once in `plain` or `unsorted`, or in one or more
 * groups — once per kind it holds.
 */
export function layoutResets(holding: readonly Credential[]): ResetLayout {
  const plain: Credential[] = []
  const unsorted: Credential[] = []
  const groups = new Map<string, KindRow[]>()
  for (const credential of holding) {
    if (!credential.resetCredits) continue
    const kinds = kindsOf(credential.resetCredits)
    if (kinds.length > 0) {
      for (const credits of kinds) {
        const rows = groups.get(credits.kind) ?? []
        rows.push({ credential, credits })
        groups.set(credits.kind, rows)
      }
    } else if (credential.provider === KINDED_PROVIDER) {
      unsorted.push(credential)
    } else {
      plain.push(credential)
    }
  }
  return {
    plain,
    groups: [...groups.entries()]
      .map(([kind, rows]) => ({ kind, rows }))
      .sort((a, b) => kindRank(a.kind) - kindRank(b.kind)),
    unsorted,
  }
}

/** One account's resets of one kind, by count, for a sentence. */
export interface KindCount {
  kind: string
  count: number
}

/**
 * The resets an account holds of every kind but one, in kindRank order: what
 * the dialog says stays banked when a reset of `kind` is spent.
 */
export function othersHeld(credits: ResetCredits | null | undefined, kind: string): KindCount[] {
  return kindsOf(credits)
    .filter((entry) => entry.kind !== kind)
    .map((entry) => ({ kind: entry.kind, count: entry.availableCount }))
}

/**
 * Whether anything on the card would have a button, were this browser able to
 * spend: a plain row the server offers, or a spendable kind it offers.
 */
export function anyRedeemable(holding: readonly Credential[]): boolean {
  return holding.some((credential) => {
    const credits = credential.resetCredits
    if (!credits) return false
    const kinds = kindsOf(credits)
    if (kinds.length > 0) return kinds.some((entry) => entry.redeemable)
    return credential.provider !== KINDED_PROVIDER && credits.redeemable
  })
}

/** The account's entry for one kind, as the page shows it, or null when it holds none. */
export function kindEntry(credential: Credential, kind: string): ResetKindCredits | null {
  return kindsOf(credential.resetCredits).find((entry) => entry.kind === kind) ?? null
}
