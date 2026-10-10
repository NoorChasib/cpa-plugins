import { useEffect, useId, useState } from "react"

import { Box, Dot, EditButton, EditFoot, EditLink, type FootStatus, RestFoot, SetHereDot } from "./Edit"
import { Flags, FoldButton } from "./Fold"
import { PoolBar } from "./WindowCard"
import {
  anySetHere,
  type CreditNote,
  creditBar,
  creditBasis,
  creditCondition,
  creditFigures,
  creditFlags,
  creditLevel,
  creditName,
  creditNotes,
  editingOffText,
  meterLine,
  notEditableText,
  poolNotes,
  type RefillWhen,
  refillWhen,
  setHere,
} from "../lib/apicredits"
import { useNowSeconds } from "../lib/now"
import { recoveryNames } from "../lib/pool"
import { canSaveHere, saveSettings } from "../lib/save"
import {
  type CreditDraft,
  type CreditField,
  type CreditRowState,
  CONFLICT_FIELD,
  conflictFields,
  creditPlan,
  creditRow,
  cycleOn,
  cycleText,
  dateEpoch,
  dropConflicts,
  EMPTY_CREDIT_DRAFT,
  editCredit,
  FIELD_FOOT,
  type FieldEdit,
  type FieldState,
  lateDayText,
  localInputValue,
  moneyText,
  newerRevision,
  READING_PICK_AGE,
  type ReadingEdit,
  type ReadingState,
  reconcileCredits,
  removeOrphan,
  SIGN_IN_FOOT,
  utcClock,
  utcDay,
} from "../lib/settings"
import { formatClock, formatDate, formatDuration, formatUTCDate } from "../lib/time"
import type { APICreditAccount, APICreditOrphan, APICredits, APICreditSettings } from "../lib/types"

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
 * date set here carries the dot.
 */
function Refill({ account, now }: { account: APICreditAccount; now: number }) {
  const when = refillWhen(account.renewsAtEpoch, account.cycleStartEpoch, now)
  const dot = setHereTitles(account).refill
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

/**
 * A line under a row or the pool, its tone a dot ahead of the words. A
 * refusal's time follows the sentence; a row a Console reading would settle
 * ends with the link that opens the editor on it.
 */
function Note({ note, now, onReading }: { note: CreditNote; now: number; onReading?: () => void }) {
  return (
    <p className={`qg-cnote is-${note.tone}`}>
      {noteText(note, now)}
      {note.at !== undefined && ` · ${dateTime(note.at)}`}
      {note.reading && onReading && (
        <>
          {" "}
          <EditLink onClick={onReading}>Enter Console reading</EditLink>
        </>
      )}
    </p>
  )
}

/** The hover on each value set here: what the config has instead. Null for a value that was not. */
function setHereTitles(account: APICreditAccount): { credit: string | null; refill: string | null } {
  const config = setHere(account)
  const title = (value: string | null, shown: (value: string) => string) =>
    value === null ? null : value ? `Set here. Quota Cache config says ${shown(value)}.` : "Set here. Not in Quota Cache config."
  return {
    credit: title(config.credit, moneyText),
    refill: title(config.refill, (value) => utcDay(dateEpoch(value))),
  }
}

/** The bar's hover: what the estimate starts from. */
function basisTitle(account: APICreditAccount): string | undefined {
  const basis = creditBasis(account)
  if (!basis) return undefined
  return basis.kind === "reading"
    ? `From your Console reading of ${basis.remainingText} on ${dateTime(basis.atEpoch)} (${utcDay(basis.atEpoch)}, ${utcClock(basis.atEpoch)}), less ≈ ${basis.spentSinceText} since`
    : `Monthly credit ${basis.monthlyCreditText} less ≈ ${basis.spentText} metered this cycle`
}

/**
 * One Console organization this cycle, on one line: its label and what is
 * wrong with it in a word, its own bar, what is left of its credit, what it
 * has used, and when the credit refills. A row with something to say earns a
 * line under it.
 *
 * Every amount is the server's text, without the card's ≈: the column heads
 * say "est." once. A bound carries ≤ and ≥. With nothing to draw, the bar's
 * place says why and the amounts are dashes, never $0.00.
 */
function CreditRow({
  account,
  meter,
  now,
  onReading,
}: {
  account: APICreditAccount
  meter: APICredits["meter"]
  now: number
  onReading: (() => void) | null
}) {
  const level = creditLevel(account)
  const condition = creditCondition(account, meter)
  const notes = creditNotes(account)
  const bar = creditBar(account)
  const figures = creditFigures(account)
  const dot = setHereTitles(account).credit
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
        <Note key={index} note={note} now={now} onReading={onReading ?? undefined} />
      ))}
    </div>
  )
}

// ---------------------------------------------------------------------------
// The editor

/** Field keys for marks: the batch's field names, as the plugin's errors name them. */
type MarkKey = "monthlyUsd" | "renews" | "reading.remainingUsd" | "reading.at"

/** A sentence a save left under one field: a conflict's, or the plugin's refusal of the value. */
type Marks = Record<string, { text: string; tone: "warn" | "bad" }>

const markOf = (marks: Marks, id: string, key: MarkKey) => marks[`${id}:${key}`] ?? null

/** Under a money or date field: where its value comes from, or what is about to happen to it. */
function FieldCap({
  state,
  kind,
  mark,
  hint,
  busy,
  onEdit,
}: {
  state: FieldState
  kind: "money" | "date"
  mark: { text: string; tone: "warn" | "bad" } | null
  hint: React.ReactNode
  busy: boolean
  onEdit: (edit: FieldEdit | undefined) => void
}) {
  const shown = (value: string) => (kind === "money" ? moneyText(value) : utcDay(dateEpoch(value)))
  const undo = (
    <EditLink disabled={busy} onClick={() => onEdit(undefined)}>
      Undo
    </EditLink>
  )
  const useConfig = (
    <EditLink disabled={busy} onClick={() => onEdit({ kind: "drop" })}>
      Use config
    </EditLink>
  )
  const base = state.stored || state.config
  if (state.error !== null) {
    return (
      <span className="qg-ecap is-bad">
        {state.ambiguous ? (
          <>
            A comma is ambiguous. Did you mean{" "}
            <EditLink label={`Use ${state.ambiguous.decimal}`} onClick={() => onEdit({ kind: "set", text: state.ambiguous!.decimal })}>
              {state.ambiguous.decimal}
            </EditLink>{" "}
            or{" "}
            <EditLink
              label={`Use ${state.ambiguous.thousands}`}
              onClick={() => onEdit({ kind: "set", text: state.ambiguous!.thousands })}
            >
              {state.ambiguous.thousands}
            </EditLink>
            ?
          </>
        ) : (
          state.error
        )}
        <Dot />
        {undo}
      </span>
    )
  }
  return (
    <>
      {mark && <span className={`qg-ecap is-${mark.tone}`}>{mark.text}</span>}
      {state.edit?.kind === "drop" ? (
        <span className="qg-ecap">
          <span className="qg-esrc is-dirty">{state.config ? "back to config" : "cleared"}</span>
          {state.config && <b className="num">{shown(state.config)}</b>}
          <Dot />
          {undo}
        </span>
      ) : state.changed ? (
        <>
          <span className="qg-ecap">
            <span className="qg-esrc is-dirty">changed</span>
            <Dot />
            <span>{base ? <>was <b className="num">{shown(base)}</b></> : "was not set"}</span>
            <Dot />
            {undo}
          </span>
          {/* A value set here keeps its config note while it is being changed
            * (must-fix 5): the reader may want the config's back instead. */}
          {state.source === "dashboard" && (
            <span className="qg-ecap">
              <span>
                {state.config ? <>config <b className="num">{shown(state.config)}</b></> : "not in config"}
              </span>
              <Dot />
              {useConfig}
            </span>
          )}
        </>
      ) : state.source === "dashboard" ? (
        <span className="qg-ecap">
          <span className="qg-esrc">
            <SetHereDot lead />
            set here
          </span>
          <Dot />
          {state.config ? (
            <span>
              config <b className="num">{shown(state.config)}</b>
            </span>
          ) : (
            <span>not in config</span>
          )}
          <Dot />
          {useConfig}
        </span>
      ) : state.source === "config" ? (
        <span className="qg-ecap">
          <span className="qg-esrc">from config</span>
          {state.edit !== undefined && (
            <>
              <Dot />
              {undo}
            </>
          )}
        </span>
      ) : (
        <span className="qg-ecap">
          <span className="qg-esrc">not set</span>
          {state.configInvalid && (
            <>
              <Dot />
              <span>{kind === "money" ? "the config's is not a dollar amount" : "the config's is not a date"}</span>
            </>
          )}
        </span>
      )}
      {hint}
    </>
  )
}

/**
 * A Console reading, in edit mode (E.7): what is stored and whether it is
 * used, with Add, Change and Clear; or the two fields of a new one, with what
 * the picked time is in UTC.
 */
function ReadingLine({
  id,
  name,
  state,
  now,
  marks,
  busy,
  fieldID,
  onEdit,
}: {
  id: string
  name: string
  state: ReadingState
  now: number
  marks: Marks
  busy: boolean
  fieldID: string
  onEdit: (edit: ReadingEdit | undefined) => void
}) {
  const { stored, edit } = state
  const mark = markOf(marks, id, "reading.remainingUsd") ?? markOf(marks, id, "reading.at")
  if (edit?.kind === "set") {
    const amount = state.amount!
    return (
      <div className="qg-eread is-open">
        <div className="qg-eread-fields">
          <label className="qg-efield">
            <span className="qg-elabel">Left in Console</span>
            <Box state={{ changed: true, dropped: false, bad: amount.error !== null, busy }} prefix="$">
              <input
                id={fieldID}
                type="text"
                inputMode="decimal"
                value={amount.text}
                disabled={busy}
                spellCheck={false}
                autoComplete="off"
                aria-invalid={amount.error !== null}
                aria-label={`${name}: amount left in Console, US dollars`}
                onChange={(event) => onEdit({ ...edit, amount: event.target.value })}
              />
            </Box>
          </label>
          <label className="qg-efield">
            <span className="qg-elabel">at</span>
            <Box state={{ changed: true, dropped: false, bad: state.atError !== null, busy }}>
              <input
                type="datetime-local"
                value={edit.at}
                min={localInputValue(now - READING_PICK_AGE)}
                max={localInputValue(now)}
                disabled={busy}
                aria-invalid={state.atError !== null}
                aria-label={`${name}: when Console showed it, your time`}
                onChange={(event) => onEdit({ ...edit, at: event.target.value })}
              />
            </Box>
          </label>
          <span className="qg-ecap qg-eread-utc">
            {state.at !== null && <span className="num">= {utcClock(state.at)}</span>}
            <Dot />
            <EditLink disabled={busy} onClick={() => onEdit(undefined)}>
              {stored ? "Keep the saved one" : "Cancel"}
            </EditLink>
          </span>
        </div>
        {amount.error !== null && (
          <span className="qg-ecap is-bad">
            {amount.ambiguous ? (
              <>
                A comma is ambiguous. Did you mean{" "}
                <EditLink onClick={() => onEdit({ ...edit, amount: amount.ambiguous!.decimal })}>{amount.ambiguous.decimal}</EditLink> or{" "}
                <EditLink onClick={() => onEdit({ ...edit, amount: amount.ambiguous!.thousands })}>
                  {amount.ambiguous.thousands}
                </EditLink>
                ?
              </>
            ) : (
              amount.error
            )}
          </span>
        )}
        {state.atError !== null && <span className="qg-ecap is-bad">{state.atError}</span>}
        {mark && <span className={`qg-ecap is-${mark.tone}`}>{mark.text}</span>}
        {state.onRefillDay && (
          <span className="qg-ecap">Today is the refill day. Enter the reading after the new credit shows in Console.</span>
        )}
        <span className="qg-ehint">From Console → Settings → Billing, Promotional credits: the amount left.</span>
      </div>
    )
  }

  const add = () => onEdit({ kind: "set", amount: "", at: localInputValue(now) })
  const change = () =>
    onEdit({ kind: "set", amount: stored ? stored.remainingUsd : "", at: localInputValue(stored ? stored.atEpoch : now) })
  return (
    <div className="qg-eread">
      <span className="qg-ecap">
        <span className="qg-elabel">Console reading</span>
        <Dot />
        {edit?.kind === "clear" ? (
          <>
            <span className="qg-esrc is-dirty">cleared</span>
            <Dot />
            <EditLink disabled={busy} onClick={() => onEdit(undefined)}>
              Undo
            </EditLink>
          </>
        ) : stored === null ? (
          <>
            <span>none this cycle</span>
            <Dot />
            <EditLink disabled={busy} onClick={add}>
              Add
            </EditLink>
          </>
        ) : (
          <>
            <span>
              <b className="num">{moneyText(stored.remainingUsd)}</b> {unusedText(state, stored.atEpoch)}
            </span>
            {state.unusedReason === "" && (
              <>
                <Dot />
                <EditLink disabled={busy} onClick={change}>
                  Change
                </EditLink>
              </>
            )}
            <Dot />
            <EditLink disabled={busy} onClick={() => onEdit({ kind: "clear" })}>
              Clear
            </EditLink>
          </>
        )}
      </span>
      {mark && <span className={`qg-ecap is-${mark.tone}`}>{mark.text}</span>}
    </div>
  )
}

/** A stored reading's date, and why it is not used when it is not (E.7). */
function unusedText(state: ReadingState, at: number): string {
  switch (state.unusedReason) {
    case "":
      return `at ${dateTime(at)} · ${utcDay(at)}, ${utcClock(at)}`
    case "beforeRefill":
      return `on ${utcDay(at)}, before the last refill; not used`
    case "otherOrganization":
      return "for another organization-id; not used"
    case "tooOld":
      return "over 31 days old; not used"
    case "future":
      return "dated in the future; not used"
    default:
      return `on ${utcDay(at)}; not used`
  }
}

/** One organization as fields: monthly credit, refill date, then its Console reading. */
function CreditEditRow({
  account,
  settings,
  row,
  now,
  marks,
  busy,
  idOf,
  focused,
  onFocus,
  onEdit,
}: {
  account: APICreditAccount
  settings: APICreditSettings
  row: CreditRowState
  now: number
  marks: Marks
  busy: boolean
  idOf: (field: string) => string
  focused: string | null
  onFocus: (field: string | null) => void
  onEdit: (key: CreditField, edit: FieldEdit | ReadingEdit | undefined) => void
}) {
  const name = creditName(account)
  const who = (
    <span className="qg-awho">
      <span className="qg-aname" title={account.organizationId || undefined}>
        {name}
      </span>
    </span>
  )
  if (!settings.editable) {
    return (
      <div className="qg-arow qg-erow is-locked">
        {who}
        <span className="qg-elock">{notEditableText(settings.notEditableReason) ?? "This organization cannot be edited here."}</span>
      </div>
    )
  }
  const { monthlyUsd, renews } = row
  const refill = renews.error === null ? (renews.edit?.kind === "drop" ? renews.config : renews.desired ?? renews.text) : ""
  const cycle = refill ? cycleOn(refill, now) : null
  const late = refill ? lateDayText(refill) : null
  const dateHint =
    focused === "renews" && (cycle || late) ? (
      <span className="qg-ehint">
        {cycle && cycleText(cycle)}
        {cycle && late && <br />}
        {late}
      </span>
    ) : null
  return (
    <div className="qg-arow qg-erow">
      {who}
      <div className="qg-efield is-credit">
        <Box
          state={{
            changed: monthlyUsd.changed,
            dropped: monthlyUsd.edit?.kind === "drop",
            bad: monthlyUsd.error !== null || markOf(marks, account.id, "monthlyUsd")?.tone === "bad",
            busy,
          }}
          prefix="$"
        >
          <input
            id={idOf("monthlyUsd")}
            type="text"
            inputMode="decimal"
            value={monthlyUsd.text}
            placeholder="not set"
            disabled={busy}
            spellCheck={false}
            autoComplete="off"
            aria-invalid={monthlyUsd.error !== null}
            aria-label={`${name}: monthly credit, US dollars`}
            onChange={(event) => onEdit("monthlyUsd", { kind: "set", text: event.target.value })}
          />
        </Box>
        <FieldCap
          state={monthlyUsd}
          kind="money"
          mark={markOf(marks, account.id, "monthlyUsd")}
          hint={null}
          busy={busy}
          onEdit={(next) => onEdit("monthlyUsd", next)}
        />
      </div>
      <div className="qg-efield is-date">
        <Box
          state={{
            changed: renews.changed,
            dropped: renews.edit?.kind === "drop",
            bad: renews.error !== null || markOf(marks, account.id, "renews")?.tone === "bad",
            busy,
          }}
        >
          <input
            id={idOf("renews")}
            type="date"
            value={renews.text}
            min="2000-01-01"
            max="2099-12-31"
            disabled={busy}
            aria-invalid={renews.error !== null}
            aria-label={`${name}: credit refill date, UTC`}
            onFocus={() => onFocus("renews")}
            onBlur={() => onFocus(null)}
            onChange={(event) =>
              onEdit("renews", { kind: "set", text: event.target.value, incomplete: event.target.validity.badInput || undefined })
            }
          />
        </Box>
        <FieldCap
          state={renews}
          kind="date"
          mark={markOf(marks, account.id, "renews")}
          hint={dateHint}
          busy={busy}
          onEdit={(next) => onEdit("renews", next)}
        />
      </div>
      <ReadingLine
        id={account.id}
        name={name}
        state={row.reading}
        now={now}
        marks={marks}
        busy={busy}
        fieldID={idOf("reading")}
        onEdit={(next) => onEdit("reading", next)}
      />
    </div>
  )
}

/** An orphan as the editor's last line: values stored for an organization no longer configured (E.8 #4). */
function OrphanLine({
  orphan,
  removing,
  busy,
  onRemove,
}: {
  orphan: APICreditOrphan
  removing: boolean
  busy: boolean
  onRemove: (remove: boolean) => void
}) {
  return (
    <p className="qg-eorphan">
      Saved values for an organization no longer in Quota Cache's config: <span className="num">{orphan.id}</span>
      <Dot />
      {removing ? (
        <>
          <span className="qg-esrc is-dirty">removing</span>
          <Dot />
          <EditLink disabled={busy} onClick={() => onRemove(false)}>
            Undo
          </EditLink>
        </>
      ) : (
        <EditLink disabled={busy} onClick={() => onRemove(true)}>
          Remove
        </EditLink>
      )}
    </p>
  )
}

/**
 * The Claude API credit a Max or Team plan deposits each month into its
 * Console organization, pooled across every organization Quota Cache meters.
 *
 * Drawn like a window card — what is left, what the next refill restores,
 * when — because it is read the same way, but in money, and marked as an
 * estimate throughout. Every amount is the server's text, sized by the
 * server's fractions; nothing here adds dollars. The accounts fold shut, and
 * the fold line names those worth a look.
 *
 * Edit credits & dates turns the open list into fields in place (option A).
 * The draft lives here, outside the fold's body, so folding keeps it; a
 * refreshed document moves every field the reader has not touched. One Save
 * sends every changed row as one batch, which the plugin applies all or
 * nothing.
 */
export function APICreditsCard({
  credits,
  open,
  onToggle,
  onSaved,
}: {
  credits: APICredits
  open: boolean
  onToggle: () => void
  /** Re-reads the document after a save, or after one whose answer was lost. */
  onSaved: () => void
}) {
  const now = useNowSeconds()
  const bodyID = useId()
  const titleID = useId()
  const countID = useId()
  const fieldPrefix = useId()
  const { pool, accounts, meter, editing: editingOffer } = credits

  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState<CreditDraft>(EMPTY_CREDIT_DRAFT)
  // What a conflict said each row holds now, until the document catches up.
  const [overlay, setOverlay] = useState<Record<string, APICreditSettings>>({})
  const [marks, setMarks] = useState<Marks>({})
  const [status, setStatus] = useState<FootStatus | null>(null)
  const [saved, setSaved] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [failed, setFailed] = useState(false)
  const [focused, setFocused] = useState<string | null>(null)
  const [focusNext, setFocusNext] = useState<string | null>(null)

  // A refreshed document: rows for accounts no longer listed are dropped and
  // said so, and a conflict's values give way once the document has them.
  useEffect(() => {
    setDraft((held) => {
      const { draft: next, dropped } = reconcileCredits(held, credits.accounts, credits.orphans)
      if (dropped.length > 0) setStatus({ tone: "warn", text: dropped.join(" ") })
      return next
    })
    setOverlay((held) => {
      const next = { ...held }
      let changed = false
      for (const account of credits.accounts) {
        const kept = next[account.id]
        if (kept && !newerRevision(kept.revision, account.settings.revision)) {
          delete next[account.id]
          changed = true
        }
      }
      return changed ? next : held
    })
  }, [credits])

  // A saved line lasts long enough to be read, then the foot says what it
  // always says.
  useEffect(() => {
    if (saved === null) return
    const timer = window.setTimeout(() => setSaved(null), 20_000)
    return () => window.clearTimeout(timer)
  }, [saved])

  // Focus the field an "Enter Console reading" link asked for, once it is on
  // screen.
  useEffect(() => {
    if (focusNext === null || !editing || !open) return
    const element = document.getElementById(focusNext)
    if (element) {
      element.focus()
      element.scrollIntoView({ block: "center" })
      setFocusNext(null)
    }
  }, [focusNext, editing, open])

  const settingsOf = (account: APICreditAccount): APICreditSettings => {
    const kept = overlay[account.id]
    return kept && newerRevision(kept.revision, account.settings.revision)
      ? { ...kept, editable: account.settings.editable, notEditableReason: account.settings.notEditableReason }
      : account.settings
  }
  const fieldID = (id: string, field: string) => `${fieldPrefix}-${id}-${field}`

  const doorOpen = canSaveHere()
  const canEdit = editingOffer.available && doorOpen
  const plan = creditPlan(accounts, draft, now, settingsOf)
  const unsaved = plan.changes

  const label = (id: string) => {
    const account = accounts.find((item) => item.id === id)
    return account ? creditName(account) : id
  }
  const refill = pool.nextRefill
  const who = refill ? recoveryNames(refill.accountIds.map(label)) : null
  const when = refill ? refillWhen(refill.refillAtEpoch, null, now) : null
  const distance = when && when.kind !== "due" ? refillDistance(when) : ""
  const whenText = distance ? `in ${distance}` : "now"
  const notes = poolNotes(credits, now)
  const flags = creditFlags(accounts, meter, (since) => formatDuration(now - since))
  const source = meterLine(meter, now)
  // The far end, when it is not the same instant as the next refill.
  const fullBy = pool.fullAtEpoch !== null && pool.fullAtEpoch !== refill?.refillAtEpoch ? pool.fullAtEpoch : null
  const over = pool.hasEstimate && pool.overage > 0
  const approx = pool.lowerBound ? "≤" : "≈"

  const barLabel = pool.hasEstimate
    ? [
        `${pool.lowerBound ? "At most" : "About"} ${pool.leftText} left of ${pool.monthlyCreditText}${
          pool.countedCount > 1 ? ` across ${plural(pool.countedCount, "account")}` : ""
        }, estimated.`,
        refill && who
          ? `Plus ${refill.gainText} when ${who.who} ${who.plural ? "refill" : "refills"} ${whenText}.`
          : "",
      ]
        .filter(Boolean)
        .join(" ")
    : "Nothing counted yet."

  const startEditing = () => {
    setSaved(null)
    setStatus(null)
    setFailed(false)
    setEditing(true)
  }

  const cancel = () => {
    setDraft(EMPTY_CREDIT_DRAFT)
    setMarks({})
    setStatus(null)
    setFailed(false)
    setEditing(false)
  }

  const edit = (account: APICreditAccount, key: CreditField, next: FieldEdit | ReadingEdit | undefined) => {
    setDraft((held) => editCredit(held, { id: account.id, settings: settingsOf(account) }, creditName(account), key, next))
    // A mark is about the value it was left under; a new one replaces it.
    setMarks((held) => {
      const prefix = key === "reading" ? `${account.id}:reading` : `${account.id}:${key}`
      const kept = Object.fromEntries(Object.entries(held).filter(([at]) => !at.startsWith(prefix)))
      return Object.keys(kept).length === Object.keys(held).length ? held : kept
    })
  }

  // The link under a row that a Console reading would settle: the editor,
  // open on that row's reading.
  const enterReading = (account: APICreditAccount) => {
    startEditing()
    if (draft.rows[account.id]?.reading?.kind !== "set") {
      edit(account, "reading", { kind: "set", amount: "", at: localInputValue(now) })
    }
    setFocusNext(fieldID(account.id, "reading"))
  }

  const save = async () => {
    if (plan.batch === null || saving) return
    setSaving(true)
    setStatus(null)
    setMarks({})
    const outcome = await saveSettings(plan.batch)
    setSaving(false)
    switch (outcome.kind) {
      case "saved":
        setDraft(EMPTY_CREDIT_DRAFT)
        setFailed(false)
        setEditing(false)
        setSaved(
          outcome.unchanged
            ? "Already saved. Nothing needed changing."
            : `Saved ${plural(plan.changes, "change")}. The figures above use ${plan.changes === 1 ? "it" : "them"}.`,
        )
        onSaved()
        return
      case "conflict": {
        // Each conflicting row takes what is stored now, and says so under
        // the fields that moved; every other row's draft is kept.
        const nextMarks: Marks = {}
        const nextOverlay = { ...overlay }
        for (const id of outcome.ids) {
          const account = accounts.find((item) => item.id === id)
          const current = outcome.current[id]
          if (account && current && typeof current === "object") {
            const block = current as APICreditSettings
            for (const field of conflictFields(settingsOf(account), block, draft.rows[id])) {
              nextMarks[`${id}:${field === "reading" ? "reading.remainingUsd" : field}`] = { text: CONFLICT_FIELD, tone: "warn" }
            }
            nextOverlay[id] = block
          }
        }
        const next = dropConflicts(draft, outcome.ids)
        setDraft(next.draft)
        setOverlay(nextOverlay)
        setMarks(nextMarks)
        setStatus({ tone: "bad", text: outcome.text })
        // Try again only while a kept row is left to send.
        setFailed(next.retry)
        onSaved()
        return
      }
      case "field":
        setMarks({ [`${outcome.id}:${outcome.field}`]: { text: outcome.text, tone: "bad" } })
        setStatus({ tone: "bad", text: FIELD_FOOT })
        setFailed(true)
        return
      case "failed":
        setStatus({ tone: "bad", text: outcome.text })
        setFailed(true)
        return
      case "lost":
        setStatus({ tone: "unknown", text: outcome.text })
        setFailed(true)
        onSaved()
        return
    }
  }

  const blocked = !editingOffer.available ? editingOffText(editingOffer) : !doorOpen ? SIGN_IN_FOOT
    : plan.tooLarge ? "Too many changes for one save; undo some, save, then make the rest." : null
  const pricedOn = /^\d{4}-\d{2}-\d{2}$/.test(credits.pricing.asOf) ? utcDay(dateEpoch(credits.pricing.asOf)) : credits.pricing.asOf

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

      <PoolBar
        level={pool.level}
        fraction={pool.remainingFraction}
        gain={refill?.gainFraction ?? 0}
        gainTitle="back at the next refill"
        label={barLabel}
      />

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
        <Note key={index} note={note} now={now} />
      ))}

      <FoldButton open={open} controls={bodyID} labelledBy={`${titleID} ${countID}`} onToggle={onToggle}>
        <span id={countID} className="qg-fold-line">
          <span className="qg-fold-count">{plural(accounts.length, "account")}</span>
          {editing && (
            <span className="qg-fold-note">
              {" · "}
              <span className={`qg-estate ${failed ? "is-bad" : ""}`}>{failed ? "not saved" : "editing"}</span>
              {unsaved > 0 && ` · ${unsaved} unsaved`}
            </span>
          )}
          <Flags flags={flags} />
        </span>
      </FoldButton>

      <div id={bodyID} hidden={!open} className="qg-rows">
        {open &&
          (editing ? (
            <>
              <form
                className="qg-eform"
                aria-label="Monthly credits, refill dates and Console readings"
                onSubmit={(event) => {
                  event.preventDefault()
                  void save()
                }}
              >
                <div className="qg-ehead" aria-hidden="true">
                  <span>Account</span>
                  <span>Monthly credit, USD</span>
                  <span>Refills on, UTC</span>
                </div>
                {accounts.map((account) => {
                  const settings = settingsOf(account)
                  const held = draft.rows[account.id]
                  return (
                    <CreditEditRow
                      key={account.id}
                      account={account}
                      settings={settings}
                      row={creditRow(settings, held, now)}
                      now={now}
                      marks={marks}
                      busy={saving}
                      idOf={(field) => fieldID(account.id, field)}
                      focused={focused?.startsWith(`${account.id}:`) ? focused.slice(account.id.length + 1) : null}
                      onFocus={(field) => setFocused(field === null ? null : `${account.id}:${field}`)}
                      onEdit={(key, next) => edit(account, key, next)}
                    />
                  )
                })}
                {credits.orphans.map((orphan) => (
                  <OrphanLine
                    key={orphan.id}
                    orphan={orphan}
                    removing={orphan.id in draft.remove}
                    busy={saving}
                    onRemove={(remove) => setDraft((held) => removeOrphan(held, orphan, remove))}
                  />
                ))}
              </form>
              <EditFoot
                help={
                  <>
                    A refill date repeats on that day every month, at 00:00 UTC. Labels and organization-ids stay in Quota
                    Cache's config.
                  </>
                }
                changes={unsaved}
                errors={plan.errors}
                saving={saving}
                blocked={blocked}
                status={status}
                failed={failed}
                onCancel={cancel}
                onSave={() => void save()}
              />
            </>
          ) : (
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
                  now={now}
                  onReading={canEdit && account.settings.editable ? () => enterReading(account) : null}
                />
              ))}
              {status && !editing && <p className={`qg-cnote is-${status.tone === "warn" ? "warn" : "quiet"}`}>{status.text}</p>}
              <RestFoot
                saved={saved}
                action={
                  canEdit ? <EditButton onClick={startEditing}>Edit credits &amp; dates</EditButton> : null
                }
              >
                {anySetHere(accounts) && (
                  <>
                    <SetHereDot lead />
                    <b>set here</b>
                    {" · "}
                  </>
                )}
                Credits, refill dates and Console readings are set by you · spend is estimated from CPA traffic at
                Anthropic's list prices of {pricedOn}
                {!editingOffer.available && (
                  <>
                    <br />
                    {editingOffText(editingOffer)}
                  </>
                )}
              </RestFoot>
            </>
          ))}
      </div>
    </article>
  )
}
