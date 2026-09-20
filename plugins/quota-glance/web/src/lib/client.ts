import { authHeaders, managementKey, managementPath } from "./cpa-auth"
import * as token from "./token"
import type { Summary } from "./types"

/**
 * Neither way in worked.
 *
 * `hadCredential` distinguishes "nothing to try" — a browser with no console
 * session and no saved password — from "what we had was refused", which is the
 * difference between asking the reader to sign in and telling them their
 * credential stopped working.
 */
export class NoSessionError extends Error {
  constructor(readonly hadCredential: boolean) {
    super(hadCredential ? "credentials rejected" : "no credentials")
    this.name = "NoSessionError"
  }
}

/** Where the document lives, by which credential is being spent. */
const RESOURCE_FALLBACK = "/v0/resource/plugins/quota-glance/summary"

function fallbackURL(): string {
  // The sibling of this page, so a reverse-proxy prefix survives; the literal
  // path is only for the dev server, where the page is served from /.
  const path = window.location.pathname.replace(/\/+$/, "")
  return path.endsWith("/app") ? `${path.slice(0, -"/app".length)}/summary` : RESOURCE_FALLBACK
}

/**
 * The last response, kept so a 304 has something to return.
 *
 * Keyed by route, because the two paths are authenticated differently and a
 * fall from one to the other must not hand back a document the new credential
 * was never shown.
 */
let cached: { url: string; etag: string; document: Summary } | null = null

export function resetCache(): void {
  cached = null
}

type Attempt = { url: string; headers: Record<string, string> }

/**
 * How to try to read the document, best first.
 *
 * The console's key costs the reader nothing, so it goes first whenever it is
 * there. The saved password is the fallback, and the only thing a reader
 * arriving without a console session has.
 */
function attempts(): Attempt[] {
  const list: Attempt[] = []
  if (managementKey() !== null) {
    list.push({ url: managementPath("/summary"), headers: authHeaders({ Accept: "application/json" }) })
  }
  const saved = token.read()
  if (saved) {
    list.push({
      url: fallbackURL(),
      headers: { Accept: "application/json", Authorization: ["Bearer", saved].join(" ") },
    })
  }
  return list
}

function devScenario(url: string): string {
  if (!import.meta.env.DEV) return url
  // The dev fixture route serves one scenario per state. Stripped from the
  // production bundle, where the only thing on the other end is the plugin.
  const dev = new URLSearchParams(window.location.search)
  const forwarded = new URLSearchParams()
  for (const key of ["scenario", "rebase", "expiring", "redeem"]) {
    const value = dev.get(key)
    if (value !== null) forwarded.set(key, value)
  }
  return [...forwarded].length > 0 ? `${url}?${forwarded}` : url
}

async function attempt(one: Attempt, signal?: AbortSignal): Promise<Summary | "rejected"> {
  const headers = { ...one.headers }
  if (cached && cached.url === one.url) headers["If-None-Match"] = cached.etag

  // A connection that is refused rejects on its own; one that is accepted and
  // then never answered does not. Without a deadline a hung plugin leaves the
  // page showing old figures with no banner and no further attempt, because the
  // poll interval will not start a second request while the first is in flight.
  const deadline = AbortSignal.any([AbortSignal.timeout(20_000), ...(signal ? [signal] : [])])
  const response = await fetch(devScenario(one.url), {
    headers,
    signal: deadline,
    cache: "no-store",
    credentials: "same-origin",
  })

  // 429 is the fallback route's rate limit on failed attempts. It means this
  // credential is not getting in right now, which is a rejection like any other.
  if (response.status === 401 || response.status === 403 || response.status === 429) return "rejected"
  if (response.status === 304 && cached && cached.url === one.url) return cached.document
  if (!response.ok) throw new Error(`summary request failed: ${response.status}`)

  const document = (await response.json()) as Summary
  const etag = response.headers.get("ETag")
  cached = etag ? { url: one.url, etag, document } : null
  return document
}

export async function fetchSummary(signal?: AbortSignal): Promise<Summary> {
  const list = attempts()
  if (list.length === 0) throw new NoSessionError(false)

  for (const one of list) {
    const result = await attempt(one, signal)
    if (result !== "rejected") return result
    // Rejected: drop any document cached against this route before trying the
    // next credential, or falling through to the sign-in screen.
    if (cached && cached.url === one.url) resetCache()
  }
  throw new NoSessionError(true)
}
