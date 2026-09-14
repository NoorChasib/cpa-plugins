import { authHeaders, managementKey, managementPath } from "./cpa-auth"
import type { Summary } from "./types"

/**
 * No usable CPA session.
 *
 * Either the console's management key was not in this browser's storage, or it
 * was and CPA rejected it. Both mean the same thing to a reader — sign in to
 * the console — so they are one error rather than two.
 */
export class NoSessionError extends Error {
  constructor(readonly rejected: boolean) {
    super(rejected ? "management key rejected" : "no management key")
    this.name = "NoSessionError"
  }
}

/**
 * The last response, kept so a 304 has something to return.
 *
 * The route is a memory read on the plugin and answers `If-None-Match` with a
 * 304, which is the point of polling it every minute. A 304 carries no body, so
 * the previous document is what the poll resolves to.
 */
let cached: { etag: string; document: Summary } | null = null

export function resetCache(): void {
  cached = null
}

export async function fetchSummary(signal?: AbortSignal): Promise<Summary> {
  // Checked before the request rather than after a 401, so a browser with no
  // console session is told what to do instead of being shown a failure.
  if (managementKey() === null) throw new NoSessionError(false)

  const headers = authHeaders({ Accept: "application/json" })
  if (cached) headers["If-None-Match"] = cached.etag

  let url = managementPath("/summary")
  if (import.meta.env.DEV) {
    // The dev fixture route serves one scenario per §5 state. Stripped from the
    // production bundle, where the only thing on the other end is the plugin.
    const dev = new URLSearchParams(window.location.search)
    const forwarded = new URLSearchParams()
    for (const key of ["scenario", "rebase", "expiring"]) {
      const value = dev.get(key)
      if (value !== null) forwarded.set(key, value)
    }
    if ([...forwarded].length > 0) url += `?${forwarded}`
  }

  // A connection that is refused rejects on its own; one that is accepted and
  // then never answered does not. Without a deadline a hung plugin leaves the
  // page showing old figures with no banner and no further attempt, because the
  // poll interval will not start a second request while the first is in flight.
  const deadline = AbortSignal.any([AbortSignal.timeout(20_000), ...(signal ? [signal] : [])])
  const response = await fetch(url, {
    headers,
    signal: deadline,
    cache: "no-store",
    // Same-origin, because that is the only thing this page ever talks to.
    credentials: "same-origin",
  })

  if (response.status === 401 || response.status === 403) {
    resetCache()
    throw new NoSessionError(true)
  }
  if (response.status === 304 && cached) return cached.document
  if (!response.ok) throw new Error(`summary request failed: ${response.status}`)

  const document = (await response.json()) as Summary
  const etag = response.headers.get("ETag")
  cached = etag ? { etag, document } : null
  return document
}
