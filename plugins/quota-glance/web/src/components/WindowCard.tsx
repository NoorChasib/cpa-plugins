import { useId } from "react"

import { CredentialRow } from "./CredentialRow"
import { useNowSeconds } from "../lib/now"
import { type AccountName, foldNotes, nameText, recoveryNames, shortName, weeklyNote } from "../lib/pool"
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

/** What a row's readers call its accounts, in entry order. */
type Named = { name: string }

/**
 * The pool, as one bar in one colour.
 *
 * What is left, as a single fill in the level's colour — the accent while the
 * pool is healthy, amber when it runs low, red when it is critical — then a
 * hatched stretch in the same colour for what the next reset gives back, then
 * the empty track. Which account holds what is the rows' job below, not the
 * bar's: one fill reads as one quantity, which is what a pool is.
 *
 * Every width is a field the server wrote, so the bar cannot disagree with
 * the number under it. A plugin too old to report the projected gain draws
 * no hatching rather than guessing at it.
 */
function PoolBar({ row, label }: { row: Row; label: string }) {
  const aggregate = row.aggregate
  const gain = typeof aggregate.projectedGainFraction === "number" ? aggregate.projectedGainFraction : 0
  return (
    <div className={`qg-bar qg-lvl-${aggregate.level}`} role="img" aria-label={label}>
      {aggregate.remainingFraction > 0 && (
        <i className="qg-fill" style={{ width: `${aggregate.remainingFraction * 100}%` }} />
      )}
      {gain > 0 && <i className="qg-ghost" style={{ width: `${gain * 100}%` }} title="back at the next reset" />}
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
 * What folds is the per-account detail; the pool stays. The bar, the number
 * and when it recovers are all still there when the card is shut,
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

  const named: Named[] = row.entries.map((entry) => ({
    name: nameText(
      names.get(entry.credentialId) ?? {
        local: shortName(credentials.get(entry.credentialId), entry.credentialId),
        qualifier: "",
      },
    ),
  }))
  const levelWord = LEVEL_WORD[aggregate.level]

  // Each account's own figure, with the reason when the pool counts it as
  // less — "noor 100% (weekly spent)". The label is read with the card
  // folded, when the chip that says so beside the row is not there, and
  // without it the figures would add up to more than the pool. In brackets,
  // because the comma already separates the accounts and a note set off by
  // one more is heard as belonging to whichever account comes next.
  const figures = row.entries
    .map((entry, index) => {
      const weekly = weeklyNote(entry)
      return `${named[index]!.name} ${entry.hasReading ? `${entry.remainingPercent}%` : "no reading"}${
        weekly ? ` (${weekly.text})` : ""
      }`
    })
    .join(", ")
  // How many accounts the pool's figure is over. When every account that
  // reports is held out the mean covers none of them, and "across 0 accounts"
  // ahead of a list of five reads as a miscount, so it says why instead —
  // "reporting", because accounts with no reading can sit beside them.
  const heldOut = typeof aggregate.heldOutCount === "number" ? aggregate.heldOutCount : 0
  const scope =
    row.entries.length <= 1
      ? ""
      : aggregate.memberCount === 0 && heldOut > 0
        ? ": every reporting account's weekly is spent"
        : ` across ${plural(aggregate.memberCount, "account")}`
  const foldNote = foldNotes(aggregate)
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
    `${aggregate.remainingPercent}% left${levelWord ? `, ${levelWord}` : ""}${scope}.`,
    row.entries.length > 1 ? `${figures}.` : "",
    recoverySentence(row, named, now),
    full > 0 ? `Full again in ${formatDuration(full)}.` : "",
  ]
    .filter(Boolean)
    .join(" ")
  // One account: the pool bar above is that account's bar, so its row carries
  // who it is and what it is doing, not the same bar and figure again.
  const solo = row.entries.length === 1

  return (
    // A critical pool says so at the card's edge as well as in its bar, its
    // number and its chip.
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

      <PoolBar row={row} label={label} />

      <div className="qg-hfig">
        <span className={`qg-big ${LEVEL_CLASS[aggregate.level] ?? ""}`}>
          <span className="num">{aggregate.remainingPercent}</span>
          <span className="qg-unit">% left</span>
        </span>
        <Recovery row={row} named={named} now={now} />
      </div>

      {full > 0 && (
        // The bar's label already carries this, so assistive technology is
        // not read it twice.
        <div className="qg-hsub" aria-hidden="true">
          <span className="qg-full">
            full again in <b className="num">{formatDuration(full)}</b>
          </span>
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
          {/* The mean is over the accounts with a reading that it does not
            * hold out. Saying how many it leaves out, and why, is what
            * explains a pool that looks smaller than its rows — most of all
            * a session pool at 0% over accounts each reading 100%. */}
          {foldNote.length > 0 && <span className="qg-fold-note"> · {foldNote.join(" · ")}</span>}
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
          row.entries.map((entry) => (
            <CredentialRow
              key={entry.credentialId}
              entry={entry}
              credential={credentials.get(entry.credentialId)}
              solo={solo}
            />
          ))}
      </div>
    </article>
  )
}
