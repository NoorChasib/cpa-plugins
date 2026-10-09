// The dev fixture route, driven over real HTTP.
//
// It is the only server the page meets in development, so it has to refuse
// the way CPA and the plugin refuse — the page's classifier is keyed on those
// exact answers — and it has to answer every press itself. I13: no spend or
// redeem path is ever passed on, whatever the method, so nothing pressed
// against the dev server can reach the CPA QUOTA_GLANCE_PROXY points at; and
// the same for both settings paths, which it answers from a store of its own.
//
// node:http rather than fetch, so no client adds headers of its own: the
// fetch-metadata gate reads exactly the ones a test sends.

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { createServer, type IncomingMessage, request, type Server, type ServerResponse } from "node:http"
import type { AddressInfo } from "node:net"
import { after, before, describe, test } from "node:test"

import { EPOCH_FIELDS, goldenFixtureRoute, PRESS_PATHS, SETTINGS_PATHS } from "../dev/fixture-route.ts"
import { classifyRefusal, encodeSettingsHeader, encodeSpendHeader, type SettingsBatch } from "../src/lib/access.ts"
import type { APICredits, RowEntry, Summary } from "../src/lib/types.ts"

type Middleware = (req: IncomingMessage, res: ServerResponse, next: () => void) => void

/** A middleware chain like Vite's, ending where Vite's proxy would take over. */
const PASSED_THROUGH = 599

/**
 * One dev server's fixture, over HTTP. Each group of tests gets its own,
 * because the fixture keeps CPA's failure count as CPA does — per address,
 * for the life of the server — and one group's deliberate refusals must not
 * ban the next group's requests.
 */
function serve() {
  const stack: Middleware[] = []
  const warnings: string[] = []
  let server: Server | null = null
  let base = ""
  before(async () => {
    const fakeServer = {
      middlewares: { use: (fn: Middleware) => stack.push(fn) },
      config: { logger: { warn: (message: string) => warnings.push(message) } },
    }
    const hook = goldenFixtureRoute({ pressDelayMs: 0 }).configureServer
    const configure = typeof hook === "function" ? hook : hook?.handler
    assert.ok(configure)
    await configure.call({} as never, fakeServer as never)
    const running = createServer((req, res) => {
      let at = 0
      const next = () => {
        const middleware = stack[at++]
        if (middleware) middleware(req, res, next)
        else {
          res.statusCode = PASSED_THROUGH
          res.end("passed through")
        }
      }
      next()
    })
    await new Promise<void>((resolve) => running.listen(0, "127.0.0.1", resolve))
    server = running
    base = `http://127.0.0.1:${(running.address() as AddressInfo).port}`
  })
  after(() => server?.close())
  return { warnings, call: (method: string, path: string, headers: Record<string, string> = {}, body?: string) => call(base, method, path, headers, body) }
}

type Reply = { status: number; headers: IncomingMessage["headers"]; body: string; error?: string }

function call(base: string, method: string, path: string, headers: Record<string, string>, body?: string): Promise<Reply> {
  return new Promise((resolve) => {
    const req = request(`${base}${path}`, { method, headers }, (res) => {
      let text = ""
      res.setEncoding("utf8")
      res.on("data", (chunk: string) => (text += chunk))
      res.on("end", () => resolve({ status: res.statusCode ?? 0, headers: res.headers, body: text }))
    })
    req.on("error", (error) => resolve({ status: 0, headers: {}, body: "", error: error.message }))
    if (body !== undefined) req.write(body)
    req.end()
  })
}

const SPEND = "/v0/resource/plugins/quota-glance/spend"
const REDEEM = "/v0/management/plugins/quota-glance/redeem"
const SUMMARY = "/v0/management/plugins/quota-glance/summary"
const RESOURCE_SUMMARY = "/v0/resource/plugins/quota-glance/summary"
const TOKEN = { Authorization: "Bearer dev-token" }

let pressCount = 0
/** A fresh, valid press id. */
const freshId = () => `press-id-${String(++pressCount).padStart(8, "0")}`
const spendHeaders = (body: Record<string, unknown>): Record<string, string> => ({
  ...TOKEN,
  "X-Quota-Glance-Spend": encodeSpendHeader(body as never),
})
const press = (credentialId = "codex-a.json", pressId = freshId()) => ({ credentialId, confirmed: true as const, pressId })
const errorOf = (reply: Reply) => (JSON.parse(reply.body) as { error: string }).error

describe("I13: every press path is answered here", () => {
  const { call } = serve()

  for (const path of PRESS_PATHS) {
    for (const method of ["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"]) {
      test(`${method} ${path}`, async () => {
        const reply = await call(method, path, { ...TOKEN })
        assert.notEqual(reply.status, PASSED_THROUGH)
      })
    }
  }

  test("a query string does not change that", async () => {
    for (const path of PRESS_PATHS) {
      assert.notEqual((await call("POST", `${path}?redeem=reset&scenario=golden`)).status, PASSED_THROUGH)
    }
  })

  test("everything else is passed on", async () => {
    assert.equal((await call("GET", "/v0/management/config")).status, PASSED_THROUGH)
  })
})

describe("every settings path is answered here (E.9)", () => {
  const { call } = serve()

  for (const path of SETTINGS_PATHS) {
    for (const method of ["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"]) {
      test(`${method} ${path}`, async () => {
        const reply = await call(method, path, { ...TOKEN, Authorization: "Bearer dev-token" })
        assert.notEqual(reply.status, PASSED_THROUGH)
      })
    }
  }

  test("a query string does not change that", async () => {
    for (const path of SETTINGS_PATHS) {
      assert.notEqual((await call("POST", `${path}?save=saved&scenario=degraded`)).status, PASSED_THROUGH)
    }
  })
})

describe("saving settings", () => {
  const { call } = serve()
  const SAVE = "/v0/resource/plugins/quota-glance/save-settings"
  const CONSOLE = "/v0/management/plugins/quota-glance/settings"
  const KEY = { Authorization: "Bearer dev", "Content-Type": "application/json" }
  const saveHeaders = (batch: SettingsBatch) => ({ ...TOKEN, "X-Quota-Glance-Settings": encodeSettingsHeader(batch) })
  const credit = (base: string, monthlyUsd: string | null): SettingsBatch => ({
    kind: "apiCredits",
    items: [{ id: "org-11e594f48195", baseRevision: base, monthlyUsd, renews: null, reading: null }],
  })
  const summary = async (scenario = "golden") =>
    JSON.parse((await call("GET", `${RESOURCE_SUMMARY}?scenario=${scenario}`, TOKEN)).body) as Summary
  const alpha = (doc: Summary) => doc.apiCredits!.accounts.find((account) => account.label === "alpha")!

  test("the token door saves, the summary shows it, and a resend changes nothing", async () => {
    const first = await call("GET", SAVE, saveHeaders(credit("", "300")))
    assert.equal(first.status, 200, first.body)
    const body = JSON.parse(first.body) as { ok: boolean; unchanged: boolean; revision: string; settings: Record<string, { monthlyUsd: string }> }
    assert.deepEqual([body.ok, body.unchanged], [true, false])
    assert.equal(body.settings["org-11e594f48195"]!.monthlyUsd, "300")

    const after = alpha(await summary())
    assert.deepEqual([after.settings.monthlyUsd, after.monthlyCreditSource, after.monthlyCreditText], ["300", "dashboard", "$300.00"])

    const again = await call("GET", SAVE, saveHeaders(credit("", "300")))
    assert.deepEqual(JSON.parse(again.body), { ok: true, unchanged: true, revision: body.revision })
  })

  test("a credit edit keeps an unchanged reading's time and metered spend after rebasing", async () => {
    const doc = await summary()
    const bravo = doc.apiCredits!.accounts.find((account) => account.label === "bravo")!
    const reading = bravo.settings.reading!
    const batch: SettingsBatch = { kind: "apiCredits", items: [{
      id: bravo.id, baseRevision: bravo.settings.revision, monthlyUsd: "300", renews: bravo.settings.renews || null,
      reading: { remainingUsd: reading.remainingUsd, at: new Date(reading.atEpoch * 1000).toISOString().replace(".000Z", "Z") },
    }] }
    const saved = await call("GET", SAVE, saveHeaders(batch))
    assert.equal(saved.status, 200, saved.body)
    const after = (await summary()).apiCredits!.accounts.find((account) => account.id === bravo.id)!
    assert.equal(after.settings.reading!.atEpoch, reading.atEpoch)
    assert.equal(after.settings.reading!.enteredAtEpoch, reading.enteredAtEpoch)
    assert.equal(after.reading!.spentSince, bravo.reading!.spentSince)
    assert.equal(after.left, bravo.left)
  })

  test("a row saved elsewhere since it was opened is a conflict, with what it holds now", async () => {
    const reply = await call("GET", SAVE, saveHeaders(credit("", "310")))
    assert.equal(reply.status, 409)
    const body = JSON.parse(reply.body) as { error: string; conflicts: string[]; current: Record<string, { monthlyUsd: string }> }
    assert.equal(body.error, "conflict")
    assert.deepEqual(body.conflicts, ["org-11e594f48195"])
    assert.equal(body.current["org-11e594f48195"]!.monthlyUsd, "300")
  })

  test("one invalid row saves none of the batch", async () => {
    const doc = await summary()
    const first = alpha(doc)
    const second = doc.apiCredits!.accounts.find((account) => account.label === "bravo")!
    const batch: SettingsBatch = { kind: "apiCredits", items: [
      { id: first.id, baseRevision: first.settings.revision, monthlyUsd: "500", renews: null, reading: null },
      { id: second.id, baseRevision: second.settings.revision, monthlyUsd: "12,50", renews: null, reading: null },
    ] }
    const saved = await call("GET", SAVE, saveHeaders(batch))
    assert.equal(saved.status, 400)
    assert.equal(alpha(await summary()).settings.monthlyUsd, first.settings.monthlyUsd)
  })

  test("a saved renewal orphan can be removed when no credentials remain", async () => {
    const path = "?scenario=renewal-orphans"
    const batch: SettingsBatch = { kind: "renewals", items: [{ id: "0123456789abcdef", baseRevision: "1", date: null }] }
    const saved = await call("GET", `${SAVE}${path}`, saveHeaders(batch))
    assert.equal(saved.status, 200, saved.body)
    assert.deepEqual((await summary("renewal-orphans")).renewalOrphans, [])
  })

  test("the console door: CPA's sign-in, POST only, JSON only", async () => {
    const revision = alpha(await summary()).settings.revision
    const saved = await call("POST", CONSOLE, KEY, JSON.stringify(credit(revision, null)))
    assert.equal(saved.status, 200, saved.body)
    assert.equal(alpha(await summary()).monthlyCreditSource, "config")
    assert.equal((await call("POST", CONSOLE, { "Content-Type": "application/json" }, "{}")).status, 401)
    assert.equal((await call("GET", CONSOLE, KEY)).status, 404)
    assert.equal((await call("POST", CONSOLE, { ...KEY, "Content-Type": "text/plain" }, "{}")).status, 415)
  })

  test("the token door is GET only, fenced as /spend is", async () => {
    assert.equal((await call("POST", SAVE, saveHeaders(credit("", "1")))).status, 404)
    assert.equal((await call("GET", SAVE, { ...saveHeaders(credit("", "1")), "Sec-Fetch-Site": "cross-site" })).status, 403)
    assert.equal((await call("GET", SAVE, { ...saveHeaders(credit("", "1")), "Early-Data": "1" })).status, 425)
    assert.equal((await call("GET", SAVE, { "X-Quota-Glance-Settings": encodeSettingsHeader(credit("", "1")) })).status, 401)
    assert.equal((await call("GET", SAVE, { ...TOKEN, "X-Quota-Glance-Settings": "not base64url!" })).status, 400)
  })

  test("values, shape and editability are checked in the plugin's order", async () => {
    const error = async (batch: unknown) =>
      JSON.parse((await call("GET", SAVE, saveHeaders(batch as SettingsBatch))).body) as Record<string, unknown>
    assert.deepEqual(await error(credit("", "12,50")), { error: "invalid_monthly_usd", id: "org-11e594f48195", field: "monthlyUsd" })
    assert.equal((await error({ kind: "apiCredits", items: [{ id: "x" }] })).error, "invalid_request")
    assert.deepEqual(await error({ kind: "renewals", items: [{ id: "claude-agency@example.com.json", baseRevision: "", date: "2026-10-27" }] }), {
      error: "not_editable",
      ids: ["claude-agency@example.com.json"],
    })
  })

  test("editing switched off is a 404; settings.json unreadable a 503", async () => {
    const off = await call("GET", `${SAVE}?scenario=degraded`, saveHeaders(credit("", "1")))
    assert.deepEqual([off.status, JSON.parse(off.body).error], [404, "not_found"])
    const unreadable = await call("GET", `${SAVE}?scenario=settings-unreadable`, saveHeaders(credit("", "1")))
    assert.deepEqual([unreadable.status, JSON.parse(unreadable.body).error], [503, "settings_unavailable"])
  })

  test("unreadable settings apply no stored values in the fixture", async () => {
    const doc = await summary("settings-unreadable")
    const bravo = doc.apiCredits!.accounts.find((account) => account.label === "bravo")!
    assert.equal(bravo.monthlyCreditSource, "config")
    assert.equal(bravo.settings.monthlyUsd, "")
    assert.equal(bravo.settings.reading, null)
    assert.equal(bravo.settings.editable, false)
    assert.equal(doc.credentials.find((one) => one.id === "5f2b8c41d09e7a36")!.renewalSetting, null)
  })

  test("serves every ending the page has copy for", async () => {
    const endings: [string, number, string][] = [
      ["unwritable", 503, "settings_unwritable"],
      ["full", 409, "settings_full"],
      ["throttled", 429, "too_many_writes"],
      ["not-editable", 409, "not_editable"],
      ["unavailable", 503, "settings_unavailable"],
      ["switched-off", 404, "not_found"],
    ]
    for (const [ending, status, code] of endings) {
      const reply = await call("GET", `${SAVE}?scenario=api-states&save=${ending}`, saveHeaders(credit("", "123")))
      assert.equal(reply.status, status, ending)
      assert.equal(JSON.parse(reply.body).error, code, ending)
    }
    const gateway = await call("GET", `${SAVE}?scenario=api-states&save=gateway`, saveHeaders(credit("", "124")))
    assert.equal(gateway.status, 504)
  })
})

describe("the token door's spend route", () => {
  const { call } = serve()

  test("spends with the password and a well-formed press", async () => {
    const reply = await call("GET", SPEND, spendHeaders(press("claude-a.json")))
    assert.equal(reply.status, 200)
    assert.equal(reply.headers["cache-control"], "no-store")
    assert.deepEqual(JSON.parse(reply.body), {
      provider: "claude",
      outcome: "reset",
      windowsReset: 2,
      remainingCount: 1,
      snapshotPending: true,
    })
  })

  test("answers a second copy of a press from the ledger, byte for byte", async () => {
    const body = press()
    const first = await call("GET", `${SPEND}?redeem=nothing-to-reset`, spendHeaders(body))
    const second = await call("GET", `${SPEND}?redeem=reset`, spendHeaders(body))
    assert.equal(first.headers["x-quota-glance-replayed"], undefined)
    assert.equal(second.headers["x-quota-glance-replayed"], "1")
    assert.equal(second.body, first.body)
  })

  test("a dropped answer is the plugin's reset lost on the way back, and the same press hears it", async () => {
    const body = press()
    const dropped = await call("GET", `${SPEND}?redeem=dropped`, spendHeaders(body))
    assert.equal(dropped.status, 0)
    const again = await call("GET", `${SPEND}?redeem=dropped`, spendHeaders(body))
    assert.equal(again.status, 200)
    assert.equal(again.headers["x-quota-glance-replayed"], "1")
    assert.equal((JSON.parse(again.body) as { outcome: string }).outcome, "reset")
  })

  test("refuses a press id reused for another credential", async () => {
    const id = freshId()
    await call("GET", SPEND, spendHeaders(press("codex-a.json", id)))
    const other = await call("GET", SPEND, spendHeaders(press("codex-b.json", id)))
    assert.equal(other.status, 400)
    assert.equal(errorOf(other), "invalid_request")
  })

  test("refuses a wrong or missing password with a bare 401", async () => {
    for (const headers of [{}, { Authorization: "Bearer wrong" }] as Record<string, string>[]) {
      const reply = await call("GET", SPEND, { ...headers, "X-Quota-Glance-Spend": encodeSpendHeader(press()) })
      assert.equal(reply.status, 401)
      assert.equal(reply.body, "")
      assert.equal(classifyRefusal("token", reply.status, null)?.code, "token_refused")
    }
  })

  test("refuses a malformed press, before anything is spent", async () => {
    const cases: [Record<string, string>, string][] = [
      [{ ...TOKEN }, "invalid_request"],
      [{ ...TOKEN, "X-Quota-Glance-Spend": "not base64url!" }, "invalid_request"],
      [{ ...TOKEN, "X-Quota-Glance-Spend": Buffer.from("[1]").toString("base64url") }, "invalid_request"],
      [{ ...TOKEN, "X-Quota-Glance-Spend": "x".repeat(4097) }, "invalid_request"],
      [spendHeaders({ credentialId: "a", confirmed: false, pressId: freshId() }), "confirmation_required"],
      [spendHeaders({ credentialId: "a", confirmed: true }), "invalid_request"],
      [spendHeaders({ credentialId: "a", confirmed: true, pressId: "short" }), "invalid_request"],
      [spendHeaders({ credentialId: "a", confirmed: true, pressId: "has spaces in it ok" }), "invalid_request"],
    ]
    for (const [headers, code] of cases) {
      const reply = await call("GET", SPEND, headers)
      assert.equal(reply.status, 400)
      assert.equal(errorOf(reply), code)
    }
  })

  test("refuses what the browser says did not come from this page, before the password is looked at", async () => {
    const refused: Record<string, string>[] = [
      { "Sec-Fetch-Site": "cross-site" },
      { "Sec-Fetch-Site": "same-site" },
      { "Sec-Fetch-Site": "none" },
      { "Sec-Fetch-Mode": "navigate" },
      { "Sec-Fetch-Dest": "document" },
      { "Sec-Purpose": "prefetch" },
      { Purpose: "prefetch" },
    ]
    for (const headers of refused) {
      const reply = await call("GET", SPEND, { ...headers, "X-Quota-Glance-Spend": encodeSpendHeader(press()) })
      assert.equal(reply.status, 403, JSON.stringify(headers))
      assert.equal(errorOf(reply), "cross_site")
    }
    const page = await call("GET", SPEND, {
      ...spendHeaders(press()),
      "Sec-Fetch-Site": "same-origin",
      "Sec-Fetch-Mode": "same-origin",
      "Sec-Fetch-Dest": "empty",
    })
    assert.equal(page.status, 200)
  })

  test("refuses early data", async () => {
    const reply = await call("GET", SPEND, { ...spendHeaders(press()), "Early-Data": "1" })
    assert.equal(reply.status, 425)
    assert.equal(classifyRefusal("token", 425, errorOf(reply))?.code, "too_early")
  })

  test("is GET only, as CPA dispatches it", async () => {
    for (const method of ["POST", "PUT", "DELETE"]) {
      const reply = await call(method, SPEND, spendHeaders(press()))
      assert.equal(reply.status, 404)
      assert.equal(classifyRefusal("token", 404, null)?.code, "route_missing")
    }
  })

  test("serves every ending the page has copy for", async () => {
    const endings: [string, number, string][] = [
      ["switched-off", 404, "not_found"],
      ["cross-site", 403, "cross_site"],
      ["too-early", 425, "too_early"],
      ["outcome-unknown", 502, "outcome_unknown"],
      ["rate-limited", 502, "provider_rate_limited"],
      ["in-flight", 409, "already_in_flight"],
      ["not-redeemable", 409, "not_redeemable"],
    ]
    for (const [ending, status, code] of endings) {
      const reply = await call("GET", `${SPEND}?redeem=${ending}`, spendHeaders(press()))
      assert.equal(reply.status, status, ending)
      assert.equal(errorOf(reply), code, ending)
    }
    const gateway = await call("GET", `${SPEND}?redeem=gateway`, spendHeaders(press()))
    assert.equal(gateway.status, 504)
    assert.match(gateway.body, /Gateway/)
  })

  test("the old resource POST is answered as CPA answers it", async () => {
    const post = await call("POST", "/v0/resource/plugins/quota-glance/redeem", { ...TOKEN }, "{}")
    assert.equal(post.status, 404)
    assert.equal(post.body, "")
  })
})

describe("the console door, behind CPA's sign-in", () => {
  const { call } = serve()

  test("CPA refuses a missing key in JSON, in its own words, with its version header", async () => {
    for (const [method, path] of [["GET", SUMMARY], ["POST", REDEEM]]) {
      const reply = await call(method!, path!, { "Content-Type": "application/json" }, method === "POST" ? JSON.stringify(press()) : undefined)
      assert.equal(reply.status, 401)
      assert.equal(reply.headers["x-cpa-version"], "dev")
      assert.equal(errorOf(reply), "missing management key")
      assert.equal(classifyRefusal("console", 401, errorOf(reply))?.code, "console_refused")
    }
  })

  test("CPA's refusals by scenario and ending are the ones the classifier knows", async () => {
    const key = { Authorization: "Bearer dev" }
    const cases: [string, number, string][] = [
      [`${SUMMARY}?scenario=cpa-expired`, 401, "console_refused"],
      [`${SUMMARY}?scenario=cpa-banned`, 403, "ip_banned"],
      [`${SUMMARY}?scenario=cpa-remote-off`, 403, "remote_disabled"],
    ]
    for (const [path, status, code] of cases) {
      const reply = await call("GET", path, key)
      assert.equal(reply.status, status, path)
      assert.equal(classifyRefusal("console", status, errorOf(reply))?.code, code, path)
    }
    for (const [ending, status, code] of [["key-refused", 401, "console_refused"], ["ip-banned", 403, "ip_banned"], ["remote-off", 403, "remote_disabled"]] as const) {
      const reply = await call("POST", `${REDEEM}?redeem=${ending}`, { ...key, "Content-Type": "application/json" }, JSON.stringify(press()))
      assert.equal(reply.status, status, ending)
      assert.equal(classifyRefusal("console", status, errorOf(reply))?.code, code, ending)
    }
    // A clean sign-in clears the count the two refusals above added.
    assert.equal((await call("GET", SUMMARY, key)).status, 200)
  })

  test("a press with a key spends, and a press id is optional on this door", async () => {
    const headers = { Authorization: "Bearer dev", "Content-Type": "application/json" }
    const withId = await call("POST", REDEEM, headers, JSON.stringify(press("claude-x.json")))
    assert.equal(withId.status, 200)
    assert.equal((JSON.parse(withId.body) as { provider: string }).provider, "claude")
    const without = await call("POST", REDEEM, headers, JSON.stringify({ credentialId: "codex-a.json", confirmed: true }))
    assert.equal(without.status, 200)
    const form = await call("POST", REDEEM, { ...headers, "Content-Type": "application/x-www-form-urlencoded" }, "a=b")
    assert.equal(form.status, 415)
  })

  test("the password and the console key each open only their own tree", async () => {
    assert.equal((await call("GET", RESOURCE_SUMMARY, TOKEN)).status, 200)
    assert.equal((await call("GET", RESOURCE_SUMMARY, { Authorization: "Bearer dev" })).status, 401)
    assert.equal((await call("GET", `${RESOURCE_SUMMARY}?scenario=token-refused`, TOKEN)).status, 401)
    assert.equal((await call("GET", `${SUMMARY}?scenario=token-refused`, { Authorization: "Bearer dev" })).status, 200)
  })

})

describe("CPA's ban, as the fixture keeps it", () => {
  const { call, warnings } = serve()

  test("five counted failures ban the address, as CPA does", async () => {
    for (let i = 0; i < 5; i++) assert.equal((await call("GET", SUMMARY)).status, 401)
    const banned = await call("GET", SUMMARY, { Authorization: "Bearer dev" })
    assert.equal(banned.status, 403)
    assert.match(errorOf(banned), /^IP banned due to too many failed attempts\. Try again in (29m59s|30m0s)$/)
    assert.equal(classifyRefusal("console", 403, errorOf(banned))?.code, "ip_banned")
    assert.ok(warnings.some((line) => line.includes("ban this address")))
    // The token door is the plugin's own, and CPA's ban never reaches it.
    assert.equal((await call("GET", SPEND, spendHeaders(press()))).status, 200)
  })
})

describe("the weekly-spent scenario", () => {
  const { call } = serve()

  test("holds every Claude account out of the session, and adds up as a real document does", async () => {
    const reply = await call("GET", `${RESOURCE_SUMMARY}?scenario=weekly-spent`, TOKEN)
    assert.equal(reply.status, 200)
    const claude = (JSON.parse(reply.body) as Summary).providers.find((provider) => provider.id === "claude")
    assert.ok(claude)
    const row = (rowId: string) => {
      const found = claude.rows.find((candidate) => candidate.rowId === rowId)
      assert.ok(found, rowId)
      return found
    }

    // The sums checkPoolAddsUp in pool_test.go holds the server to, so the
    // hand-patched document cannot drift from what the server would send.
    for (const { rowId, aggregate, entries } of claude.rows) {
      const sum = (share: (entry: RowEntry) => number | undefined) =>
        entries.reduce((total, entry) => total + (share(entry) ?? 0), 0)
      assert.equal(aggregate.memberCount + aggregate.excludedCount, claude.credentialCount, rowId)
      assert.ok(Math.abs(sum((entry) => entry.poolShare) - aggregate.remainingFraction) < 1e-9, rowId)
      assert.ok(Math.abs(sum((entry) => entry.recoveryShare) - (aggregate.projectedGainFraction ?? 0)) < 1e-9, rowId)
      assert.equal(entries.filter((entry) => entry.heldOut === true).length, aggregate.heldOutCount ?? 0, rowId)
    }

    const session = row("session")
    assert.equal(session.aggregate.memberCount, 0)
    assert.equal(session.aggregate.heldOutCount, 5)
    assert.equal(session.aggregate.remainingPercent, 0)
    assert.ok(session.entries.filter((entry) => entry.hasReading).every((entry) => entry.heldOut === true))
    assert.equal(row("weekly").aggregate.remainingPercent, 0)
    assert.equal(row("weekly_fable").aggregate.remainingPercent, 0)
  })
})

describe("rebasing", () => {
  const { call } = serve()
  const golden = (name: string): unknown =>
    JSON.parse(readFileSync(new URL(`../../testdata/golden/${name}.json`, import.meta.url), "utf8"))

  /** Every key ending in Epoch, anywhere in the document. */
  const epochKeys = (value: unknown, into = new Set<string>()): Set<string> => {
    if (Array.isArray(value)) for (const item of value) epochKeys(item, into)
    else if (value && typeof value === "object") {
      for (const [key, item] of Object.entries(value)) {
        if (key.endsWith("Epoch")) into.add(key)
        epochKeys(item, into)
      }
    }
    return into
  }

  // A name missing from EPOCH_FIELDS fails nothing else: the field is simply
  // left at the fixture's frozen instant, and its countdown reads long past.
  test("moves every epoch field either golden document carries", () => {
    for (const name of ["summary", "summary-degraded"]) {
      for (const key of epochKeys(golden(name))) assert.ok(EPOCH_FIELDS.has(key), `${name}: ${key} is not rebased`)
    }
  })

  test("and every one each scenario carries", async () => {
    for (const scenario of ["api-states", "meter-stopped", "no-meter", "settings-unreadable", "degraded-editable", "single-claude", "renewal-orphans", "renewals-editable"]) {
      const reply = await call("GET", `${RESOURCE_SUMMARY}?scenario=${scenario}`, TOKEN)
      assert.equal(reply.status, 200, scenario)
      for (const key of epochKeys(JSON.parse(reply.body))) assert.ok(EPOCH_FIELDS.has(key), `${scenario}: ${key} is not rebased`)
    }
  })

  test("keeps every API credit countdown in step with its instant", async () => {
    for (const scenario of ["golden", "degraded"]) {
      const reply = await call("GET", `${RESOURCE_SUMMARY}?scenario=${scenario}`, TOKEN)
      assert.equal(reply.status, 200)
      const doc = JSON.parse(reply.body) as Summary
      const credits = doc.apiCredits as APICredits
      assert.ok(credits, scenario)
      const now = doc.generatedAtEpoch
      for (const account of credits.accounts) {
        if (account.renewsAtEpoch === null) continue
        assert.equal(now + (account.renewsInSeconds ?? 0), account.renewsAtEpoch, account.id)
      }
      const { nextRefill, fullAtEpoch, fullInSeconds } = credits.pool
      assert.ok(nextRefill && fullAtEpoch !== null, scenario)
      assert.equal(now + nextRefill.refillInSeconds, nextRefill.refillAtEpoch)
      assert.equal(now + (fullInSeconds ?? 0), fullAtEpoch)
    }
  })
})
