import { ConsoleUnansweredError, NoSessionError, readThrough, refetchInterval } from "./access"
import { managementPath, resourcePath } from "./cpa-auth"
import { accessContext, openDoors } from "./session"
import type { Summary } from "./types"

/**
 * Neither way in worked. `hadCredential` and `reason` are documented where the
 * class lives, in access.ts, which is where it is thrown.
 */
export { NoSessionError }

/**
 * A console read got no answer, so reads with that key stop until the reader
 * asks again. Documented where it is thrown, in access.ts.
 */
export { ConsoleUnansweredError }

/**
 * How often to poll the document: every minute while some door is open, and
 * not at all while none is.
 *
 * TanStack keeps an interval running whatever the last attempt came to, so a
 * fixed one would ask again every minute with nothing to ask with. Polling
 * with no door sends nothing — fetchSummary throws before the network — but it
 * would keep the sign-in screen re-rendering for no reason. A door that opens
 * later, by a sign-in here or a storage change from another tab, comes with a
 * refetch of its own.
 */
export function summaryRefetchInterval(): number | false {
  return refetchInterval(openDoors())
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

/**
 * The document, through the first open door: the dashboard password, else
 * the console's key. Which door, and what a refusal does to it, is decided in
 * access.ts readThrough; this adds the cache and reads the body.
 *
 * `signal` is TanStack's. It cancels a read through the password, which costs
 * nothing to abandon, and never one through the console — see readThrough.
 */
export async function fetchSummary(signal?: AbortSignal): Promise<Summary> {
  let read: Awaited<ReturnType<typeof readThrough>>
  try {
    read = await readThrough(
      accessContext(),
      (door) => {
        const url = devScenario(door.kind === "token" ? resourcePath("/summary") : managementPath("/summary"))
        const headers: Record<string, string> = { Accept: "application/json" }
        if (cached && cached.url === url) headers["If-None-Match"] = cached.etag
        return { url, headers }
      },
      signal,
    )
  } catch (error) {
    // Neither door let the page in: drop whatever was cached against the old
    // credential before the sign-in screen, or a later one, can be handed it.
    if (error instanceof NoSessionError) resetCache()
    throw error
  }

  const { target, response } = read
  if (response.status === 304 && cached && cached.url === target.url) return cached.document
  if (!response.ok) throw new Error(`summary request failed: ${response.status}`)

  const document = (await response.json()) as Summary
  const etag = response.headers.get("ETag")
  cached = etag ? { url: target.url, etag, document } : null
  return document
}
