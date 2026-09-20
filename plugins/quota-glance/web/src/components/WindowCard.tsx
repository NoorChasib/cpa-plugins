import { useId } from "react"

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

/**
 * The fold marker: pointing down when the card is open, right when it is shut.
 *
 * Inline SVG like every other mark in this bundle, and `aria-hidden` because
 * the button around it already announces its state through `aria-expanded` —
 * a reader would otherwise be told the same thing twice.
 */
function Chevron({ open }: { open: boolean }) {
  return (
    <svg
      viewBox="0 0 10 10"
      aria-hidden="true"
      className={`size-[9px] shrink-0 text-ink-3 transition-transform duration-150 ${open ? "" : "-rotate-90"}`}
    >
      <path
        d="M1.5 3.5 L5 7 L8.5 3.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

/**
 * One window across a provider's credentials, foldable.
 *
 * What folds is the per-credential detail; the headline stays. Title, trend,
 * the big percentage and the server's subtext are all still there when the card
 * is shut, because those are what a reader scanning the page came for — a fold
 * that hid the number would just be a card that was gone.
 */
export function WindowCard({
  row,
  credentials,
  collapsed,
  onToggle,
}: {
  row: Row
  credentials: Map<string, Credential>
  collapsed: boolean
  onToggle: () => void
}) {
  // Ties the button to what it folds, so assistive technology can follow the
  // relationship rather than infer it from where the elements sit.
  const bodyID = useId()

  return (
    <div className={`mb-[10px] rounded-[12px] border border-line bg-card px-4 pt-[15px] ${collapsed ? "pb-[14px]" : "pb-[6px]"}`}>
      <button
        type="button"
        className={`qg-head grid w-full grid-cols-[1fr_auto] items-baseline gap-x-3 text-left ${
          // The rule under the header separates it from the rows below it.
          // With the rows folded away there is nothing to separate, and a rule
          // along the bottom of a card reads as a card that failed to load.
          collapsed ? "" : "mb-[4px] border-b border-line pb-3"
        }`}
        aria-expanded={!collapsed}
        aria-controls={bodyID}
        onClick={onToggle}
      >
        <span className="flex min-w-0 items-center gap-[7px]">
          <Chevron open={!collapsed} />
          <span className="qg-title truncate text-[13.5px] font-[550] text-ink transition-colors">{row.title}</span>
        </span>

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
          * member of this row has a future reset. Kept while folded: it is the
          * one line that says when this window recovers. */}
        {row.aggregate.subtext && (
          <span className="num col-span-full mt-[7px] text-[11px] text-ink-3">{row.aggregate.subtext}</span>
        )}
      </button>

      {/* Rendered in the order received. credentials[] is pre-sorted by soonest
        * weekly reset and every row repeats that order, so the top line is the
        * credential that recovers next — in this card and in every other.
        *
        * Unmounted rather than hidden when folded. Each row runs its own
        * countdown off the shared clock, and keeping a dozen invisible ones
        * ticking is work nobody can see. */}
      <div id={bodyID} hidden={collapsed}>
        {!collapsed &&
          row.entries.map((entry) => (
            <CredentialRow key={entry.credentialId} entry={entry} credential={credentials.get(entry.credentialId)} />
          ))}
      </div>
    </div>
  )
}
