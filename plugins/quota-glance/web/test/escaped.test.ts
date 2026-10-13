// The summary as CPA hands it to the page. Below schema 6, CPA HTML-escapes
// every string value of a management answer, so the console path's summary
// arrives with an address's apostrophe as &#39;, and says so with
// stringsEscaped. The page decodes it then, and only then: a raw document's
// "&amp;" is a label's own text.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"

import { decodeSummary } from "../src/lib/escaped.ts"

const golden = JSON.parse(readFileSync(new URL("../../testdata/golden/summary.json", import.meta.url), "utf8"))

/** What CPA does to a management answer below schema 6: Go's html.EscapeString on every string value, keys untouched. */
function cpaEscape(value: unknown): unknown {
  const entities: Record<string, string> = { "&": "&amp;", "'": "&#39;", "<": "&lt;", ">": "&gt;", '"': "&#34;" }
  if (typeof value === "string") return value.replace(/[&'<>"]/g, (c) => entities[c] ?? c)
  if (Array.isArray(value)) return value.map(cpaEscape)
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, cpaEscape(item)]))
  }
  return value
}

/** The golden summary with text in it that CPA's escaping changes, including an entity that is the label's own. */
function awkward(): Record<string, unknown> {
  const document = structuredClone(golden)
  document.credentials[0].email = "o'brien&co@example.com"
  document.credentials[0].plan = `Pro <Max> "20x"`
  document.providers[0].title = "R&amp;D's &lt;b&gt;"
  return document
}

describe("decodeSummary", () => {
  test("a summary CPA escaped decodes to the one the plugin built", () => {
    const built = { ...awkward(), stringsEscaped: true }
    const escaped = cpaEscape(built) as Record<string, unknown>
    assert.notDeepEqual(escaped, built)
    assert.equal(escaped.stringsEscaped, true)
    assert.deepEqual(decodeSummary(escaped), built)
  })

  test("a raw summary is left as it is, entities and all", () => {
    for (const flag of [false, undefined]) {
      const raw: Record<string, unknown> = { ...awkward(), stringsEscaped: flag }
      assert.equal(decodeSummary(raw), raw)
      assert.equal((decodeSummary(raw).providers as { title: string }[])[0]?.title, "R&amp;D's &lt;b&gt;")
    }
  })

  test("only the flag's own true decodes", () => {
    for (const flag of ["true", 1, null]) {
      const raw = { stringsEscaped: flag, plan: "a &amp; b" }
      assert.equal(decodeSummary(raw).plan, "a &amp; b")
    }
  })

  test("keys stay as they are, and a __proto__ key stays data", () => {
    const decoded = decodeSummary(
      JSON.parse('{"stringsEscaped":true,"a&amp;b":"x&amp;y","nested":{"__proto__":"p&#39;s"}}'),
    ) as Record<string, unknown>
    assert.equal(decoded["a&amp;b"], "x&y")
    const nested = decoded.nested as Record<string, unknown>
    assert.ok(Object.hasOwn(nested, "__proto__"))
    assert.equal(nested["__proto__"], "p's")
    assert.equal(Object.getPrototypeOf(nested), Object.prototype)
  })
})
