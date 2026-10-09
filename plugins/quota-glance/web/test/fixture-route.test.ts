// The dev fixture route, driven over real HTTP.
//
// It is the only server the page meets in development, so it has to refuse
// the way CPA and the plugin refuse — the page's classifier is keyed on those
// exact answers — and it has to answer every press itself. I13: no spend or
// redeem path is ever passed on, whatever the method, so nothing pressed
// against the dev server can reach the CPA QUOTA_GLANCE_PROXY points at.
//
// node:http rather than fetch, so no client adds headers of its own: the
// fetch-metadata gate reads exactly the ones a test sends.

import assert from "node:assert/strict"
import { createServer, type IncomingMessage, request, type Server, type ServerResponse } from "node:http"
import type { AddressInfo } from "node:net"
import { after, before, describe, test } from "node:test"

import { goldenFixtureRoute, PRESS_PATHS } from "../dev/fixture-route.ts"
import { classifyRefusal, encodeSpendHeader } from "../src/lib/access.ts"
import type { RowEntry, Summary } from "../src/lib/types.ts"

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
