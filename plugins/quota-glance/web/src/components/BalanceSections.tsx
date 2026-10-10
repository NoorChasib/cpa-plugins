import { useId } from "react"

import { SectionHead } from "./ProviderSection"
import { useNowSeconds } from "../lib/now"
import { formatDuration } from "../lib/time"
import type { Balance } from "../lib/types"

/** The amount's ink, from the level the server computed. */
const LEVEL_CLASS: Record<string, string> = {
  ok: "is-ok",
  low: "is-low",
  critical: "is-crit",
}

/** The level in words, so colour is not the only thing saying it. */
const LEVEL_WORD: Record<string, string> = {
  low: "low",
  critical: "critical",
}

/** Why the amount is not the whole story, in the words the contract uses. */
const STATE_LABELS: Record<string, string> = {
  error: "last poll failed",
  stale: "stale",
  pending: "not polled yet",
}

/**
 * One prepaid account: the amount left, headline-sized, and the server's line
 * under it.
 *
 * Laid out like a window card's figures so it reads as the same kind of thing,
 * but with no bar and no fold. There is no allowance for a bar to be a fraction
 * of, and there are no per-credential rows to hide.
 */
function BalanceCard({ balance }: { balance: Balance }) {
  const now = useNowSeconds()
  const degraded = balance.state !== "ok"
  const label = degraded ? (STATE_LABELS[balance.state] ?? balance.state) : null
  const levelWord = balance.hasReading ? LEVEL_WORD[balance.level] : undefined

  return (
    <article className="qg-win qg-balance" aria-label={`${balance.title} balance`}>
      <div className="qg-whead">
        <h3 className="qg-wtitle">Prepaid balance</h3>
        <span className="qg-chips">
          {levelWord && <span className={`qg-chip qg-chip-${balance.level}`}>{levelWord}</span>}
        </span>
      </div>

      <div className="qg-hfig">
        {/* A dash, never $0.00: an unread balance and an empty one are the
          * same zero in the document and opposite facts on this page. A
          * reading the server no longer vouches for keeps its figure, dimmed,
          * the way a window row keeps its percentage. */}
        {balance.hasReading ? (
          <span className={`qg-big ${LEVEL_CLASS[balance.level] ?? ""} ${degraded ? "opacity-60" : ""}`}>
            <span className="num">{balance.remainingText}</span>
            <span className="qg-unit">left</span>
          </span>
        ) : (
          <span className="qg-big">
            <span className="num text-ink-3">—</span>
          </span>
        )}
        {/* How old the figure is, ticked here because the server caches the
          * document between rebuilds, and why it may not be current. */}
        {(balance.hasReading || label) && (
          <p className="qg-rec qg-rec-quiet">
            {balance.hasReading && (
              <>
                observed <span className="num">{formatDuration(now - balance.observedAtEpoch)}</span> ago
              </>
            )}
            {balance.hasReading && label && " · "}
            {label && <b className="text-ink-2">{label}</b>}
          </p>
        )}
      </div>

      {/* The server's sentence, printed whole and never parsed, so it is set
        * as a sentence rather than in the figures' monospace. */}
      {balance.subtext && <p className="qg-bsub">{balance.subtext}</p>}
    </article>
  )
}

function BalanceGroup({ members }: { members: Balance[] }) {
  const headingID = useId()
  return (
    <section className="qg-prov" aria-labelledby={headingID}>
      <SectionHead id={headingID} title={members[0]!.title} />
      {members.map((balance) => (
        <BalanceCard key={balance.id} balance={balance} />
      ))}
    </section>
  )
}

/**
 * Prepaid balances, one section per provider, below the quota providers.
 *
 * Absent entirely when none is configured, which leaves the page exactly as
 * it was for anyone without an OpenRouter management key in Quota Cache.
 */
export function BalanceSections({ balances }: { balances: Balance[] }) {
  if (balances.length === 0) return null
  // Sorted by the server's key; grouped in the order that produces.
  const sorted = [...balances].sort((a, b) => a.order - b.order)
  const groups = new Map<string, Balance[]>()
  for (const balance of sorted) groups.set(balance.provider, [...(groups.get(balance.provider) ?? []), balance])

  return (
    <>
      {[...groups.entries()].map(([provider, members]) => (
        <BalanceGroup key={provider} members={members} />
      ))}
    </>
  )
}
