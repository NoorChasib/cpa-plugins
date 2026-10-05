// The dashboard password: `web-token` from the plugin's configuration.
//
// It is the door this page tries first (see access.ts). It goes to this
// plugin's own resource routes, which CPA does not authenticate, so a wrong or
// stale one costs the reader nothing at CPA — no management sign-in is counted
// against the address. With it the page reads the document and, since 0.5.0,
// spends banked resets through the GET spend route. Resource routes are
// GET-only, so there is no login POST and no endpoint that can set a cookie:
// the value is held here and sent as a bearer header.

import { ACCESS_CHANGED, TOKEN_KEY } from "./access"

/**
 * The value the plugin refused in this document, held back so no poll, retry
 * or press presents it again. Memory only, on purpose: a reload, saving a new
 * password or signing out each forgets it, and each of those is the reader
 * acting on the refusal.
 */
let refusedToken: string | null = null

/**
 * Tells this document's listeners that a door may have opened or closed. A
 * `storage` event reaches only other documents, and the refusal above is not
 * in storage at all. Deferred, so a change made while React renders is not
 * announced into the middle of that render.
 */
function changed(): void {
  queueMicrotask(() => window.dispatchEvent(new Event(ACCESS_CHANGED)))
}

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
    const value = window.localStorage.getItem(TOKEN_KEY)
    return value === "" ? null : value
  } catch {
    // Private browsing, or storage disabled. The page still works for this one
    // load; the sign-in screen is what the reader sees next time.
    return null
  }
}

/** Saves a password the reader typed, and forgets that an earlier one was refused. */
export function write(token: string): void {
  refusedToken = null
  try {
    window.localStorage.setItem(TOKEN_KEY, token.trim())
  } catch {
    /* see read() */
  }
  changed()
}

export function clear(): void {
  refusedToken = null
  try {
    window.localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* see read() */
  }
  changed()
}

/** Holds `value` back: the plugin refused it, and it is not tried again here. */
export function refuse(value: string): void {
  if (refusedToken === value) return
  refusedToken = value
  changed()
}

/** The value refused in this document, if any. */
export function refused(): string | null {
  return refusedToken
}
