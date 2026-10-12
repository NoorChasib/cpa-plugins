// The link to Quota Cache's page, where the API credits and renewal dates are
// edited since 0.8.0: drawn only where that page can work, a browser
// whose console session this page may present, and never in the menu bar
// app's popover, where it would load Quota Cache's page in place of this one
// and stop the readout. Driven with access.ts's own rules, so "may present"
// means exactly what it means for a read.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { describe, test } from "node:test"

import { doors, type Env, latchConsole, rootOf, type StorageLike, TOKEN_KEY, usableConsoleKey } from "../src/lib/access.ts"
import { QUOTA_CACHE_PAGE, quotaCacheAnchor, quotaCacheEditURL } from "../src/lib/quotaCacheLink.ts"

class MemoryStorage implements StorageLike {
  values = new Map<string, string>()
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
}

const ORIGIN = "http://cpa.test:8317"
const APP = "/v0/resource/plugins/quota-glance/app"

function makeEnv(pathname = APP): Env & { storage: MemoryStorage } {
  return {
    storage: new MemoryStorage(),
    origin: ORIGIN,
    host: new URL(ORIGIN).host,
    pathname,
    userAgent: "Mozilla/5.0 (test) QuotaGlance/1",
    now: () => 1_800_000_000,
  }
}

/** A remembered console session, in the plain-JSON form the console's store also reads. */
function remember(env: Env & { storage: MemoryStorage }, apiBase = env.origin): void {
  env.storage.values.set(
    "cli-proxy-auth",
    JSON.stringify({ state: { apiBase, managementKey: "mgmt-key-1", rememberPassword: true }, version: 0 }),
  )
}

/** access.ts's rules, and a bridge that is there or not. */
const access = (readout: boolean) => ({ usableConsoleKey, rootOf, readout: () => readout })

describe("quotaCacheEditURL", () => {
  test("with a console session this page may present, Quota Cache's page at the card's section", () => {
    const env = makeEnv()
    remember(env)
    assert.equal(quotaCacheEditURL(access(false), env, "api-credits"), `${ORIGIN}/v0/resource/plugins/quota-cache/status#api-credits`)
    // The Accounts card's foot.
    assert.equal(quotaCacheEditURL(access(false), env, "renewals"), `${ORIGIN}${QUOTA_CACHE_PAGE}#renewals`)
  })

  test("keeps a reverse proxy's prefix, as Quota Cache's own page does", () => {
    const env = makeEnv("/proxy/v0/resource/plugins/quota-glance/app")
    remember(env, `${ORIGIN}/proxy`)
    assert.equal(
      quotaCacheEditURL(access(false), env, "api-credits"),
      `${ORIGIN}/proxy/v0/resource/plugins/quota-cache/status#api-credits`,
    )
  })

  test("none in the menu bar's popover, even with a console session", () => {
    const env = makeEnv()
    remember(env)
    assert.equal(quotaCacheEditURL(access(true), env, "api-credits"), null)
    assert.equal(quotaCacheEditURL(access(true), env, "renewals"), null)
  })

  test("none behind the dashboard password alone: a phone, a bookmark", () => {
    assert.equal(quotaCacheEditURL(access(false), makeEnv(), "api-credits"), null)
  })

  test("drawn with a dashboard password saved too: the door that reads does not decide it", () => {
    const env = makeEnv()
    remember(env)
    env.storage.values.set(TOKEN_KEY, "dashboard-password-1")
    // The page reads through the password first, and the console session is
    // still there for Quota Cache's page.
    assert.deepEqual(
      doors(env, "dashboard-password-1", null).map((door) => door.kind),
      ["token", "console"],
    )
    assert.equal(quotaCacheEditURL(access(false), env, "api-credits"), `${ORIGIN}${QUOTA_CACHE_PAGE}#api-credits`)
  })

  test("none with a console session for another CPA, or one CPA refused", () => {
    const elsewhere = makeEnv()
    remember(elsewhere, "http://other.test:8317")
    assert.equal(quotaCacheEditURL(access(false), elsewhere, "api-credits"), null)

    const refused = makeEnv()
    remember(refused)
    latchConsole(refused, "mgmt-key-1", "refused", 401)
    assert.equal(quotaCacheEditURL(access(false), refused, "api-credits"), null)
  })

  test("asks storage and nothing else: no write, so the console's session is untouched", () => {
    const env = makeEnv()
    remember(env)
    quotaCacheEditURL(access(false), env, "api-credits")
    quotaCacheEditURL(access(true), env, "renewals")
    assert.deepEqual(env.storage.writes, [])
  })
})

describe("quotaCacheAnchor", () => {
  test("a new tab, without the opener: in CPA's sidebar this page sits in a frame", () => {
    const url = `${ORIGIN}${QUOTA_CACHE_PAGE}#api-credits`
    assert.deepEqual(quotaCacheAnchor(url), {
      href: url,
      target: "_blank",
      rel: "noopener",
      title: "Opens Quota Cache's page in a new tab",
    })
  })

  test("no anchor at all without a URL", () => {
    assert.equal(quotaCacheAnchor(null), null)
  })
})
