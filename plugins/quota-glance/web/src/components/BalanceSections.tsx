import { useNowSeconds } from "../lib/now"
import { formatAgo } from "../lib/time"
import type { Balance } from "../lib/types"

/** The amount's colour, from the level the server computed. */
const LEVEL_INK: Record<string, string> = {
  ok: "text-good",
  low: "text-warn",
  critical: "text-crit",
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
 * Laid out like a window card's header so it reads as the same kind of thing,
 * but with no bar and no fold. There is no allowance for a bar to be a fraction
 * of, and there are no per-credential rows to hide.
 */
function BalanceCard({ balance }: { balance: Balance }) {
  const now = useNowSeconds()
  const degraded = balance.state !== "ok"
  const label = degraded ? (STATE_LABELS[balance.state] ?? balance.state) : null

  return (
    <div className="mb-[10px] rounded-[12px] border border-line bg-card px-4 pb-[14px] pt-[15px]">
      <div className="grid grid-cols-[1fr_auto] items-baseline gap-x-3">
        <span className="truncate text-[13.5px] font-[550] text-ink">Balance</span>

        {/* A dash, never $0.00: an unread balance and an empty one are the
          * same zero in the document and opposite facts on this page. A
          * reading the server no longer vouches for keeps its figure, dimmed,
          * the way a window row keeps its percentage. */}
        {balance.hasReading ? (
          <span className={`flex items-baseline gap-1 whitespace-nowrap ${degraded ? "opacity-60" : ""}`}>
            <span
              className={`num text-[26px] font-medium leading-none tracking-[-0.025em] ${
                LEVEL_INK[balance.level] ?? "text-ink"
              }`}
            >
              {balance.remainingText}
            </span>
            <span className="text-[11px] text-ink-3">left</span>
          </span>
        ) : (
          <span className="num text-[26px] font-medium leading-none text-ink-3">—</span>
        )}

        {balance.subtext && (
          <span className="num col-span-full mt-[7px] text-[11px] text-ink-3">{balance.subtext}</span>
        )}
        {/* How old the figure is, ticked here because the server caches the
          * document between rebuilds, and why it may not be current. */}
        {(balance.hasReading || label) && (
          <span className="num col-span-full mt-[3px] text-[10.5px] text-ink-4">
            {balance.hasReading && `observed ${formatAgo(balance.observedAtEpoch, now)}`}
            {balance.hasReading && label && " · "}
            {label && <span className="text-ink-2">{label}</span>}
          </span>
        )}
      </div>
    </div>
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
        <section key={provider} className="mb-[30px]">
          <div className="mb-[11px] flex items-baseline gap-[9px] pl-[2px]">
            <h2 className="text-[14px] font-semibold tracking-[-0.01em]">{members[0]!.title}</h2>
            <span className="text-[11.5px] text-ink-3">prepaid credit</span>
          </div>
          {members.map((balance) => (
            <BalanceCard key={balance.id} balance={balance} />
          ))}
        </section>
      ))}
    </>
  )
}
