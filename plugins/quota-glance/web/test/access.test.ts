// The page's ban-safety rules, driven as written.
//
// src/lib/access.ts is a leaf with no imports, so node loads it directly and
// these tests call the same readThrough and spendThrough that client.ts and
// redeem.ts call in the browser, against a fake origin, storage and fetch.
// Each test that pins one of the invariants in the access spec (I1–I13) says
// which. I11 (the menu bar readout) and I12 (the Go plugin) live outside the
// page and are tested where they live; I13 (the dev fixture) is in
// fixture-route.test.ts.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { after, describe, test } from "node:test"

import {
  type Access,
  base64url,
  classifyRefusal,
  ConsoleUnansweredError,
  consoleLatch,
  createPressBook,
  createSerial,
  doors,
  encodeSpendHeader,
  type Env,
  fingerprint,
  fnv1a64,
  forgiveConsole,
  latchConsole,
  LATCH_KEY,
  matchesRoot,
  newPressId,
  NoSessionError,
  noSession,
  pagePrefix,
  parseBanSeconds,
  PLUGIN_ERRORS,
  readConsoleKey,
  readThrough,
  refetchInterval,
  rereadAfter,
  type RequestLike,
  type ResponseLike,
  retryBlocked,
  SPEND_HEADER,
  spendThrough,
  type StorageLike,
  TOKEN_KEY,
  usableConsoleKey,
  validKey,
  CONSOLE_KEYS,
} from "../src/lib/access.ts"

// ---------------------------------------------------------------------------
// Fakes

class MemoryStorage implements StorageLike {
  values = new Map<string, string>()
  /** Every key this page wrote or removed, in order. */
  writes: string[] = []
  getItem(key: string): string | null {
    return this.values.get(key) ?? null
  }
  setItem(key: string, value: string): void {
    this.writes.push(key)
    this.values.set(key, value)
  }
  removeItem(key: string): void {
    this.writes.push(key)
    this.values.delete(key)
  }
  /** What the console, or a test, puts there: not counted as the page's write. */
  seed(key: string, value: string): void {
    this.values.set(key, value)
  }
}

const UA = "Mozilla/5.0 (test) QuotaGlance/1"
const ORIGIN = "http://cpa.test:8317"
const APP = "/v0/resource/plugins/quota-glance/app"

type TestEnv = Env & { storage: MemoryStorage; clock: { now: number } }

function makeEnv(overrides: { origin?: string; pathname?: string; userAgent?: string } = {}): TestEnv {
  const origin = overrides.origin ?? ORIGIN
  const clock = { now: 1_800_000_000 }
  return {
    storage: new MemoryStorage(),
    origin,
    host: new URL(origin).host,
    pathname: overrides.pathname ?? APP,
    userAgent: overrides.userAgent ?? UA,
    now: () => clock.now,
    clock,
  }
}

/** The console's own codec (CPAMC src/utils/encryption.ts), written independently of the page's. */
function obfuscate(text: string, host: string, userAgent: string): string {
  const key = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${host}|${userAgent}`)
  const bytes = new TextEncoder().encode(text)
  let binary = ""
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]! ^ key[i % key.length]!)
  return `enc::v1::${btoa(binary)}`
}

/** A remembered console session, as the console's zustand store persists it. */
function remember(
  env: TestEnv,
  state: Record<string, unknown> = {},
  options: { version?: unknown; obfuscated?: boolean; userAgent?: string } = {},
): void {
  const record = JSON.stringify({
    state: { apiBase: env.origin, managementKey: "mgmt-key-1", rememberPassword: true, ...state },
    version: "version" in options ? options.version : 0,
  })
  env.storage.seed(
    "cli-proxy-auth",
    options.obfuscated === false ? record : obfuscate(record, env.host, options.userAgent ?? env.userAgent),
  )
}

class TokenSlot {
  saved: string | null
  refusedValue: string | null = null
  constructor(saved: string | null) {
    this.saved = saved
  }
  read = (): string | null => this.saved
  refused = (): string | null => this.refusedValue
  refuse = (value: string): void => {
    this.refusedValue = value
  }
}

type Call = { url: string; init: RequestLike }
type Handler = (url: string, init: RequestLike) => ResponseLike | Promise<ResponseLike>

const json = (status: number, body: unknown, headers: Record<string, string> = {}): ResponseLike =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } })
const text = (status: number, body: string): ResponseLike =>
  new Response(body, { status, headers: { "Content-Type": "text/html" } })

const isManagement = (url: string) => url.includes("/v0/management/")

/**
 * Every management request any test below made without a key the strict
 * reader accepts. Collected rather than thrown, because spendThrough reports a
 * fetch that throws as a lost answer, which would hide the assertion.
 */
const keyless: string[] = []

// I1, across every test in this file.
after(() => assert.deepEqual(keyless, [], "I1: management requests without a usable key"))

/** A fetch that records every call, and every management call made without a key. */
function makeFetch(handler: Handler) {
  const calls: Call[] = []
  const fetch = async (url: string, init: RequestLike): Promise<ResponseLike> => {
    calls.push({ url, init })
    if (isManagement(url)) {
      const header = init.headers.Authorization ?? ""
      if (!header.startsWith("Bearer ") || !validKey(header.slice("Bearer ".length))) keyless.push(url)
    }
    return handler(url, init)
  }
  return {
    fetch,
    calls,
    management: () => calls.filter((call) => isManagement(call.url)).length,
    resource: () => calls.filter((call) => !isManagement(call.url)).length,
  }
}

function makeAccess(
  env: TestEnv,
  fetch: Access["fetch"],
  token: TokenSlot = new TokenSlot(null),
  serial = createSerial(),
): Access & { token: TokenSlot } {
  return {
    env,
    fetch,
    serial,
    token,
    presses: createPressBook((bytes) => crypto.getRandomValues(bytes)),
    unanswered: new Set<string>(),
  }
}

const SUMMARY = {
  tokenURL: "/v0/resource/plugins/quota-glance/summary",
  consoleURL: "/v0/management/plugins/quota-glance/summary",
}
const summaryTarget = (door: { kind: string }) => ({
  url: door.kind === "token" ? SUMMARY.tokenURL : SUMMARY.consoleURL,
  headers: { Accept: "application/json" },
})
const read = (access: Access, signal?: AbortSignal) => readThrough(access, summaryTarget, signal)

const SPEND = {
  tokenURL: "/v0/resource/plugins/quota-glance/spend",
  consoleURL: "/v0/management/plugins/quota-glance/redeem",
}
const spend = (access: Access, credentialId = "codex-a.json") => spendThrough(access, credentialId, SPEND)

/** Settles every promise already queued, and the ones they queue. */
const drain = () => new Promise<void>((resolve) => setImmediate(resolve))

const latchOf = (env: TestEnv) => {
  const raw = env.storage.getItem(LATCH_KEY)
  return raw === null ? null : (JSON.parse(raw) as { reason: string; status: number; until: number | null; fp: string })
}

// ---------------------------------------------------------------------------
// §2.2 The strict console reader

describe("strict console reader", () => {
  test("reads a remembered, obfuscated session for this CPA", () => {
    const env = makeEnv()
    remember(env)
    assert.equal(readConsoleKey(env), "mgmt-key-1")
  })

  test("reads a plain-JSON session too, which is what the dev seed writes", () => {
    const env = makeEnv()
    remember(env, {}, { obfuscated: false })
    assert.equal(readConsoleKey(env), "mgmt-key-1")
  })

  test("no entry at all is no session", () => {
    assert.equal(readConsoleKey(makeEnv()), null)
  })

  test("a logout record is no session", () => {
    const env = makeEnv()
    remember(env, { managementKey: "" })
    assert.equal(readConsoleKey(env), null)
  })

  test("Remember password off is no session, even with a key beside it", () => {
    const env = makeEnv()
    remember(env, { rememberPassword: false })
    assert.equal(readConsoleKey(env), null)
  })

  test("a session the console marks signed out is no session", () => {
    const env = makeEnv()
    remember(env, { isAuthenticated: false })
    assert.equal(readConsoleKey(env), null)
  })

  test("only version 0 of the store is understood", () => {
    for (const version of [1, undefined, "0"]) {
      const env = makeEnv()
      remember(env, {}, { version })
      assert.equal(readConsoleKey(env), null, `version ${String(version)}`)
    }
  })

  test("a session for another CPA is never this one's", () => {
    for (const apiBase of ["http://other.test:8317", "https://cpa.test:8317", "http://cpa.test:9999", "http://cpa.test:8317/cpa", ""]) {
      const env = makeEnv()
      remember(env, { apiBase })
      assert.equal(readConsoleKey(env), null, apiBase)
    }
  })

  test("the console's management suffixes and trailing slashes are normalised away", () => {
    for (const apiBase of [
      `${ORIGIN}/`,
      `${ORIGIN}/v8/management`,
      `${ORIGIN}/v0/management/`,
      `${ORIGIN}/V8/Management`,
      `  ${ORIGIN}  `,
    ]) {
      const env = makeEnv()
      remember(env, { apiBase })
      assert.equal(readConsoleKey(env), "mgmt-key-1", apiBase)
    }
  })

  test("a base with credentials, a query or a fragment is refused", () => {
    for (const apiBase of [`http://user:pw@cpa.test:8317`, `${ORIGIN}/?x=1`, `${ORIGIN}/#top`, "javascript:alert(1)", `${ORIGIN}/${"a".repeat(2050)}`]) {
      const env = makeEnv()
      remember(env, { apiBase })
      assert.equal(readConsoleKey(env), null, apiBase.slice(0, 60))
    }
  })

  test("behind a reverse-proxy prefix, the base must carry the same prefix", () => {
    const env = makeEnv({ pathname: `/cpa${APP}` })
    assert.equal(pagePrefix(env.pathname), "/cpa")
    remember(env, { apiBase: `${ORIGIN}/cpa/v0/management` })
    assert.equal(readConsoleKey(env), "mgmt-key-1")
    remember(env, { apiBase: ORIGIN })
    assert.equal(readConsoleKey(env), null)
  })

  test("pagePrefix reads either tree, and is empty on the dev server", () => {
    assert.equal(pagePrefix("/v0/resource/plugins/quota-glance/app"), "")
    assert.equal(pagePrefix("/edge/v0/management/plugins/quota-glance/app/"), "/edge")
    assert.equal(pagePrefix("/"), "")
  })

  test("a blob obfuscated under another user agent fails closed", () => {
    const env = makeEnv()
    remember(env, {}, { userAgent: "Mozilla/5.0 (another browser)" })
    assert.equal(readConsoleKey(env), null)
  })

  test("garbage in the modern entry is no session, never a fall back to legacy", () => {
    for (const raw of ["enc::v1::%%%", "enc::v1::AAAA", "{not json", "null", "[]", '"a string"']) {
      const env = makeEnv()
      env.storage.seed("cli-proxy-auth", raw)
      env.storage.seed("isLoggedIn", "true")
      env.storage.seed("apiBase", ORIGIN)
      env.storage.seed("managementKey", "legacy-key")
      assert.equal(readConsoleKey(env), null, raw)
    }
  })

  test("a legacy key beside a modern entry is ignored, after a logout above all", () => {
    const env = makeEnv()
    remember(env, { managementKey: "" })
    env.storage.seed("isLoggedIn", "true")
    env.storage.seed("apiBase", ORIGIN)
    env.storage.seed("managementKey", "legacy-key")
    assert.equal(readConsoleKey(env), null)
  })

  test("legacy entries are read only while isLoggedIn is 'true'", () => {
    const env = makeEnv()
    env.storage.seed("apiBase", ORIGIN)
    env.storage.seed("managementKey", "legacy-key")
    assert.equal(readConsoleKey(env), null)
    env.storage.seed("isLoggedIn", "false")
    assert.equal(readConsoleKey(env), null)
    env.storage.seed("isLoggedIn", "true")
    assert.equal(readConsoleKey(env), "legacy-key")
  })

  test("legacy entries decode loosely: obfuscated, JSON-quoted, or apiUrl in place of apiBase", () => {
    const env = makeEnv()
    env.storage.seed("isLoggedIn", "true")
    env.storage.seed("apiUrl", obfuscate(JSON.stringify(`${ORIGIN}/v0/management`), env.host, UA))
    env.storage.seed("managementKey", obfuscate(JSON.stringify("legacy-key"), env.host, UA))
    assert.equal(readConsoleKey(env), "legacy-key")
    env.storage.seed("apiUrl", "http://elsewhere.test/v0/management")
    assert.equal(readConsoleKey(env), null)
  })

  test("keys that cannot travel as a header exactly as stored are refused", () => {
    for (const key of [" padded", "padded ", "tab\tinside", "new\nline", "nul\u0000", "del\u007f", "ключ", "k".repeat(4097), ""]) {
      assert.equal(validKey(key), false, JSON.stringify(key).slice(0, 40))
      const env = makeEnv()
      remember(env, { managementKey: key })
      assert.equal(readConsoleKey(env), null)
    }
    assert.equal(validKey("k".repeat(4096)), true)
    assert.equal(validKey("café-latin1"), true)
    assert.equal(validKey(42), false)
  })

  test("any storage value past 32 KB is no session", () => {
    const env = makeEnv()
    remember(env, { padding: "x".repeat(33_000) }, { obfuscated: false })
    assert.equal(readConsoleKey(env), null)
    const legacy = makeEnv()
    legacy.storage.seed("isLoggedIn", "true")
    legacy.storage.seed("apiBase", ORIGIN)
    legacy.storage.seed("managementKey", "k".repeat(32_769))
    assert.equal(readConsoleKey(legacy), null)
  })

  test("storage that throws is no session", () => {
    const env = makeEnv()
    env.storage.getItem = () => {
      throw new Error("SecurityError")
    }
    assert.equal(readConsoleKey(env), null)
    assert.deepEqual(doors(env, null, null), [])
  })

  test("matchesRoot refuses a page that is not on http(s)", () => {
    const env = makeEnv({ origin: "file://" })
    assert.equal(matchesRoot("file://", env), false)
  })
})

// ---------------------------------------------------------------------------
// §2.3 Latches

describe("console latch", () => {
  test("FNV-1a 64 matches its published vectors", () => {
    assert.equal(fnv1a64(""), "cbf29ce484222325")
    assert.equal(fnv1a64("a"), "af63dc4c8601ec8c")
    assert.equal(fnv1a64("foobar"), "85944171f73967e8")
  })

  test("the fingerprint binds the key to this CPA's origin and prefix", () => {
    const here = makeEnv()
    const prefixed = makeEnv({ pathname: `/cpa${APP}` })
    const elsewhere = makeEnv({ origin: "http://other.test" })
    assert.notEqual(fingerprint(here, "k"), fingerprint(prefixed, "k"))
    assert.notEqual(fingerprint(here, "k"), fingerprint(elsewhere, "k"))
    assert.notEqual(fingerprint(here, "k"), fingerprint(here, "k2"))
    assert.equal(fingerprint(here, "k"), fnv1a64(`${ORIGIN}\nk`))
  })

  test("a latch holds back exactly the key it was written for", () => {
    const env = makeEnv()
    remember(env)
    latchConsole(env, "mgmt-key-1", "refused", 401)
    assert.equal(usableConsoleKey(env), null)
    assert.equal(consoleLatch(env)?.reason, "refused")
  })

  test("pruned when the console signs in again with another key", () => {
    const env = makeEnv()
    remember(env)
    latchConsole(env, "mgmt-key-1", "refused", 401)
    remember(env, { managementKey: "mgmt-key-2" })
    assert.equal(consoleLatch(env), null)
    assert.equal(env.storage.getItem(LATCH_KEY), null, "the stale latch is deleted, not just ignored")
    assert.equal(usableConsoleKey(env), "mgmt-key-2")
  })

  test("pruned when the console's key is gone", () => {
    const env = makeEnv()
    remember(env)
    latchConsole(env, "mgmt-key-1", "refused", 401)
    remember(env, { managementKey: "" })
    assert.equal(consoleLatch(env), null)
    assert.equal(env.storage.getItem(LATCH_KEY), null)
  })

  test("a latch this page cannot read holds back whatever key is there", () => {
    for (const raw of ["{", "[]", JSON.stringify({ v: 2, fp: "x" }), JSON.stringify({ v: 1, fp: 3, reason: "refused" })]) {
      const env = makeEnv()
      remember(env)
      env.storage.seed(LATCH_KEY, raw)
      assert.equal(usableConsoleKey(env), null, raw)
      assert.equal(consoleLatch(env)?.reason, "other")
    }
  })

  test("a ban records when CPA said it lifts; other reasons record none", () => {
    const env = makeEnv()
    remember(env)
    const ban = latchConsole(env, "mgmt-key-1", "banned", 403, 1780)
    assert.equal(ban.until, env.clock.now + 1780)
    assert.equal(latchConsole(env, "mgmt-key-1", "banned", 403).until, env.clock.now + 1800)
    assert.equal(latchConsole(env, "mgmt-key-1", "remote", 403).until, null)
  })

  test("I8: Try it again is refused while CPA's ban has not lifted, and allowed after", () => {
    const env = makeEnv()
    remember(env)
    latchConsole(env, "mgmt-key-1", "banned", 403, 600)
    assert.equal(retryBlocked(consoleLatch(env), env.now()), true)
    assert.equal(forgiveConsole(env), false)
    assert.equal(usableConsoleKey(env), null)
    env.clock.now += 601
    assert.equal(retryBlocked(consoleLatch(env), env.now()), false)
    assert.equal(forgiveConsole(env), true)
    assert.equal(usableConsoleKey(env), "mgmt-key-1")
  })

  test("I8: Try it again lifts any other latch at once", () => {
    for (const reason of ["refused", "remote", "off", "other"] as const) {
      const env = makeEnv()
      remember(env)
      latchConsole(env, "mgmt-key-1", reason, 401)
      assert.equal(forgiveConsole(env), true)
      assert.equal(usableConsoleKey(env), "mgmt-key-1")
    }
  })

  test("parseBanSeconds reads CPA's Go durations, and nothing else", () => {
    const say = (d: string) => `IP banned due to too many failed attempts. Try again in ${d}`
    assert.equal(parseBanSeconds(say("29m59s")), 1799)
    assert.equal(parseBanSeconds(say("29m40s")), 1780)
    assert.equal(parseBanSeconds(say("1h0m0s")), 3600)
    assert.equal(parseBanSeconds(say("45s")), 45)
    assert.equal(parseBanSeconds(say("0s")), 0)
    assert.equal(parseBanSeconds(say("1.5s")), 2)
    assert.equal(parseBanSeconds(say("9999h0m0s")), 86_400, "capped at a day")
    for (const garbage of [say(""), say("soon"), say("29 minutes"), "IP banned", "", say("-5s")]) {
      assert.equal(parseBanSeconds(garbage), null, garbage)
    }
  })
})

describe("token latch", () => {
  test("a refused token closes the token door until it changes", () => {
    const env = makeEnv()
    assert.deepEqual(doors(env, "tok", "tok"), [])
    assert.deepEqual(doors(env, "tok-2", "tok"), [{ kind: "token", token: "tok-2" }])
  })
})

// ---------------------------------------------------------------------------
// §2.6 The classifier

describe("classifyRefusal", () => {
  const cases: [string, Parameters<typeof classifyRefusal>, string | null, string | null][] = [
    // door, status, error → code, latch
    ["CPA, no key", ["console", 401, "missing management key"], "console_refused", "refused"],
    ["CPA, wrong key", ["console", 401, "invalid management key"], "console_refused", "refused"],
    ["CPA, banned", ["console", 403, "IP banned due to too many failed attempts. Try again in 29m59s"], "ip_banned", "banned"],
    ["CPA, remote off", ["console", 403, "remote management disabled"], "remote_disabled", "remote"],
    ["CPA, no key set", ["console", 403, "remote management key not set"], "management_off", "off"],
    ["a proxy's 401 page", ["console", 401, null], "refused_other", "other"],
    ["a proxy's 403 page", ["console", 403, null], "refused_other", "other"],
    ["an unknown 401 string", ["console", 401, "who are you"], "refused_other", "other"],
    ["an unknown 403 string", ["console", 403, "forbidden"], "refused_other", "other"],
    ["the plugin, wrong token", ["token", 401, null], "token_refused", "token"],
    ["the plugin's limiter", ["token", 429, null], "token_refused", "token"],
    ["the plugin's fetch-metadata gate", ["token", 403, "cross_site"], "cross_site", null],
    ["a proxy's 403 on the token door", ["token", 403, null], "refused_other", "token"],
    ["another 403 on the token door", ["token", 403, "forbidden"], "refused_other", "token"],
    ["early data", ["token", 425, "too_early"], "too_early", null],
    ["redeem switched off, token door", ["token", 404, "not_found"], "not_found", null],
    ["redeem switched off, console door", ["console", 404, "not_found"], "not_found", null],
    ["CPA never dispatched it, token door", ["token", 404, null], "route_missing", null],
    ["CPA never dispatched it, console door", ["console", 404, null], "route_missing", null],
  ]
  for (const [what, args, code, latch] of cases) {
    test(what, () => {
      const result = classifyRefusal(...args)
      assert.equal(result?.code ?? null, code)
      assert.equal(result?.latch ?? null, latch)
    })
  }

  test("a ban carries CPA's duration, or thirty minutes when it does not say", () => {
    assert.equal(classifyRefusal("console", 403, "IP banned due to too many failed attempts. Try again in 29m59s")?.banSeconds, 1799)
    assert.equal(classifyRefusal("console", 403, "IP banned")?.banSeconds, 1800)
  })

  test("anything that is not a refusal is not classified", () => {
    for (const [door, status] of [["token", 200], ["token", 400], ["token", 409], ["token", 502], ["console", 429], ["console", 409], ["console", 500]] as const) {
      assert.equal(classifyRefusal(door, status, "x"), null, `${door} ${status}`)
    }
  })
})

// ---------------------------------------------------------------------------
// §2.1 Doors

describe("doors", () => {
  test("the token before the console", () => {
    const env = makeEnv()
    remember(env)
    assert.deepEqual(doors(env, "tok", null), [
      { kind: "token", token: "tok" },
      { kind: "console", key: "mgmt-key-1" },
    ])
  })

  test("a refused token is skipped", () => {
    const env = makeEnv()
    remember(env)
    assert.deepEqual(doors(env, "tok", "tok"), [{ kind: "console", key: "mgmt-key-1" }])
  })

  test("a latched console key is skipped", () => {
    const env = makeEnv()
    remember(env)
    latchConsole(env, "mgmt-key-1", "refused", 401)
    assert.deepEqual(doors(env, "tok", null), [{ kind: "token", token: "tok" }])
  })

  test("nothing usable is no doors", () => {
    assert.deepEqual(doors(makeEnv(), null, null), [])
  })

  test("a token that cannot travel as a header is no door", () => {
    assert.deepEqual(doors(makeEnv(), "ключ", null), [])
  })

  test("I7: the poll runs only while a door is open", () => {
    assert.equal(refetchInterval([]), false)
    assert.equal(refetchInterval([{ kind: "token", token: "t" }]), 60_000)
  })

  test("NoSessionError names the token first, then the console's latch", () => {
    const env = makeEnv()
    remember(env)
    latchConsole(env, "mgmt-key-1", "banned", 403, 60)
    const token = new TokenSlot("tok")
    token.refuse("tok")
    assert.deepEqual(pick(noSession({ env, token })), { hadCredential: true, reason: "token" })
    assert.deepEqual(pick(noSession({ env, token: new TokenSlot(null) })), { hadCredential: true, reason: "banned" })
    assert.deepEqual(pick(noSession({ env: makeEnv(), token: new TokenSlot(null) })), { hadCredential: false, reason: null })
  })
})

const pick = (error: NoSessionError) => ({ hadCredential: error.hadCredential, reason: error.reason })

// ---------------------------------------------------------------------------
// §2.4 Reads, and the invariants they carry

describe("reads", () => {
  test("I7: with no door, no request of any kind, however often it is asked", async () => {
    const env = makeEnv()
    const net = makeFetch(() => json(200, {}))
    const access = makeAccess(env, net.fetch)
    for (let tick = 0; tick < 100; tick++) {
      await assert.rejects(read(access), (error: unknown) => error instanceof NoSessionError && !error.hadCredential)
    }
    assert.equal(net.calls.length, 0)
  })

  test("I3: with the token open, no management request over 100 polls", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(200, { schemaVersion: 1 }))
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    for (let tick = 0; tick < 100; tick++) await read(access)
    assert.equal(net.management(), 0)
    assert.equal(net.resource(), 100)
    assert.equal(net.calls[0]?.init.headers.Authorization, "Bearer tok")
  })

  test("I4: one refusal, then 10 ticks, 3 focus refetches and 2 retries send nothing more", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(401, { error: "invalid management key" }))
    const access = makeAccess(env, net.fetch)
    await assert.rejects(read(access), (error: unknown) => error instanceof NoSessionError && error.reason === "refused")
    for (let i = 0; i < 15; i++) {
      await assert.rejects(read(access), (error: unknown) => error instanceof NoSessionError && error.hadCredential)
    }
    assert.equal(net.management(), 1)
    assert.equal(latchOf(env)?.reason, "refused")
  })

  test("I5: three concurrent triggers with a stale key send one request", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(async () => {
      await drain()
      return json(401, { error: "invalid management key" })
    })
    const access = makeAccess(env, net.fetch)
    const results = await Promise.allSettled([read(access), read(access), read(access), spend(access)])
    assert.equal(net.management(), 1)
    assert.deepEqual(
      results.slice(0, 3).map((one) => one.status),
      ["rejected", "rejected", "rejected"],
    )
    const press = results[3]
    assert.equal(press?.status, "fulfilled")
    assert.deepEqual(press?.status === "fulfilled" && press.value, { sent: false, code: "console_refused", status: 401 })
  })

  test("I4: the latch is in storage before the refusal's body has even been read", async () => {
    const env = makeEnv()
    remember(env)
    let release = () => {}
    const held = new Promise<void>((resolve) => (release = resolve))
    const slow: ResponseLike = {
      status: 401,
      ok: false,
      headers: new Headers(),
      text: async () => {
        await held
        return JSON.stringify({ error: "invalid management key" })
      },
      json: async () => ({}),
    }
    const net = makeFetch(() => slow)
    const first = makeAccess(env, net.fetch)
    const pending = read(first)
    await drain()
    assert.equal(net.management(), 1)
    assert.notEqual(latchOf(env), null, "latched on the status alone")
    // Another document on the origin, sharing storage, sees it at once.
    const other = makeAccess(env, net.fetch)
    await assert.rejects(read(other), NoSessionError)
    assert.deepEqual(await spend(other), { sent: false, code: "console_refused", status: 401 })
    release()
    await assert.rejects(pending, NoSessionError)
    assert.equal(net.management(), 1)
    assert.equal(latchOf(env)?.reason, "refused")
  })

  test("I4: a reload, or another tab, presents a latched key zero times", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(401, { error: "missing management key" }))
    await assert.rejects(read(makeAccess(env, net.fetch)))
    for (let doc = 0; doc < 5; doc++) {
      const fresh = makeAccess(env, net.fetch)
      await assert.rejects(read(fresh), NoSessionError)
      assert.deepEqual(await spend(fresh), { sent: false, code: "console_refused", status: 401 })
    }
    assert.equal(net.management(), 1)
  })

  test("each of CPA's refusals latches with its own reason", async () => {
    const cases: [ResponseLike, string, number][] = [
      [json(401, { error: "missing management key" }), "refused", 401],
      [json(401, { error: "invalid management key" }), "refused", 401],
      [json(403, { error: "IP banned due to too many failed attempts. Try again in 29m40s" }), "banned", 403],
      [json(403, { error: "remote management disabled" }), "remote", 403],
      [json(403, { error: "remote management key not set" }), "off", 403],
      [text(401, "<html>401 Authorization Required</html>"), "other", 401],
      [text(403, "<html>Forbidden</html>"), "other", 403],
    ]
    for (const [response, reason, status] of cases) {
      const env = makeEnv()
      remember(env)
      const net = makeFetch(() => response)
      await assert.rejects(read(makeAccess(env, net.fetch)), (error: unknown) => error instanceof NoSessionError && error.reason === reason)
      const latch = latchOf(env)
      assert.equal(latch?.reason, reason)
      assert.equal(latch?.status, status)
      assert.equal(latch?.until, reason === "banned" ? env.clock.now + 1780 : null)
    }
  })

  test("I5: a console read is never cancelled by the query's signal", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(200, {}))
    const cancelled = AbortSignal.abort()
    await read(makeAccess(env, net.fetch), cancelled)
    assert.equal(net.calls[0]?.init.signal.aborted, false)
  })

  test("a token read does carry the query's signal, since abandoning it costs nothing", async () => {
    const env = makeEnv()
    const net = makeFetch(() => json(200, {}))
    await read(makeAccess(env, net.fetch, new TokenSlot("tok")), AbortSignal.abort())
    assert.equal(net.calls[0]?.init.signal.aborted, true)
  })

  test("I9: a refused token is latched in memory and never presented again", async () => {
    for (const status of [401, 403, 429]) {
      const env = makeEnv()
      const net = makeFetch(() => new Response(null, { status }))
      const token = new TokenSlot("tok")
      const access = makeAccess(env, net.fetch, token)
      await assert.rejects(read(access), (error: unknown) => error instanceof NoSessionError && error.reason === "token")
      for (let tick = 0; tick < 10; tick++) await assert.rejects(read(access), NoSessionError)
      assert.equal(net.calls.length, 1, `status ${status}`)
      assert.equal(net.management(), 0)
      assert.equal(token.refused(), "tok")
    }
  })

  test("a refused token falls back to the console's key, once", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch((url) => (isManagement(url) ? json(200, { via: "console" }) : new Response(null, { status: 401 })))
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    const first = await read(access)
    assert.equal(first.door.kind, "console")
    await read(access)
    assert.equal(net.resource(), 1, "the token was not tried again")
    assert.equal(net.management(), 2)
  })

  test("a failing poll that is not a refusal leaves both doors open", async () => {
    const env = makeEnv()
    const net = makeFetch(() => text(502, "bad gateway"))
    const token = new TokenSlot("tok")
    const result = await read(makeAccess(env, net.fetch, token))
    assert.equal(result.response.status, 502)
    assert.equal(token.refused(), null)
  })

  test("I2: a key the strict reader refuses is never presented", async () => {
    const env = makeEnv()
    remember(env, { managementKey: "" })
    env.storage.seed("isLoggedIn", "true")
    env.storage.seed("apiBase", ORIGIN)
    env.storage.seed("managementKey", "legacy-after-logout")
    const net = makeFetch(() => json(200, {}))
    for (let tick = 0; tick < 20; tick++) await assert.rejects(read(makeAccess(env, net.fetch)), NoSessionError)
    remember(env, { apiBase: "http://another-cpa.test" })
    for (let tick = 0; tick < 20; tick++) await assert.rejects(read(makeAccess(env, net.fetch)), NoSessionError)
    assert.equal(net.calls.length, 0)
  })

  test("I8: one Try it again presents the key once, and a second refusal latches it again", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(401, { error: "invalid management key" }))
    const access = makeAccess(env, net.fetch)
    await assert.rejects(read(access))
    assert.equal(forgiveConsole(env), true)
    await Promise.allSettled([read(access), read(access), read(access)])
    await Promise.allSettled([read(access), read(access)])
    assert.equal(net.management(), 2)
    assert.equal(latchOf(env)?.reason, "refused")
  })

  test("I8: during a ban, Try it again sends nothing", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(403, { error: "IP banned due to too many failed attempts. Try again in 29m40s" }))
    const access = makeAccess(env, net.fetch)
    await assert.rejects(read(access))
    assert.equal(forgiveConsole(env), false)
    await assert.rejects(read(access), NoSessionError)
    assert.equal(net.management(), 1)
  })
})

// ---------------------------------------------------------------------------
// Other documents on the origin

describe("other documents", () => {
  /** Documents sharing one origin: one storage, one fetch, and the origin's queue. */
  const documents = (env: TestEnv, fetch: Access["fetch"], count: number) => {
    const origin = createSerial()
    return Array.from({ length: count }, () => makeAccess(env, fetch, new TokenSlot(null), createSerial(origin)))
  }
  const slowRefusal = () =>
    makeFetch(async () => {
      // Longer than one turn of the event loop, so every document has read
      // its door before the first refusal could be latched without a queue.
      await drain()
      await drain()
      return json(401, { error: "invalid management key" })
    })

  test("I5: five documents loading at once with a stale key send one request between them", async () => {
    const env = makeEnv()
    remember(env)
    const net = slowRefusal()
    const results = await Promise.allSettled(documents(env, net.fetch, 5).map((doc) => read(doc)))
    assert.equal(net.management(), 1)
    assert.ok(results.every((one) => one.status === "rejected"))
  })

  test("I8: one Try it again, with four other documents asking at the same moment, presents the key once", async () => {
    const env = makeEnv()
    remember(env)
    const net = slowRefusal()
    const docs = documents(env, net.fetch, 5)
    await assert.rejects(read(docs[0]!))
    assert.equal(forgiveConsole(env), true)
    await Promise.allSettled(docs.map((doc) => read(doc)))
    assert.equal(net.management(), 2)
    assert.equal(latchOf(env)?.reason, "refused")
  })

  test("I8: a latch lifted or a password removed elsewhere is not a reason to ask again here", () => {
    assert.equal(rereadAfter(LATCH_KEY, null), false)
    assert.equal(rereadAfter(TOKEN_KEY, null), false)
    // Everything else that can open or close a door still is.
    assert.equal(rereadAfter(LATCH_KEY, JSON.stringify({ v: 1 })), true)
    assert.equal(rereadAfter(TOKEN_KEY, "tok"), true)
    for (const key of CONSOLE_KEYS) {
      assert.equal(rereadAfter(key, "x"), true, key)
      assert.equal(rereadAfter(key, null), true, key)
    }
    assert.equal(rereadAfter(null, null), true, "the whole of storage cleared")
    assert.equal(rereadAfter("quota-glance.collapsed", "[]"), false)
    // Which cards are open, and the old folded list being removed on load.
    assert.equal(rereadAfter("quota-glance.opened", "[]"), false)
    assert.equal(rereadAfter("quota-glance.collapsed", null), false)
  })
})

// ---------------------------------------------------------------------------
// A console request with no answer

describe("no answer", () => {
  test("a console read that gets no answer holds its key back from every later read", async () => {
    for (const failure of [
      () => new TypeError("Failed to fetch"),
      () => new DOMException("The operation timed out.", "TimeoutError"),
    ]) {
      const env = makeEnv()
      remember(env)
      const net = makeFetch(() => {
        throw failure()
      })
      const access = makeAccess(env, net.fetch)
      await assert.rejects(read(access), ConsoleUnansweredError)
      // Ten interval ticks, three focus refetches, two retries.
      for (let i = 0; i < 15; i++) await assert.rejects(read(access), ConsoleUnansweredError)
      assert.equal(net.management(), 1)
      // The key may be fine, so nothing is written for other documents.
      assert.equal(env.storage.getItem(LATCH_KEY), null)
    }
  })

  test("the hold is on that key: a new console sign-in, or the password, reads at once", async () => {
    const env = makeEnv()
    remember(env)
    let turn = 0
    const net = makeFetch(() => {
      if (turn++ === 0) throw new TypeError("dropped")
      return json(200, {})
    })
    const token = new TokenSlot(null)
    const access = makeAccess(env, net.fetch, token)
    await assert.rejects(read(access), ConsoleUnansweredError)
    remember(env, { managementKey: "mgmt-key-2" })
    await read(access)
    assert.equal(net.management(), 2)
    // And the password door was never held back by any of it.
    token.saved = "tok"
    await read(access)
    assert.equal(net.resource(), 1)
  })

  test("asking again lifts the hold for one request, and a second loss holds it again", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => {
      throw new TypeError("dropped")
    })
    const access = makeAccess(env, net.fetch)
    await assert.rejects(read(access), ConsoleUnansweredError)
    access.unanswered.clear()
    await Promise.allSettled([read(access), read(access), read(access)])
    assert.equal(net.management(), 2)
  })

  test("a console press that gets no answer holds reads back too; a press is never held back", async () => {
    const env = makeEnv()
    remember(env)
    let turn = 0
    const net = makeFetch(() => {
      if (turn++ < 2) throw new TypeError("dropped")
      return json(200, { outcome: "reset" })
    })
    const access = makeAccess(env, net.fetch)
    assert.deepEqual(await spend(access), { sent: true, door: "console", answered: false })
    await assert.rejects(read(access), ConsoleUnansweredError)
    assert.equal(net.management(), 1)
    // The reader presses again: sent, despite the hold.
    assert.deepEqual(await spend(access), { sent: true, door: "console", answered: false })
    assert.equal(net.management(), 2)
    // A press CPA accepts proves the key, and reads resume.
    const accepted = await spend(access)
    assert.equal(accepted.sent && accepted.answered && accepted.ok, true)
    await read(access)
    assert.equal(net.management(), 4)
  })

  test("a token read that gets no answer holds nothing", async () => {
    const env = makeEnv()
    let turn = 0
    const net = makeFetch(() => {
      if (turn++ === 0) throw new TypeError("dropped")
      return json(200, {})
    })
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    await assert.rejects(read(access), TypeError)
    await read(access)
    assert.equal(net.resource(), 2)
    assert.equal(access.unanswered.size, 0)
  })
})

// ---------------------------------------------------------------------------
// §2.5 Writes

describe("spends", () => {
  const decodeSpend = (value: string | undefined) =>
    JSON.parse(Buffer.from(value ?? "", "base64url").toString("utf8")) as { credentialId: string; confirmed: boolean; pressId: string }
  const reset = () => json(200, { provider: "codex", outcome: "reset", windowsReset: 2, remainingCount: 1, snapshotPending: true })

  test("the token door: one GET, the press in a header, nothing in the URL", async () => {
    const env = makeEnv()
    const net = makeFetch(reset)
    const answer = await spend(makeAccess(env, net.fetch, new TokenSlot("tok")), "claude-ü@example.json")
    assert.equal(net.calls.length, 1)
    const call = net.calls[0]!
    assert.equal(call.url, SPEND.tokenURL)
    assert.equal(call.init.method, "GET")
    assert.equal(call.init.body, undefined)
    assert.equal(call.init.headers.Authorization, "Bearer tok")
    assert.deepEqual(
      { mode: call.init.mode, credentials: call.init.credentials, cache: call.init.cache, redirect: call.init.redirect },
      { mode: "same-origin", credentials: "same-origin", cache: "no-store", redirect: "error" },
    )
    const body = decodeSpend(call.init.headers[SPEND_HEADER])
    assert.equal(body.credentialId, "claude-ü@example.json")
    assert.equal(body.confirmed, true)
    assert.match(body.pressId, /^[A-Za-z0-9_-]{22}$/)
    assert.equal(answer.sent && answer.answered && answer.ok, true)
  })

  test("the console door: one POST, the same press as the body", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(reset)
    await spend(makeAccess(env, net.fetch))
    const call = net.calls[0]!
    assert.equal(call.url, SPEND.consoleURL)
    assert.equal(call.init.method, "POST")
    assert.equal(call.init.headers["Content-Type"], "application/json")
    assert.equal(call.init.redirect, "error")
    assert.equal(call.init.mode, "same-origin")
    const body = JSON.parse(call.init.body ?? "{}") as { confirmed: unknown; pressId: unknown }
    assert.equal(body.confirmed, true)
    assert.match(String(body.pressId), /^[A-Za-z0-9_-]{22}$/)
  })

  test("I3: with the token open, a press never touches the management API", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(reset)
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    for (let i = 0; i < 10; i++) await spend(access, `codex-${i}.json`)
    assert.equal(net.management(), 0)
  })

  test("I6: a refused press is one request and never falls through to the other door", async () => {
    for (const response of [
      () => new Response(null, { status: 401 }),
      () => new Response(null, { status: 429 }),
      () => json(403, { error: "cross_site" }),
      () => json(425, { error: "too_early" }),
      () => new Response(null, { status: 404 }),
      () => text(502, "<html>bad gateway</html>"),
      () => json(502, { error: "outcome_unknown", retryUntilEpoch: 1 }),
    ]) {
      const env = makeEnv()
      remember(env)
      const net = makeFetch(response)
      const token = new TokenSlot("tok")
      await spend(makeAccess(env, net.fetch, token))
      assert.equal(net.calls.length, 1)
      assert.equal(net.management(), 0, "the console key behind it was not tried")
    }
  })

  test("I6: a failed fetch is reported, not retried", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => {
      throw new TypeError("network")
    })
    assert.deepEqual(await spend(makeAccess(env, net.fetch, new TokenSlot("tok"))), { sent: true, door: "token", answered: false })
    assert.equal(net.calls.length, 1)
  })

  test("I9: the token door's refusals latch the token, except the plugin's own gate and a 404", async () => {
    const cases: [() => ResponseLike, string, boolean][] = [
      [() => new Response(null, { status: 401 }), "token_refused", true],
      [() => new Response(null, { status: 429 }), "token_refused", true],
      [() => text(403, "proxy says no"), "refused_other", true],
      [() => json(403, { error: "cross_site" }), "cross_site", false],
      [() => json(425, { error: "too_early" }), "too_early", false],
      [() => json(404, { error: "not_found" }), "not_found", false],
      [() => new Response(null, { status: 404 }), "route_missing", false],
    ]
    for (const [response, code, latched] of cases) {
      const env = makeEnv()
      const token = new TokenSlot("tok")
      const answer = await spend(makeAccess(env, makeFetch(response).fetch, token))
      assert.equal(answer.sent && answer.answered ? answer.refusal?.code : "none", code)
      assert.equal(token.refused() === "tok", latched, code)
      assert.equal(env.storage.getItem(LATCH_KEY), null, "a token refusal never touches the console latch")
    }
  })

  test("I4, I6: a console refusal latches, and the next press sends nothing", async () => {
    const env = makeEnv()
    remember(env)
    const net = makeFetch(() => json(401, { error: "invalid management key" }))
    const access = makeAccess(env, net.fetch)
    const first = await spend(access)
    assert.equal(first.sent && first.answered ? first.refusal?.code : "none", "console_refused")
    assert.deepEqual(await spend(access), { sent: false, code: "console_refused", status: 401 })
    assert.deepEqual(await spend(access, "other.json"), { sent: false, code: "console_refused", status: 401 })
    assert.equal(net.management(), 1)
  })

  test("a press with no door sends nothing", async () => {
    const net = makeFetch(reset)
    assert.deepEqual(await spend(makeAccess(makeEnv(), net.fetch)), { sent: false, code: "no_session", status: 0 })
    assert.equal(net.calls.length, 0)
  })

  test("I9: a press after the token was refused sends nothing, and says why", async () => {
    const env = makeEnv()
    const net = makeFetch(() => new Response(null, { status: 401 }))
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    await spend(access)
    assert.deepEqual(await spend(access), { sent: false, code: "token_refused", status: 401 })
    assert.equal(net.calls.length, 1)
  })

  test("the replay header is read", async () => {
    const env = makeEnv()
    const net = makeFetch(() => json(200, { outcome: "reset" }, { "X-Quota-Glance-Replayed": "1" }))
    const answer = await spend(makeAccess(env, net.fetch, new TokenSlot("tok")))
    assert.equal(answer.sent && answer.answered && answer.replayed, true)
  })

  test("a press id is reused after a lost answer, and only then", async () => {
    const env = makeEnv()
    const replies: (() => ResponseLike)[] = [
      () => {
        throw new TypeError("dropped")
      },
      () => text(504, "<html>gateway timeout</html>"),
      () => text(200, "not json"),
      () => json(200, { outcome: "reset" }, { "X-Quota-Glance-Replayed": "1" }),
      () => json(200, { outcome: "reset" }),
    ]
    let turn = 0
    const net = makeFetch(() => replies[turn++]!())
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    for (let i = 0; i < replies.length; i++) await spend(access)
    const ids = net.calls.map((call) => decodeSpend(call.init.headers[SPEND_HEADER]).pressId)
    // Lost, lost, unreadable 200 and the replayed answer all carry the first id;
    // the replay was read, so the press after it is a new one.
    assert.deepEqual(ids.slice(0, 4), [ids[0], ids[0], ids[0], ids[0]])
    assert.notEqual(ids[4], ids[0])
  })

  test("a press id is dropped after the plugin's answer, or a new press turned away", async () => {
    for (const response of [
      () => json(502, { error: "outcome_unknown" }),
      () => json(409, { error: "not_redeemable" }),
      () => new Response(null, { status: 401 }),
      () => text(400, "bad request"),
    ]) {
      const env = makeEnv()
      let turn = 0
      const net = makeFetch(() => (turn++ === 0 ? response() : json(200, { outcome: "reset" })))
      const token = new TokenSlot("tok")
      const access = makeAccess(env, net.fetch, token)
      await spend(access)
      token.refusedValue = null
      await spend(access)
      const [first, second] = net.calls.map((call) => decodeSpend(call.init.headers[SPEND_HEADER]).pressId)
      assert.notEqual(first, second)
    }
  })

  test("a gateway's own JSON at 500 or above is a lost answer, and keeps the id", async () => {
    // AWS API Gateway's integration timeout, a gateway that names its error,
    // and CPA answering for a plugin call that failed: none is the plugin's,
    // and the plugin may have finished the spend after each.
    for (const response of [
      () => json(504, { message: "Endpoint request timed out" }),
      () => json(502, { error: "Bad Gateway" }),
      () => json(500, { error: "plugin call failed" }),
    ]) {
      const env = makeEnv()
      let turn = 0
      const net = makeFetch(() => (turn++ === 0 ? response() : json(200, { outcome: "reset" }, { "X-Quota-Glance-Replayed": "1" })))
      const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
      const first = await spend(access)
      assert.equal(first.sent && first.answered && first.kept, true)
      await spend(access)
      const [one, two] = net.calls.map((call) => decodeSpend(call.init.headers[SPEND_HEADER]).pressId)
      assert.equal(two, one, "the press after it asks the ledger what the first came to")
    }
  })

  test("a repeat turned away before the plugin keeps the id it repeated", async () => {
    // The first press's answer is lost. The repeat is refused at the door —
    // the password rotated, redeeming switched off a moment, a proxy's own
    // 400 — which says nothing about the first, so the press after it, once
    // the reader has signed in again, still asks the ledger rather than
    // making a fresh claim that could spend a second reset.
    for (const refusal of [
      () => new Response(null, { status: 401 }),
      () => json(404, { error: "not_found" }),
      () => text(400, "<html>bad request</html>"),
    ]) {
      const env = makeEnv()
      let turn = 0
      const net = makeFetch(() => {
        turn++
        if (turn === 1) throw new TypeError("dropped")
        if (turn === 2) return refusal()
        return json(200, { outcome: "reset" }, { "X-Quota-Glance-Replayed": "1" })
      })
      const token = new TokenSlot("tok")
      const access = makeAccess(env, net.fetch, token)
      await spend(access)
      const repeat = await spend(access)
      assert.equal(repeat.sent && repeat.answered && repeat.kept, true)
      token.refusedValue = null
      await spend(access)
      const ids = net.calls.map((call) => decodeSpend(call.init.headers[SPEND_HEADER]).pressId)
      assert.deepEqual(ids, [ids[0], ids[0], ids[0]])
    }
  })

  test("a repeat CPA turns away on the console door keeps its id too", async () => {
    const env = makeEnv()
    remember(env)
    let turn = 0
    const net = makeFetch(() => {
      turn++
      if (turn === 1) throw new TypeError("dropped")
      if (turn === 2) return json(401, { error: "invalid management key" })
      return json(200, { outcome: "reset" }, { "X-Quota-Glance-Replayed": "1" })
    })
    const token = new TokenSlot(null)
    const access = makeAccess(env, net.fetch, token)
    await spend(access)
    const repeat = await spend(access)
    assert.equal(repeat.sent && repeat.answered && repeat.kept, true)
    // Signed in with the password instead, the reader presses again.
    token.saved = "tok"
    await spend(access)
    const ids = [
      JSON.parse(net.calls[0]!.init.body ?? "{}").pressId,
      JSON.parse(net.calls[1]!.init.body ?? "{}").pressId,
      decodeSpend(net.calls[2]!.init.headers[SPEND_HEADER]).pressId,
    ]
    assert.deepEqual(ids, [ids[0], ids[0], ids[0]])
  })

  test("every code the plugin answers with is one the page reads as the plugin's", () => {
    // api.go is the plugin's side of this; a code added there and not here
    // would read as a lost answer, and keep an id the plugin had settled.
    const source = readFileSync(new URL("../../internal/api/api.go", import.meta.url), "utf8")
    const codes = new Set([...source.matchAll(/"error":\s*"([a-z_]+)"/g)].map((match) => match[1]!))
    assert.ok(codes.size >= 10, `found ${[...codes].join(", ")}`)
    for (const code of codes) assert.equal(PLUGIN_ERRORS.has(code), true, code)
  })

  test("a press id is not reused once ten minutes have passed", async () => {
    const env = makeEnv()
    let turn = 0
    const net = makeFetch(() => {
      if (turn++ === 0) throw new TypeError("dropped")
      return json(200, { outcome: "reset" })
    })
    const access = makeAccess(env, net.fetch, new TokenSlot("tok"))
    await spend(access)
    env.clock.now += 600
    await spend(access)
    const [first, second] = net.calls.map((call) => decodeSpend(call.init.headers[SPEND_HEADER]).pressId)
    assert.notEqual(first, second)
  })

  test("press ids are per credential, and say when they are sent again", () => {
    const book = createPressBook((bytes) => crypto.getRandomValues(bytes))
    const a = book.take("a", 0)
    assert.equal(a.reused, false)
    assert.deepEqual(book.take("a", 10), { id: a.id, reused: true })
    assert.notEqual(book.take("b", 10).id, a.id)
    book.settle("a")
    assert.deepEqual(book.take("a", 20).reused, false)
  })
})

// ---------------------------------------------------------------------------
// I10, across everything above

describe("storage the page writes", () => {
  test("I10: the page writes only its own latch, never the console's entries or the token", async () => {
    const env = makeEnv()
    remember(env)
    env.storage.seed(TOKEN_KEY, "tok")
    const responses = [
      () => json(401, { error: "invalid management key" }),
      () => json(403, { error: "IP banned due to too many failed attempts. Try again in 1m0s" }),
      () => json(403, { error: "remote management disabled" }),
      () => text(403, "proxy"),
      () => json(200, {}),
    ]
    for (const response of responses) {
      const net = makeFetch(response)
      const access = makeAccess(env, net.fetch, new TokenSlot(null))
      await read(access).catch(() => undefined)
      await spend(access).catch(() => undefined)
      env.clock.now += 3600
      forgiveConsole(env)
      remember(env, { managementKey: `mgmt-key-${env.clock.now}` })
    }
    const touched = new Set(env.storage.writes)
    assert.deepEqual([...touched], [LATCH_KEY])
    for (const name of CONSOLE_KEYS) assert.equal(touched.has(name), false, name)
  })
})

// ---------------------------------------------------------------------------
// Encodings

describe("encodings", () => {
  test("base64url matches Node's for every length", () => {
    for (let length = 0; length < 40; length++) {
      const bytes = crypto.getRandomValues(new Uint8Array(length))
      assert.equal(base64url(bytes), Buffer.from(bytes).toString("base64url"))
    }
  })

  test("the spend header round-trips, non-ASCII ids included", () => {
    for (const credentialId of ["codex-a@example.com.json", "claude-ünïcødé-😀.json", ""]) {
      const body = { credentialId, confirmed: true as const, pressId: "AAAAAAAAAAAAAAAAAAAAAA" }
      const header = encodeSpendHeader(body)
      assert.match(header, /^[A-Za-z0-9_-]+$/)
      assert.deepEqual(JSON.parse(Buffer.from(header, "base64url").toString("utf8")), body)
    }
  })

  test("press ids are 22 base64url characters, and differ", () => {
    const seen = new Set<string>()
    for (let i = 0; i < 200; i++) {
      const id = newPressId((bytes) => crypto.getRandomValues(bytes))
      assert.match(id, /^[A-Za-z0-9_-]{22}$/)
      seen.add(id)
    }
    assert.equal(seen.size, 200)
  })
})
