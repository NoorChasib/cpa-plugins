import { CredentialRow } from "./CredentialRow"
import type { Credential, Row } from "../lib/types"

/** The big number's colour, from the level the server computed. */
const LEVEL_INK: Record<string, string> = {
  ok: "text-good",
  low: "text-warn",
  critical: "text-crit",
}

/**
 * Direction of travel over the last hour.
 *
 * Hidden entirely on `unknown`, which is the normal state for the first half
 * hour after a restart and would otherwise put a meaningless arrow on every
 * card. Inline SVG, like every other mark in this bundle.
 */
function TrendMark({ trend }: { trend: string }) {
  if (trend !== "up" && trend !== "down") return null
  const label = trend === "up" ? "rising" : "falling"
  return (
    <svg
      viewBox="0 0 10 10"
      aria-label={label}
      role="img"
      className={`size-[9px] shrink-0 ${trend === "up" ? "text-good" : "text-ink-3"}`}
    >
      <path
        d={trend === "up" ? "M5 1.5 L9 8 L1 8 Z" : "M5 8.5 L1 2 L9 2 Z"}
        fill="currentColor"
      />
    </svg>
  )
}

export function WindowCard({ row, credentials }: { row: Row; credentials: Map<string, Credential> }) {
  return (
    <div className="mb-[10px] rounded-[12px] border border-line bg-card px-4 pb-[6px] pt-[15px]">
      <div className="mb-[4px] grid grid-cols-[1fr_auto] items-baseline gap-x-3 border-b border-line pb-3">
        <span className="text-[13.5px] font-[550] text-ink">{row.title}</span>

        <span className="flex items-baseline gap-1 whitespace-nowrap">
          <TrendMark trend={row.aggregate.trend} />
          <span
            className={`num text-[26px] font-medium leading-none tracking-[-0.025em] ${
              LEVEL_INK[row.aggregate.level] ?? "text-ink"
            }`}
          >
            {row.aggregate.remainingPercent}
          </span>
          <span className="text-[11px] text-ink-3">% left</span>
        </span>

        {/* Already written out by the server, down to the wording, so every
          * client says the same thing about the same window. Empty when no
          * member of this row has a future reset. */}
        {row.aggregate.subtext && (
          <span className="num col-span-full mt-[7px] text-[11px] text-ink-3">{row.aggregate.subtext}</span>
        )}
      </div>

      {/* Rendered in the order received. credentials[] is pre-sorted by soonest
        * weekly reset and every row repeats that order, so the top line is the
        * credential that recovers next — in this card and in every other. */}
      {row.entries.map((entry) => (
        <CredentialRow key={entry.credentialId} entry={entry} credential={credentials.get(entry.credentialId)} />
      ))}
    </div>
  )
}
