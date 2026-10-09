// Which cards the reader has opened.
//
// Kept in this browser rather than in the document: it is a preference about
// how one person reads the page, not a fact about anyone's quota, and the
// plugin serves the same bytes to every reader by design.
//
// Every fold starts shut, so what is stored is the exception: the cards the
// reader opened. This used to be the other way round — the cards folded shut,
// under LEGACY_KEY — and that list is dropped rather than inverted, so a
// reader who had every card open meets the calmer page too.
//
// Stored by `<providerId>:<rowId>` because a row id is only unique inside its
// provider — Claude and Codex both have a `session` row, and keying on the row
// id alone would open both when the reader opened one. A card that is not a
// window takes a name no row id can have; see CARD.

const STORAGE_KEY = "quota-glance.opened"
const LEGACY_KEY = "quota-glance.collapsed"

/**
 * Bounds what one browser can accumulate. Row ids change as providers gain and
 * lose windows, and without a cap the list would keep every id this dashboard
 * ever showed. Far above any real deployment's card count.
 */
const MAX_ENTRIES = 64

/**
 * The cards in a section that are not windows. A server row id is a window
 * key or a model name, never one starting with "@", so these cannot collide
 * with one.
 */
export const CARD = {
  accounts: "@accounts",
  apiCredits: "@api-credits",
} as const

export function cardKey(providerId: string, rowId: string): string {
  return `${providerId}:${rowId}`
}

function load(): string[] {
  try {
    // globalThis rather than window, which is the same object in a browser
    // and lets the tests stand in a store of their own under node.
    const storage = globalThis.localStorage
    // The old list is read by nothing and would otherwise sit in storage for
    // good. Removing it is the whole migration.
    storage.removeItem(LEGACY_KEY)
    const raw = storage.getItem(STORAGE_KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    // Anything that is not a list of strings is treated as absent rather than
    // repaired: the cost of getting it wrong is one card shutting.
    return Array.isArray(parsed) ? parsed.filter((item): item is string => typeof item === "string") : []
  } catch {
    // Private browsing, storage disabled, or a value this version cannot read.
    // Every card stays shut, which is the default anyway.
    return []
  }
}

function save(keys: string[]): void {
  try {
    globalThis.localStorage.setItem(STORAGE_KEY, JSON.stringify(keys.slice(-MAX_ENTRIES)))
  } catch {
    /* see load() — the page still works for this session */
  }
}

/** Every opened card, read fresh so a second tab's change is picked up. */
export function openedKeys(): Set<string> {
  return new Set(load())
}

export function setOpened(key: string, opened: boolean): Set<string> {
  const keys = load().filter((item) => item !== key)
  if (opened) keys.push(key)
  save(keys)
  return new Set(keys)
}
