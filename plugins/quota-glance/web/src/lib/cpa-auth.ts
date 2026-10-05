// Authentication borrowed from the console that is already open — the second
// of this page's two doors, behind the dashboard password (see access.ts).
//
// This page is served from a CPA resource route, which CPA does not
// authenticate. A browser that has the official management console
// (Cli-Proxy-API-Management-Center) open on this origin may also hold the
// console's management key in localStorage, and this page can present that key
// to its plugin's management routes, which CPA does authenticate. That is how
// the CPA sidebar works with nothing to sign in to.
//
// It is also the one credential this page holds that CPA counts against the
// address when it is wrong, so the reading is strict (access.ts
// readConsoleKey): only a key the console remembered on purpose, for this very
// CPA, and not one CPA has already refused here.
//
// The recovered key goes to same-origin CPA management routes and nowhere else.
// It is never stored, copied, or logged by this page.

import { bearer, pagePrefix, PLUGIN_ID, usableConsoleKey } from "./access"
import { browserEnv } from "./session"

/** The console's management key, when this page may present it; else null. */
export function managementKey(): string | null {
  return usableConsoleKey(browserEnv())
}

/**
 * An absolute management path, built from where this page is.
 *
 * Derived rather than hardcoded so a reverse proxy that mounts CPA under a
 * prefix keeps working, and so the page behaves the same whether it was opened
 * from the resource tree or the management one.
 */
export function managementPath(suffix: string): string {
  return `${pagePrefix(window.location.pathname)}/v0/management/plugins/${PLUGIN_ID}${suffix}`
}

/** The same, on the resource tree, which the dashboard password opens. */
export function resourcePath(suffix: string): string {
  return `${pagePrefix(window.location.pathname)}/v0/resource/plugins/${PLUGIN_ID}${suffix}`
}

/**
 * `extra`, plus the Authorization header for `key`.
 *
 * The header value is assembled from parts so the built page never contains a
 * literal bearer-credential string — the same precaution the other plugin pages
 * take, and what lets the build assert it carries no token.
 */
export function authHeaders(key: string, extra?: Record<string, string>): Record<string, string> {
  return { ...extra, Authorization: bearer(key) }
}
