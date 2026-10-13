// The summary's strings, as the plugin wrote them.
//
// While CPA holds the plugin to an RPC schema below 6, it HTML-escapes every
// string value of a management answer, so on the console path an address with
// an apostrophe arrives as &#39; and React would print the entity. Quota
// Glance answers the schema CPA offers, at most 6, which every supported CPA
// offers; on an older one the summary says `stringsEscaped`, and only then is
// anything decoded: on a raw document a label that really holds "&amp;" is its
// own text. The resource path is never escaped. Keys are never escaped either,
// so they are never decoded.
//
// The press's answers are not decoded: every string they carry is a code that
// escaping leaves alone (TestEveryPressAnswerReadsTheSameEscapedOrNot).
//
// Imports nothing, so node loads it directly in test/.

const ENTITIES: Record<string, string> = { "&lt;": "<", "&gt;": ">", "&#34;": '"', "&#39;": "'", "&amp;": "&" }

/**
 * The inverse of Go's html.EscapeString, which is what CPA applies. One pass,
 * so a label's own "&lt;", escaped to "&amp;lt;", comes back as "&lt;".
 */
function unescapeString(value: string): string {
  return value.replace(/&(?:lt|gt|#34|#39|amp);/g, (entity) => ENTITIES[entity] ?? entity)
}

function unescapeAll(value: unknown): unknown {
  if (typeof value === "string") return unescapeString(value)
  if (Array.isArray(value)) return value.map(unescapeAll)
  // Object.fromEntries defines each key as its own property, so a key such as
  // __proto__ stays data.
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, unescapeAll(item)]))
  }
  return value
}

/** The summary as the plugin built it: decoded when, and only when, it says CPA escaped it. */
export function decodeSummary<T>(document: T): T {
  const escaped =
    document !== null && typeof document === "object" && (document as { stringsEscaped?: unknown }).stringsEscaped === true
  return escaped ? (unescapeAll(document) as T) : document
}
