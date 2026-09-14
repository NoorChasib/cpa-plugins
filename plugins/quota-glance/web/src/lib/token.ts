// The fallback credential, for a reader with no CPA console session.
//
// The primary way in is the console's own management key (see cpa-auth.ts),
// which costs the reader nothing. This is what is left when that is not
// available: a browser that has never signed in to the console, a phone, a
// bookmark. Resource routes are GET-only, so there is no login POST and no
// endpoint that can set a cookie — the value is held here and sent as a bearer
// header.

const STORAGE_KEY = "quota-glance.token"

/**
 * Takes a token out of the URL and into storage, before anything renders or
 * fetches.
 *
 * Supports the `…/app?token=<token>` link. The URL is rewritten immediately
 * because it is the most exposed copy of the value there is: it sits in the
 * address bar, in history, and in anything the page later hands a URL to.
 * replaceState rather than pushState, so Back does not navigate to an entry
 * that still carries it.
 */
export function captureTokenFromURL(): void {
  const url = new URL(window.location.href)
  const token = url.searchParams.get("token")
  if (!token) return
  write(token)
  url.searchParams.delete("token")
  window.history.replaceState(null, "", `${url.pathname}${url.search}${url.hash}`)
}

export function read(): string | null {
  try {
    return window.localStorage.getItem(STORAGE_KEY)
  } catch {
    // Private browsing, or storage disabled. The page still works for this one
    // load; the sign-in screen is what the reader sees next time.
    return null
  }
}

export function write(token: string): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, token.trim())
  } catch {
    /* see read() */
  }
}

export function clear(): void {
  try {
    window.localStorage.removeItem(STORAGE_KEY)
  } catch {
    /* see read() */
  }
}
