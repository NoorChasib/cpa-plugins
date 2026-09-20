// Which cards the reader has folded shut.
//
// Kept in this browser rather than in the document: it is a preference about
// how one person reads the page, not a fact about anyone's quota, and the
// plugin serves the same bytes to every reader by design.
//
// Stored by `<providerId>:<rowId>` because a row id is only unique inside its
// provider — Claude and Codex both have a `session` row, and keying on the row
// id alone would fold both when the reader folded one.

const STORAGE_KEY = "quota-glance.collapsed"

/**
 * Bounds what one browser can accumulate. Row ids change as providers gain and
 * lose windows, and without a cap the list would keep every id this dashboard
 * ever showed. Far above any real deployment's card count.
 */
const MAX_ENTRIES = 64

export function cardKey(providerId: string, rowId: string): string {
  return `${providerId}:${rowId}`
}

function load(): string[] {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    // Anything that is not a list of strings is treated as absent rather than
    // repaired: the cost of getting it wrong is one card opening.
    return Array.isArray(parsed) ? parsed.filter((item): item is string => typeof item === "string") : []
  } catch {
    // Private browsing, storage disabled, or a value this version cannot read.
    // Every card opens, which is the harmless direction.
    return []
  }
}

function save(keys: string[]): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(keys.slice(-MAX_ENTRIES)))
  } catch {
    /* see load() — the page still works for this session */
  }
}

/** Every folded card, read fresh so a second tab's change is picked up. */
export function collapsedKeys(): Set<string> {
  return new Set(load())
}

export function setCollapsed(key: string, collapsed: boolean): Set<string> {
  const keys = load().filter((item) => item !== key)
  if (collapsed) keys.push(key)
  save(keys)
  return new Set(keys)
}
