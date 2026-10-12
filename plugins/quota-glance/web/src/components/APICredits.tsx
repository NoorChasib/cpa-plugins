import { useId } from "react"

import { QuotaCacheLink, SetHereDot } from "./Edit"
import { Flags, FoldButton } from "./Fold"
import { PoolBar } from "./WindowCard"
import {
  anySetHere,
  type CreditLink,
  type CreditNote,
  creditBar,
  creditBasis,
  creditCondition,
  conditionKey,
  conditionsSaidOnce,
  creditFigures,
  creditFlags,
  creditLevel,
  creditName,
  creditNotes,
  dateEpoch,
  editingOffText,
  editingTrouble,
  ENTERED_HERE,
  meterLine,
  poolNotes,
  type RefillWhen,
  refillWhen,
  setHereTitles,
  type SharedIssue,
  sharedIssues,
} from "../lib/apicredits"
import { useNowSeconds } from "../lib/now"
import { recoveryNames } from "../lib/pool"
import { formatClock, formatDate, formatDuration, formatUTCClock, formatUTCDate } from "../lib/time"
import type { APICreditAccount, APICredits } from "../lib/types"

/** The big figure's ink, from the level the server computed. */
const LEVEL_CLASS: Record<string, string> = {
  ok: "is-ok",
  low: "is-low",
  critical: "is-crit",
}

/** A figure's ink on a row. */
const LEVEL_INK: Record<string, string> = {
  low: "text-warn",
  critical: "text-crit",
}

/** A mark's chip, from its tone. A wait is amber like a low level; a refusal red like an empty one. */
const CHIP_TONE: Record<string, string> = {
  low: "qg-chip-low",
  warn: "qg-chip-low",
  critical: "qg-chip-critical",
  bad: "qg-chip-critical",
  quiet: "qg-chip-route",
}

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

/** An instant as a reading or a refusal is dated: "Oct 9, 13:20", in the reader's zone. */
const dateTime = (epoch: number) => `${formatDate(epoch)}, ${formatClock(epoch)}`

/** A note's sentence, with `{age}` filled in. */
function noteText(note: CreditNote, now: number): string {
  return note.since !== undefined ? note.text.replace("{age}", formatDuration(now - note.since)) : note.text
}

/** How far off a refill is, as the figures print it. */
function refillDistance(when: RefillWhen): string {
  switch (when.kind) {
    case "days":
      return `${when.days}d`
    case "within":
      return formatDuration(when.seconds)
    default:
      return ""
  }
}

/**
 * When the account's credit refills: the date and how far off it is,
 * "refilled today" on the first day of a cycle, or a dash with no date. A
 * date entered on Quota Cache's page carries the dot.
 */
function Refill({ account, now }: { account: APICreditAccount; now: number }) {
  const when = refillWhen(account.renewsAtEpoch, account.cycleStartEpoch, now)
  const dot = setHereTitles(account, formatUTCDate).refill
  if (when.kind === "refilledToday") {
    // The next date is a month off and says less than this does.
    return (
      <span
        className="text-accent"
        title={account.renewsAtEpoch !== null ? `Next refill ${formatUTCDate(account.renewsAtEpoch)}` : undefined}
      >
        refilled today
        {dot && <SetHereDot title={dot} />}
      </span>
    )
  }
  if (when.kind === "none" || account.renewsAtEpoch === null) {
    return (
      <>
        <span className="qg-short">refills </span>
        <span className="text-ink-4">—</span>
      </>
    )
  }
  return (
    <>
      <span className="qg-short">refills </span>
      <b>{formatUTCDate(account.renewsAtEpoch)}</b>
      {dot && <SetHereDot title={dot} />}
      {when.kind === "due" ? (
        " · due"
      ) : (
        <>
          {" · "}
          <span className="qg-long">in </span>
          <span className="num">{refillDistance(when)}</span>
        </>
      )}
    </>
  )
}

/** What a line's link to Quota Cache's page says. */
const LINK_TEXT: Record<CreditLink, string> = {
  reading: "Enter Console reading in Quota Cache",
  settings: "Set in Quota Cache",
}

/**
 * A line under a row or the pool, its tone a dot ahead of the words. A
 * refusal's time follows the sentence; a line that asks for a Console
 * reading or a setting ends with a link to Quota Cache's page, where it is
 * entered, while this browser may follow one (`link`).
 */
function Note({ note, now, link }: { note: CreditNote; now: number; link: string | null }) {
  return (
    <p className={`qg-cnote is-${note.tone}`}>
      {noteText(note, now)}
      {note.at !== undefined && ` · ${dateTime(note.at)}`}
      {note.link && link !== null && (
        <>
          {" "}
          <QuotaCacheLink url={link}>{LINK_TEXT[note.link]}</QuotaCacheLink>
        </>
      )}
    </p>
  )
}

/** The bar's hover: what the estimate starts from. */
function basisTitle(account: APICreditAccount): string | undefined {
  const basis = creditBasis(account)
  if (!basis) return undefined
  return basis.kind === "reading"
    ? `From your Console reading of ${basis.remainingText} on ${dateTime(basis.atEpoch)} (${formatUTCDate(basis.atEpoch)}, ${formatUTCClock(basis.atEpoch)}), less ≈ ${basis.spentSinceText} since`
    : `Monthly credit ${basis.monthlyCreditText} less ≈ ${basis.spentText} metered this cycle`
}

/**
 * One Console organization this cycle, on one line: its label and what is
 * wrong with it in a word, its own bar, what is left of its credit, what it
 * has used, and when the credit refills. A row with something to say earns a
 * line under it, unless the pool says it once for several rows; a condition
 * every row shares and the pool explains is not repeated beside each name.
 *
 * Every amount is the server's text, without the card's ≈: the column heads
 * say "est." once. A bound carries ≤ and ≥. With nothing to draw, the bar's
 * place says why and the amounts are dashes, never $0.00.
 */
function CreditRow({
  account,
  meter,
  shared,
  saidOnce,
  now,
  link,
}: {
  account: APICreditAccount
  meter: APICredits["meter"]
  shared: SharedIssue[]
  saidOnce: Set<string>
  now: number
  /** Quota Cache's page, for a line's link; null for none. */
  link: string | null
}) {
  const level = creditLevel(account)
  const marked = creditCondition(account, meter)
  const condition = marked && !saidOnce.has(conditionKey(marked)) ? marked : null
  const notes = creditNotes(account, shared)
  const bar = creditBar(account)
  const figures = creditFigures(account)
  const dot = setHereTitles(account, formatUTCDate).credit
  const aged = account.state === "stale"
  return (
    <div className={`qg-arow qg-crow ${aged ? "is-aged" : ""}`}>
      <span className="qg-awho">
        <span className="qg-aname" title={account.organizationId || undefined}>
          {creditName(account)}
        </span>
        {level && <span className={`qg-chip ${CHIP_TONE[level.tone]}`}>{level.figure && <><span className="num">{level.figure}</span>{" "}</>}{level.word}</span>}
        {condition && (condition.word || condition.since !== undefined) && (
          <span className={`qg-chip ${CHIP_TONE[condition.tone]}`}>
            {condition.since !== undefined
              ? `${condition.word ? `${condition.word} · ` : ""}${formatDuration(now - condition.since)} old`
              : condition.word}
          </span>
        )}
      </span>

      {bar.kind === "bar" ? (
        <span
          className={`qg-abar qg-lvl-${bar.level} ${bar.empty === "over" ? "is-over" : bar.empty === "out" ? "is-out" : ""}`}
          role="img"
          aria-label={
            figures.left !== null
              ? `${figures.leftMark ? "at most " : "about "}${figures.left} left${figures.of ? ` of ${figures.of}` : ""}${
                  figures.past ? `, ${figures.past} past it` : ""
                }`
              : "no estimate"
          }
          title={basisTitle(account)}
        >
          <i style={{ width: `${bar.fraction * 100}%` }} />
        </span>
      ) : (
        <span className="qg-nobar">{bar.text}</span>
      )}

      <span className="qg-cleft">
        {figures.left === null ? (
          <span className="text-ink-4">—</span>
        ) : (
          <>
            {figures.leftMark && <span className="qg-cmark">{figures.leftMark} </span>}
            <b className={`num ${LEVEL_INK[account.level] ?? ""}`}>{figures.left}</b>
            {figures.of ? (
              <>
                {" of "}
                <span className="num">{figures.of}</span>
              </>
            ) : (
              figures.leftAlone && " left"
            )}
            {dot && <SetHereDot title={dot} />}
          </>
        )}
      </span>

      <span className="qg-cspent">
        {figures.used === null ? (
          <span className="text-ink-4">—</span>
        ) : (
          <>
            {figures.usedMark && <span className="qg-cmark">{figures.usedMark} </span>}
            <b className="num">{figures.used}</b>
            {figures.usedWord === "spent" ? " spent" : <span className="qg-short"> used</span>}
            {figures.past && (
              <span className="qg-cpast">
                <span className="num">{figures.past}</span> past the credit
              </span>
            )}
          </>
        )}
      </span>

      <span className="qg-crnw">
        <Refill account={account} now={now} />
      </span>

      {notes.map((note, index) => (
        <Note key={index} note={note} now={now} link={link} />
      ))}
    </div>
  )
}

/**
 * The Claude API credit a Max or Team plan deposits each month into its
 * Console organization, pooled across every organization Quota Cache meters.
 *
 * Drawn like a window card — what is left, what the next refill restores,
 * when — because it is read the same way, but in money, and marked as an
 * estimate throughout. Every amount is Quota Cache's text, sized by its
 * fractions; nothing here adds dollars. The accounts fold shut, and the fold
 * line names those worth a look.
 *
 * Read-only. The credits, refill dates and Console readings are entered on
 * Quota Cache's page, and the foot links there (`link`) where this browser
 * may follow one; a line that asks for a value links there too. Without a
 * link the words stay, and say where.
 */
export function APICreditsCard({
  credits,
  open,
  onToggle,
  link,
}: {
  credits: APICredits
  open: boolean
  onToggle: () => void
  /** Quota Cache's page at its API credit section, or null where no link may be drawn. */
  link: string | null
}) {
  const now = useNowSeconds()
  const bodyID = useId()
  const titleID = useId()
  const countID = useId()
  const { pool, accounts, meter, editing } = credits

  const label = (id: string) => {
    const account = accounts.find((item) => item.id === id)
    return account ? creditName(account) : id
  }
  const refill = pool.nextRefill
  const who = refill ? recoveryNames(refill.accountIds.map(label)) : null
  const when = refill ? refillWhen(refill.refillAtEpoch, null, now) : null
  const distance = when && when.kind !== "due" ? refillDistance(when) : ""
  const whenText = distance ? `in ${distance}` : "now"
  const shared = sharedIssues(accounts)
  const notes = poolNotes(credits, now, shared)
  const saidOnce = conditionsSaidOnce(accounts, meter, shared)
  const flags = creditFlags(accounts, meter, (since) => formatDuration(now - since))
  const source = meterLine(meter, now)
  // The far end, when it is not the same instant as the next refill.
  const fullBy = pool.fullAtEpoch !== null && pool.fullAtEpoch !== refill?.refillAtEpoch ? pool.fullAtEpoch : null
  const over = pool.hasEstimate && pool.overage > 0
  const approx = pool.lowerBound ? "≤" : "≈"

  const barLabel = [
    `${pool.lowerBound ? "At most" : "About"} ${pool.leftText} left of ${pool.monthlyCreditText}${
      pool.countedCount > 1 ? ` across ${plural(pool.countedCount, "account")}` : ""
    }, estimated.`,
    refill && who ? `Plus ${refill.gainText} when ${who.who} ${who.plural ? "refill" : "refills"} ${whenText}.` : "",
  ]
    .filter(Boolean)
    .join(" ")

  // Empty when Quota Cache published no price table: it published no figures.
  const pricedOn = /^\d{4}-\d{2}-\d{2}$/.test(credits.pricing.asOf)
    ? formatUTCDate(dateEpoch(credits.pricing.asOf))
    : credits.pricing.asOf
  const trouble = editingTrouble(editing)

  return (
    <article className={`qg-win qg-api ${pool.level === "critical" ? "is-crit" : ""}`} aria-label="Monthly API Credit">
      <div className="qg-whead">
        <h3 id={titleID} className="qg-wtitle">
          Monthly API Credit
        </h3>
        <span className={`qg-src ${source.kind === "stale" || source.kind === "stopped" ? "text-warn" : ""}`}>
          Estimated from CPA traffic
          {source.kind === "updated" && (
            <>
              {" · updated "}
              <span className="num">{formatDuration(now - source.since)}</span> ago
            </>
          )}
          {source.kind === "stale" && (
            <>
              {" · not updated for "}
              <span className="num">{formatDuration(now - source.since)}</span>
            </>
          )}
          {source.kind === "stopped" && (
            <>
              {" · counting stopped "}
              <span className="num">{formatDuration(now - source.since)}</span> ago
            </>
          )}
        </span>
      </div>

      {/* No bar with nothing counted, as no row draws an empty one: a dash
        * says it below. */}
      {pool.hasEstimate && (
        <PoolBar
          level={pool.level}
          fraction={pool.remainingFraction}
          gain={refill?.gainFraction ?? 0}
          gainTitle="back at the next refill"
          label={barLabel}
        />
      )}

      <div className="qg-hfig">
        {/* A dash, never $0.00, when nothing is counted: "configured, nothing
          * counted yet" and "spent to nothing" are opposite facts. */}
        {pool.hasEstimate ? (
          <span
            className={`qg-big ${LEVEL_CLASS[pool.level] ?? ""}`}
            title={pool.lowerBound ? "At most this much: some spend may be missing" : "Estimated from CPA traffic"}
          >
            <span className="qg-approx">{approx}</span>
            <span className="num">{pool.leftText}</span>
            <span className="qg-unit">left</span>
          </span>
        ) : (
          <span className="qg-big">
            <span className="num text-ink-3">—</span>
          </span>
        )}
        {refill && who && (
          <p className="qg-rec" aria-hidden="true">
            <b className="qg-gain num">+{refill.gainText}</b> when <b className="qg-who">{who.who}</b>{" "}
            {who.plural ? "refill" : "refills"}{" "}
            {distance ? (
              <>
                in <b className="num">{distance}</b>
              </>
            ) : (
              "now"
            )}
          </p>
        )}
      </div>

      {pool.hasEstimate && (
        <div className="qg-hsub">
          {over ? (
            // Each account's overage is its own: it never takes from another's
            // credit, so the pool says how much was the credit and how much
            // was past it.
            <span>
              {pool.lowerBound ? "≥" : "≈"} <b className="num">{pool.usedText}</b> of the{" "}
              <b className="num">{pool.monthlyCreditText}</b> credit used,{" "}
              <b className="num text-crit">{pool.overageText}</b> past it
            </span>
          ) : (
            <span>
              {pool.lowerBound ? "≥" : "≈"} <b className="num">{pool.usedText}</b> used of{" "}
              <b className="num">{pool.monthlyCreditText}</b> this cycle
            </span>
          )}
          {fullBy !== null && (
            <span className="qg-full">
              all refilled by <b>{formatUTCDate(fullBy)}</b>
            </span>
          )}
        </div>
      )}

      {notes.map((note, index) => (
        <Note key={index} note={note} now={now} link={link} />
      ))}

      <FoldButton open={open} controls={bodyID} labelledBy={`${titleID} ${countID}`} onToggle={onToggle}>
        <span id={countID} className="qg-fold-line">
          <span className="qg-fold-count">{plural(accounts.length, "account")}</span>
          <Flags flags={flags} />
        </span>
      </FoldButton>

      <div id={bodyID} hidden={!open} className="qg-rows">
        {open && (
          <>
            {/* Column heads for the wide layout; in the popover each figure
              * carries its own word instead. */}
            <div className="qg-chead" aria-hidden="true">
              <span>Account</span>
              <span>Left of monthly credit (est.)</span>
              <span>Used this cycle (est.)</span>
              <span>Credit refills</span>
            </div>
            {/* Pre-sorted by configured order; not re-sorted here. */}
            {accounts.map((account) => (
              <CreditRow
                key={account.id}
                account={account}
                meter={meter}
                shared={shared}
                saidOnce={saidOnce}
                now={now}
                link={link}
              />
            ))}
            <div className="qg-afoot">
              {/* Without a link, the hover says where these are changed. */}
              <span title={link === null ? (editingOffText(editing) ?? undefined) : undefined}>
                {anySetHere(accounts) && (
                  <>
                    <SetHereDot lead />
                    <b>{ENTERED_HERE}</b>
                    {" · "}
                  </>
                )}
                Credits, refill dates and Console readings are set in Quota Cache
                {pricedOn && <> · spend is estimated from CPA traffic at Anthropic's list prices of {pricedOn}</>}
                {trouble && (
                  <>
                    <br />
                    {trouble}
                  </>
                )}
              </span>
              <QuotaCacheLink url={link}>Edit in Quota Cache</QuotaCacheLink>
            </div>
          </>
        )}
      </div>
    </article>
  )
}
