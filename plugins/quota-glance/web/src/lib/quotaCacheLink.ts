// The link from a card to Quota Cache's page, where its values are edited.
//
// Since 0.8.0 Quota Glance shows the API credits and Claude renewal dates
// read-only. Quota Cache keeps them and edits them on its own CPA sidebar
// page, which reads and saves with the management key the CPA console
// remembers. So the link is drawn only where that page can work: a browser
// whose console session this page may present (access.ts usableConsoleKey).
// Behind the dashboard password alone — a phone, a bookmark — it is not.
//
// Nor in the menu bar app's popover, even signed in through the console. The
// popover keeps every same-origin address inside itself, so a link would load
// Quota Cache's page there, and the menu bar's readout only runs on this page:
// it would stop updating until the reader found their way back. The popover's
// readout bridge, window.__quotaGlanceReadout, is defined before this page's
// first script runs, so its presence is the test.
//
// Imports nothing but types, so node loads it directly in test/. What it asks
// of access.ts and of the document comes in through `access`.

import type { Env } from "./access"

/** Quota Cache's sidebar page, from the root every path this page builds starts from. */
export const QUOTA_CACHE_PAGE = "/v0/resource/plugins/quota-cache/status"

/** The sections of Quota Cache's page a card links to: its element IDs. */
export type QuotaCacheSection = "api-credits" | "renewals"

/** What the link depends on, beyond where this page is. */
export interface LinkAccess {
  /** access.ts usableConsoleKey: the console key this page may present for `env`, or null. */
  usableConsoleKey(env: Env): string | null
  /** access.ts rootOf: the origin and any reverse-proxy prefix in front of this page. */
  rootOf(env: Env): string
  /** Whether the menu bar app's readout bridge is in this document. */
  readout(): boolean
}

/**
 * Quota Cache's page at a section, or null when no link may be drawn: no
 * console session this page may present, or the menu bar's popover. The root
 * keeps a reverse proxy's prefix, as Quota Cache's own page does when it
 * builds its paths.
 */
export function quotaCacheEditURL(access: LinkAccess, env: Env, section: QuotaCacheSection): string | null {
  if (access.readout() || access.usableConsoleKey(env) === null) return null
  return `${access.rootOf(env)}${QUOTA_CACHE_PAGE}#${section}`
}

/** An anchor's attributes for a link to Quota Cache's page. */
export interface QuotaCacheAnchor {
  href: string
  target: "_blank"
  rel: "noopener"
  title: string
}

/**
 * The anchor for a link, or null for no anchor at all. A new tab, because in
 * CPA's sidebar this page sits in a frame, and a link followed in the same
 * frame would replace it with no way back.
 */
export function quotaCacheAnchor(url: string | null): QuotaCacheAnchor | null {
  if (url === null) return null
  return { href: url, target: "_blank", rel: "noopener", title: "Opens Quota Cache's page in a new tab" }
}
