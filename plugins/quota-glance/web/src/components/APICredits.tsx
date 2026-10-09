import { useId } from "react"

import { Flags, FoldButton } from "./Fold"
import { PoolBar } from "./WindowCard"
import {
  type CreditNote,
  creditCondition,
  creditFlags,
  creditLevel,
  creditName,
  creditNoBar,
  creditNotes,
  lastRead,
  poolNotes,
  type RefillWhen,
  refillWhen,
} from "../lib/apicredits"
import { useNowSeconds } from "../lib/now"
import { recoveryNames } from "../lib/pool"
import { formatDuration, formatUTCDate } from "../lib/time"
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

/** A note's sentence, with `{age}` filled in the same way. */
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
 * When the account's credit refills: the configured date and how far off it
 * is, "refilled today" on the first day of a cycle, or a dash when the
 * configuration has no usable date — set or not, which the row's note says.
 */
function Refill({ account, now }: { account: APICreditAccount; now: number }) {
  const when = refillWhen(account.renewsAtEpoch, account.cycleStartEpoch, now)
  if (when.kind === "refilledToday") {
    // The next date is a month off and says less than this does: the spend
    // beside it is one day's.
    return (
      <span
        className="text-accent"
        title={account.renewsAtEpoch !== null ? `Next refill ${formatUTCDate(account.renewsAtEpoch)}` : undefined}
      >
        refilled today
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

/** A line under a row or the pool, its tone a dot ahead of the words. */
function Note({ note, now }: { note: CreditNote; now: number }) {
  return <p className={`qg-cnote is-${note.tone}`}>{noteText(note, now)}</p>
}

/**
 * One Console organization this cycle, on one line: its label and what is
 * wrong with it in a word, its own bar, what is left of its credit, what it
 * has spent, and when the credit refills. A row with something to say earns
 * a line under it.
 *
 * Every amount is the server's text. With no reading there is nothing to
 * draw: the bar's place says why, and the amounts are dashes, never $0.00.
 */
function CreditRow({ account, now }: { account: APICreditAccount; now: number }) {
  const level = creditLevel(account)
  const condition = creditCondition(account)
  const notes = creditNotes(account)
  const aged = condition?.since !== undefined
  const over = account.hasReading && account.overage > 0
  return (
    <div className={`qg-arow qg-crow ${aged ? "is-aged" : ""}`}>
      <span className="qg-awho">
        <span className="qg-aname" title={account.organizationId || undefined}>
          {creditName(account)}
        </span>
        {level && <span className={`qg-chip ${CHIP_TONE[level.tone]}`}>{level.word}</span>}
        {/* An aged reading's age is said in the note under the row, which
          * says why as well, so the chip carries only a word: none for a
          * reading that is merely old, the failure's for one someone must fix. */}
        {condition?.word && <span className={`qg-chip ${CHIP_TONE[condition.tone]}`}>{condition.word}</span>}
      </span>

      {account.hasReading ? (
        // Past the credit, the track itself turns, so an overage is not drawn
        // as merely empty.
        <span
          className={`qg-abar qg-lvl-${account.level} ${over ? "is-over" : ""}`}
          role="img"
          aria-label={`${account.leftText} left of ${account.monthlyCreditText}${
            over ? `, ${account.overageText} past it` : ""
          }`}
        >
          <i style={{ width: `${Math.max(0, Math.min(1, account.remainingFraction)) * 100}%` }} />
        </span>
      ) : (
        <span className="qg-nobar">{creditNoBar(account)}</span>
      )}

      <span className="qg-cleft">
        {account.hasReading ? (
          <b className={`num ${LEVEL_INK[account.level] ?? ""}`}>{account.leftText}</b>
        ) : (
          <span className="text-ink-4">—</span>
        )}
        {account.monthlyCreditText && (
          <>
            {" of "}
            <span className="num">{account.monthlyCreditText}</span>
          </>
        )}
      </span>

      <span className="qg-cspent">
        {account.hasReading ? (
          <>
            <b className="num">{account.spentText}</b>
            <span className="qg-short"> spent</span>
          </>
        ) : (
          <span className="text-ink-4">—</span>
        )}
      </span>

      <span className="qg-crnw">
        <Refill account={account} now={now} />
      </span>

      {notes.map((note, index) => (
        <Note key={index} note={note} now={now} />
      ))}
    </div>
  )
}

/**
 * The Claude API credit a Max or Team plan deposits each month into its
 * Console organization, pooled across every organization Quota Cache reads.
 *
 * Drawn like a window card — what is left, what the next refill restores,
 * when — because it is read the same way, but in money. Every amount is the
 * server's text, sized by the server's fractions; nothing here adds dollars.
 * The accounts fold shut, and the fold line names those worth a look.
 */
export function APICreditsCard({
  credits,
  open,
  onToggle,
}: {
  credits: APICredits
  open: boolean
  onToggle: () => void
}) {
  const now = useNowSeconds()
  const bodyID = useId()
  const titleID = useId()
  const countID = useId()
  const { pool, accounts } = credits

  const label = (id: string) => {
    const account = accounts.find((item) => item.id === id)
    return account ? creditName(account) : id
  }
  const refill = pool.nextRefill
  const who = refill ? recoveryNames(refill.accountIds.map(label)) : null
  const when = refill ? refillWhen(refill.refillAtEpoch, null, now) : null
  const distance = when && when.kind !== "due" ? refillDistance(when) : ""
  const whenText = distance ? `in ${distance}` : "now"
  const read = lastRead(accounts)
  const notes = poolNotes(credits)
  const flags = creditFlags(accounts, (since) => formatDuration(now - since))
  // The far end, when it is not the same instant as the next refill.
  const fullBy = pool.fullAtEpoch !== null && pool.fullAtEpoch !== refill?.refillAtEpoch ? pool.fullAtEpoch : null
  const over = pool.hasReading && pool.overage > 0

  const barLabel = pool.hasReading
    ? [
        `${pool.leftText} left of ${pool.monthlyCreditText}${
          pool.countedCount > 1 ? ` across ${plural(pool.countedCount, "account")}` : ""
        }.`,
        refill && who
          ? `Plus ${refill.gainText} when ${who.who} ${who.plural ? "refill" : "refills"} ${whenText}.`
          : "",
      ]
        .filter(Boolean)
        .join(" ")
    : "Nothing counted yet."

  return (
    <article
      className={`qg-win qg-api ${pool.level === "critical" ? "is-crit" : ""}`}
      aria-label="Monthly API Credit"
    >
      <div className="qg-whead">
        <h3 id={titleID} className="qg-wtitle">
          Monthly API Credit
        </h3>
        <span className="qg-src">
          Anthropic cost report
          {read !== null && (
            <>
              {" · updated "}
              <span className="num">{formatDuration(now - read)}</span> ago
            </>
          )}
        </span>
      </div>

      <PoolBar
        level={pool.level}
        fraction={pool.remainingFraction}
        gain={refill?.gainFraction ?? 0}
        gainTitle="back at the next refill"
        label={barLabel}
      />

      <div className="qg-hfig">
        {/* A dash, never $0.00, when nothing is counted: "configured, nothing
          * read yet" and "spent to nothing" are opposite facts. */}
        {pool.hasReading ? (
          <span className={`qg-big ${LEVEL_CLASS[pool.level] ?? ""}`}>
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

      {pool.hasReading && (
        <div className="qg-hsub">
          {over ? (
            // Each account's overage is its own: it never takes from another's
            // credit, so the pool says how much was the credit and how much
            // was past it rather than one sum that reads as all credit.
            <span>
              <b className="num">{pool.spentText}</b> spent this cycle: <b className="num">{pool.creditUsedText}</b>{" "}
              of the <b className="num">{pool.monthlyCreditText}</b> credit,{" "}
              <b className="num text-crit">{pool.overageText}</b> past it
            </span>
          ) : (
            <span>
              <b className="num">{pool.spentText}</b> spent of <b className="num">{pool.monthlyCreditText}</b> this
              cycle
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
        <Note key={index} note={note} now={now} />
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
              <span>Left of monthly credit</span>
              <span>Spent this cycle</span>
              <span>Credit refills</span>
            </div>
            {/* Pre-sorted by configured order; not re-sorted here. */}
            {accounts.map((account) => (
              <CreditRow key={account.id} account={account} now={now} />
            ))}
            <p className="qg-cfoot">
              Monthly credits and refill dates come from your Quota Cache configuration · spend from the Anthropic
              cost report
            </p>
          </>
        )}
      </div>
    </article>
  )
}
