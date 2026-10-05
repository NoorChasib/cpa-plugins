import { useId } from "react"

import { CredentialRow } from "./CredentialRow"
import { useNowSeconds } from "../lib/now"
import { type AccountName, hasSlices, identitySlot, nameText, recoveryNames, shortName } from "../lib/pool"
import { formatDuration, secondsUntil } from "../lib/time"
import type { Credential, Row } from "../lib/types"

/** The big number's ink, from the level the server computed. */
const LEVEL_CLASS: Record<string, string> = {
  ok: "is-ok",
  low: "is-low",
  critical: "is-crit",
}

/**
 * The level in words, beside the number its colour already marks.
 *
 * Colour alone cannot be the signal: a reader who cannot tell amber from green
 * would otherwise see "32% left" and "62% left" judged identically. Nothing is
 * printed at `ok`, which is the ordinary state and would only add a word to
 * every card.
 */
const LEVEL_WORD: Record<string, string> = {
  low: "low",
  critical: "critical",
}

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

/**
 * Direction of travel over the last hour, as a chip.
 *
 * Hidden entirely on `unknown`, which is the normal state for the first half
 * hour after a restart and would otherwise put a meaningless mark on every
 * card. The word is printed, so the arrow is decoration.
 */
function TrendChip({ trend }: { trend: string }) {
  if (trend !== "up" && trend !== "down") return null
  const up = trend === "up"
  return (
    <span className={`qg-chip ${up ? "qg-chip-up" : "qg-chip-down"}`}>
      <svg viewBox="0 0 10 10" aria-hidden="true">
        <path d={up ? "M5 1.5 L9 8 L1 8 Z" : "M5 8.5 L1 2 L9 2 Z"} fill="currentColor" />
      </svg>
      {up ? "rising" : "falling"}
    </span>
  )
}

/**
 * The fold marker: pointing down when the accounts are showing, right when
 * they are folded away. `aria-hidden` because the button around it announces
 * its state through `aria-expanded`.
 */
function Chevron() {
  return (
    <svg viewBox="0 0 10 10" aria-hidden="true" className="qg-chev">
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

/** What a row's readers need to name its accounts, in entry order. */
type Named = { name: string; slot: number }

/**
 * A legend swatch's state: solid for a reading the bar shows, an outline round
 * a tint for one it shows stippled, hollow for no reading, and plain grey when
 * the document has no slices at all — a coloured dot would promise a slice of
 * the bar that is not there.
 */
function swatchState(entry: Row["entries"][number], sliced: boolean): string {
  if (!sliced) return "is-plain"
  if (!entry.hasReading) return "is-hollow"
  return entry.state !== "ok" ? "is-faded" : ""
}

/**
 * The pool, as one bar.
 *
 * Each account's slice of what is left, laid end to end in entry order, then a
 * hatched slice for each account in the next recovery — what that reset gives
 * back — then the empty track: capacity no scheduled reset is about to return.
 * Every width is a field the server wrote, so the bar is the headline taken
 * apart and cannot disagree with the number under it.
 *
 * A slice of an account whose reading the server does not vouch for — stale,
 * failed — is drawn faded: it is in the mean, so it stays in the bar, but it
 * should not read as firmly as the rest.
 */
function PoolBar({ row, named, sliced, label }: { row: Row; named: Named[]; sliced: boolean; label: string }) {
  const kept = row.entries.flatMap((entry, index) => {
    const share = entry.poolShare ?? 0
    if (!sliced || share <= 0) return []
    const who = named[index]!
    return [
      <i
        key={`k${index}`}
        className={`qg-seg qg-id-${who.slot} ${entry.state !== "ok" ? "is-faded" : ""}`}
        style={{ width: `${share * 100}%` }}
        title={`${who.name} · ${entry.remainingPercent}% left`}
      />,
    ]
  })
  const returning = row.entries.flatMap((entry, index) => {
    const share = entry.recoveryShare ?? 0
    if (!sliced || share <= 0) return []
    const who = named[index]!
    return [
      <i
        key={`r${index}`}
        className={`qg-ghost qg-id-${who.slot} ${entry.state !== "ok" ? "is-faded" : ""}`}
        style={{ width: `${share * 100}%` }}
        title={`${who.name} · back to full at its next reset`}
      />,
    ]
  })

  return (
    <div className="qg-bar" role="img" aria-label={label}>
      {sliced ? (
        <>
          {kept}
          {returning}
        </>
      ) : (
        // A document without slices: the row's own fraction, undivided.
        <i className="qg-seg-whole" style={{ width: `${row.aggregate.remainingFraction * 100}%` }} />
      )}
    </div>
  )
}

/**
 * When this window next recovers, from the structured fields rather than the
 * server's sentence: the gain in large type, who resets, and a countdown that
 * ticks. A plugin too old to say who resets gets its own sentence printed
 * whole, which is still the truth, only plainer.
 */
function Recovery({ row, named, now }: { row: Row; named: Named[]; now: number }) {
  const aggregate = row.aggregate
  if (aggregate.soonestResetAtEpoch === null) return null
  if (!row.entries.every((entry) => typeof entry.resetsNext === "boolean")) {
    // A sentence, so it is set as one: the recovery line's own face, not the
    // figures' monospace.
    return aggregate.subtext ? <p className="qg-rec">{aggregate.subtext}</p> : null
  }
  const who = recoveryNames(row.entries.flatMap((entry, index) => (entry.resetsNext ? [named[index]!.name] : [])))
  if (!who) return null
  const remaining = secondsUntil(aggregate.soonestResetAtEpoch, now)
  const verb = who.plural ? "reset" : "resets"

  return (
    <p className="qg-rec">
      {aggregate.projectedGainPercent > 0 && (
        <>
          <b className="qg-gain num">+{aggregate.projectedGainPercent}%</b> when{" "}
        </>
      )}
      <b className="qg-who">{who.who}</b> {verb}{" "}
      {remaining > 0 ? (
        <>
          in <b className="num">{formatDuration(remaining)}</b>
        </>
      ) : (
        "now"
      )}
    </p>
  )
}

/** The same recovery, as one sentence for the bar's label. */
function recoverySentence(row: Row, named: Named[], now: number): string {
  const aggregate = row.aggregate
  if (aggregate.soonestResetAtEpoch === null) return ""
  const who = recoveryNames(row.entries.flatMap((entry, index) => (entry.resetsNext ? [named[index]!.name] : [])))
  if (!who) return aggregate.subtext
  const remaining = secondsUntil(aggregate.soonestResetAtEpoch, now)
  const when = remaining > 0 ? `in ${formatDuration(remaining)}` : "now"
  const verb = who.plural ? "reset" : "resets"
  return aggregate.projectedGainPercent > 0
    ? `Plus ${aggregate.projectedGainPercent}% when ${who.who} ${verb} ${when}.`
    : `${who.who} ${verb} ${when}.`
}

/**
 * One window across a provider's credentials: the pool first, its accounts
 * after, foldable.
 *
 * What folds is the per-account detail; the pool stays. The bar, the number,
 * when it recovers and the legend are all still there when the card is shut,
 * because those are what a reader scanning the page came for — a fold that hid
 * the number would just be a card that was gone.
 */
export function WindowCard({
  row,
  credentials,
  names,
  collapsed,
  onToggle,
}: {
  row: Row
  credentials: Map<string, Credential>
  /** What the provider's section calls each account; see accountNames. */
  names: Map<string, AccountName>
  collapsed: boolean
  onToggle: () => void
}) {
  // Ties the fold to what it folds, so assistive technology can follow the
  // relationship rather than infer it from where the elements sit.
  const bodyID = useId()
  // The fold is named by its card as well as its count: three cards each
  // offering a button called "5 accounts" is three buttons nobody can tell
  // apart in a list of controls.
  const titleID = useId()
  const countID = useId()
  const now = useNowSeconds()
  const aggregate = row.aggregate
  const sliced = hasSlices(row.entries)

  const named: Named[] = row.entries.map((entry, index) => ({
    name: nameText(
      names.get(entry.credentialId) ?? {
        local: shortName(credentials.get(entry.credentialId), entry.credentialId),
        qualifier: "",
      },
    ),
    slot: identitySlot(index),
  }))
  const levelWord = LEVEL_WORD[aggregate.level]

  const figures = row.entries
    .map((entry, index) => `${named[index]!.name} ${entry.hasReading ? `${entry.remainingPercent}%` : "no reading"}`)
    .join(", ")
  // The far end of the recovery: when every account below full has reset.
  // Shown only when it says something the recovery line has not — with one
  // account, or one account below full, it is the same instant.
  const full =
    typeof aggregate.fullAtEpoch === "number" && aggregate.fullAtEpoch !== aggregate.soonestResetAtEpoch
      ? secondsUntil(aggregate.fullAtEpoch, now)
      : 0

  // Everything the hidden line under the bar shows, so that line can stay
  // hidden from assistive technology without taking a figure with it.
  const label = [
    `${aggregate.remainingPercent}% left${levelWord ? `, ${levelWord}` : ""}${
      row.entries.length > 1 ? ` across ${plural(aggregate.memberCount, "account")}` : ""
    }.`,
    row.entries.length > 1 ? `${figures}.` : "",
    recoverySentence(row, named, now),
    full > 0 ? `Full again in ${formatDuration(full)}.` : "",
  ]
    .filter(Boolean)
    .join(" ")
  // A legend for one account repeats the number above it.
  const legend = row.entries.length > 1

  // One account: the pool bar above is that account's bar, so its row carries
  // who it is and what it is doing, not the same bar and figure again.
  const solo = row.entries.length === 1

  return (
    // A critical pool says so at the card's edge as well as in its number and
    // chip. Only there: the accounts keep their own shades, which say who each
    // is, and a healthy account in a critical pool is still healthy.
    <article className={`qg-win ${aggregate.level === "critical" ? "is-crit" : ""}`} aria-label={row.title}>
      <div className="qg-whead">
        <h3 id={titleID} className="qg-wtitle">
          {row.title}
        </h3>
        <span className="qg-chips">
          <TrendChip trend={aggregate.trend} />
          {levelWord && <span className={`qg-chip qg-chip-${aggregate.level}`}>{levelWord}</span>}
        </span>
      </div>

      <PoolBar row={row} named={named} sliced={sliced} label={label} />

      <div className="qg-hfig">
        <span className={`qg-big ${LEVEL_CLASS[aggregate.level] ?? ""}`}>
          <span className="num">{aggregate.remainingPercent}</span>
          <span className="qg-unit">% left</span>
        </span>
        <Recovery row={row} named={named} now={now} />
      </div>

      {(legend || full > 0) && (
        // The bar's label already carries every figure here, so assistive
        // technology is not read the same list twice.
        <div className="qg-hsub" aria-hidden="true">
          {legend && (
            <span className="qg-legend">
              {row.entries.map((entry, index) => (
                <span key={entry.credentialId} className={entry.state !== "ok" ? "is-faded" : ""}>
                  <i className={`qg-sw qg-id-${named[index]!.slot} ${swatchState(entry, sliced)}`} />
                  {named[index]!.name}
                  <b className="num">{entry.hasReading ? `${entry.remainingPercent}%` : "—"}</b>
                </span>
              ))}
            </span>
          )}
          {full > 0 && (
            <span className="qg-full">
              full again in <b className="num">{formatDuration(full)}</b>
            </span>
          )}
        </div>
      )}

      <button
        type="button"
        className="qg-fold"
        aria-expanded={!collapsed}
        aria-controls={bodyID}
        aria-labelledby={`${titleID} ${countID}`}
        onClick={onToggle}
      >
        <Chevron />
        <span id={countID}>
          {plural(row.entries.length, "account")}
          {/* The mean is over the accounts with a reading. Saying how many
            * have none is what explains a pool that looks smaller than its
            * rows. */}
          {aggregate.excludedCount > 0 && (
            <span className="qg-fold-note"> · {aggregate.excludedCount} without a reading</span>
          )}
        </span>
      </button>

      {/* Rendered in the order received. credentials[] is pre-sorted by soonest
        * weekly reset and every row repeats that order, so the top line is the
        * credential that recovers next — in this card and in every other.
        *
        * Unmounted rather than hidden when folded. Each row runs its own
        * countdown off the shared clock, and keeping a dozen invisible ones
        * ticking is work nobody can see. */}
      <div id={bodyID} hidden={collapsed} className="qg-rows">
        {!collapsed &&
          row.entries.map((entry, index) => (
            <CredentialRow
              key={entry.credentialId}
              entry={entry}
              credential={credentials.get(entry.credentialId)}
              slot={named[index]!.slot}
              solo={solo}
            />
          ))}
      </div>
    </article>
  )
}
