// What the Monthly API Credit card and Claude's Accounts card share about
// values entered on Quota Cache's page: the mark beside each one, and the link
// to that page, where they are edited. Nothing is edited here.

import { ENTERED_HERE } from "../lib/apicredits"
import { quotaCacheAnchor } from "../lib/quotaCacheLink"

/**
 * "Entered in Quota Cache": a value entered on Quota Cache's page rather than
 * read from its config. A dot in the accent after the value; the hover says
 * what the config has. `lead` is the one in a foot, before its words.
 */
export function SetHereDot({ title, lead = false }: { title?: string; lead?: boolean }) {
  return lead ? (
    <span className="qg-mk is-lead" aria-hidden="true" />
  ) : (
    <span className="qg-mk" role="img" aria-label={ENTERED_HERE} title={title} />
  )
}

/** The middot between the parts of a caption, which a screen reader skips. */
export function Dot() {
  return (
    <span className="qg-edot" aria-hidden="true">
      ·
    </span>
  )
}

/** The arrow after a link that leaves this page. */
function OutArrow() {
  return (
    <svg viewBox="0 0 12 12" fill="none" aria-hidden="true">
      <path d="M4 2.5h5.5V8M9.5 2.5 2.5 9.5" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

/**
 * A link to Quota Cache's page, opening in a new tab: in a card's foot, or
 * after a line that asks for something only that page can set. Nothing at
 * all without a URL (quotaCacheLink.ts says when there is none), so the line
 * before it keeps its own words.
 */
export function QuotaCacheLink({ url, children }: { url: string | null; children: React.ReactNode }) {
  const anchor = quotaCacheAnchor(url)
  if (anchor === null) return null
  return (
    <a className="qg-elink qg-xlink" {...anchor}>
      {children}
      <OutArrow />
    </a>
  )
}
