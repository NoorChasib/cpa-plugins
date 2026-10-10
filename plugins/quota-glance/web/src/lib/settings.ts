// What a reader types into the editor, checked as the plugin will check it,
// and turned into one batch per Save.
//
// Two cards edit: the Monthly API Credit card (each organization's monthly
// credit, refill date and Console reading) and Claude's Accounts card (each
// subscription's renewal date). Each keeps a draft of what the reader changed
// and nothing else, so a refreshed document moves every field the reader has
// not touched and leaves the typed ones alone. Save sends the draft's rows as
// one batch, which the plugin applies all or nothing (internal/overrides).
//
// The plugin is authoritative. Every rule here mirrors one it enforces, so a
// value it would refuse is said under its own field before anything is sent;
// one it refuses anyway comes back as a code, and is said the same way.
//
// Imports nothing but types, so node loads it directly in test/.

import type { SaveAnswer, SettingsBatch } from "./access.ts"
import type { APICreditAccount, APICreditOrphan, APICreditSettings, Credential, RenewalOrphan, RenewalSetting } from "./types.ts"

// ---------------------------------------------------------------------------
// Money

/** qc.ValidMonthlyUSD: 1 to 7 whole digits, at most 2 decimals, zero allowed. */
const MONTHLY_USD = /^[0-9]{1,7}(\.[0-9]{1,2})?$/

/** A dollar amount as typed, read. */
export type MoneyInput =
  | { kind: "empty" }
  | { kind: "ok"; value: string }
  /** A comma before one or two final digits: cents, or thousands? Both readings, ready to print. */
  | { kind: "ambiguous"; decimal: string; thousands: string }
  | { kind: "invalid" }

const groupThousands = (digits: string): string => digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",")

/**
 * Reads a dollar amount the way a reader types one: a leading `$`, spaces and
 * thousands commas are dropped (`$1,200.50` is `1200.50`); what is left must
 * be what the plugin accepts. A comma followed by one or two final digits —
 * `12,50`, `1,2` — is cents in some places and thousands in others, so it is
 * not guessed at: both readings come back for the reader to pick.
 */
export function readMoney(input: string): MoneyInput {
  let text = input.trim()
  if (text.startsWith("$")) text = text.slice(1)
  text = text.replace(/\s+/g, "")
  if (text === "") return { kind: "empty" }
  if (text.includes(",")) {
    const tail = /^([0-9,]*),([0-9]{1,2})$/.exec(text)
    if (tail) {
      const whole = tail[1]!.replace(/,/g, "").replace(/^0+(?=\d)/, "")
      return {
        kind: "ambiguous",
        decimal: `${whole === "" ? "0" : whole}.${tail[2]!.padEnd(2, "0")}`,
        thousands: `${groupThousands(text.replace(/,/g, "").replace(/^0+(?=\d)/, ""))}.00`,
      }
    }
    if (!/^[0-9]{1,3}(,[0-9]{3})+(\.[0-9]*)?$/.test(text)) return { kind: "invalid" }
    text = text.replace(/,/g, "")
  }
  return MONTHLY_USD.test(text) ? { kind: "ok", value: text } : { kind: "invalid" }
}

/** A valid amount as the page prints money: `1200.5` is `$1,200.50`. String work only; no float touches it. */
export function moneyText(value: string): string {
  const [whole = "0", cents = ""] = value.split(".")
  return `$${groupThousands(whole.replace(/^0+(?=\d)/, ""))}.${cents.padEnd(2, "0")}`
}

/** Whether two valid amounts are the same number of cents: `200` and `200.00` are. */
export function sameAmount(a: string, b: string): boolean {
  const cents = (value: string) => {
    const [whole = "0", fraction = ""] = value.split(".")
    return Number(whole) * 100 + Number(fraction.padEnd(2, "0"))
  }
  return MONTHLY_USD.test(a) && MONTHLY_USD.test(b) && cents(a) === cents(b)
}

// ---------------------------------------------------------------------------
// Dates

const MONTH_NAMES = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
]

/** A date as typed, read. */
export type DateInput = { kind: "empty" } | { kind: "ok"; value: string } | { kind: "invalid"; message: string }

const daysIn = (year: number, month: number): number => new Date(Date.UTC(year, month, 0)).getUTCDate()

/**
 * Reads a YYYY-MM-DD date as the plugin does (qc.ParseRenewal's date-only
 * form): a day that exists, in 2000 to 2099. The native date control cannot
 * produce an impossible day, but typing or pasting into a browser without one
 * can, and the plugin's refusal of one is said in the same words.
 *
 * `incomplete` is the control's own report of a date half typed, which it
 * hands over as an empty value.
 */
export function readDate(input: string, incomplete = false): DateInput {
  if (incomplete) return { kind: "invalid", message: "Enter a full date." }
  const text = input.trim()
  if (text === "") return { kind: "empty" }
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(text)
  if (!match) return { kind: "invalid", message: "Enter a full date." }
  const [year, month, day] = [Number(match[1]), Number(match[2]), Number(match[3])]
  if (month < 1 || month > 12 || day < 1) return { kind: "invalid", message: "Enter a full date." }
  const last = daysIn(year, month)
  if (day > last) {
    return {
      kind: "invalid",
      message: month === 2 ? `February ${year} has ${last} days.` : `${MONTH_NAMES[month - 1]} has ${last} days.`,
    }
  }
  if (year < 2000 || year > 2099) return { kind: "invalid", message: "Enter a date between 2000 and 2099." }
  return { kind: "ok", value: text }
}

/** 00:00 UTC of a valid YYYY-MM-DD date, in epoch seconds. */
export function dateEpoch(value: string): number {
  const [year, month, day] = value.split("-").map(Number)
  return Date.UTC(year!, month! - 1, day!) / 1000
}

/** A calendar day as UTC sees it, "Oct 29", for the editor's notes. Same output as time.ts formatUTCDate. */
export function utcDay(epoch: number): string {
  return new Date(epoch * 1000).toLocaleDateString([], { month: "short", day: "numeric", timeZone: "UTC" })
}

// ---------------------------------------------------------------------------
// The credit cycle

/** One credit cycle, [start, end), in epoch seconds: 00:00 UTC on two refill days. */
export interface Cycle {
  start: number
  end: number
}

/** 00:00 UTC on `day` of a month, clamped to its last day. `month` is 0-based and may run past either end. */
function refillIn(year: number, month: number, day: number): number {
  const first = new Date(Date.UTC(year, month, 1))
  const y = first.getUTCFullYear()
  const m = first.getUTCMonth()
  return Date.UTC(y, m, Math.min(day, daysIn(y, m + 1))) / 1000
}

/**
 * The cycle a refill date gives at an instant: client.CreditCycleAt, ported.
 * It starts at 00:00 UTC on the most recent occurrence of the date's day of
 * the month (clamped to a shorter month) and ends at the next. Only the day of
 * the month matters. Null for a date the plugin would not accept.
 */
export function cycleOn(renews: string, at: number): Cycle | null {
  const date = readDate(renews)
  if (date.kind !== "ok") return null
  const day = Number(date.value.slice(8, 10))
  const now = new Date(at * 1000)
  const year = now.getUTCFullYear()
  const month = now.getUTCMonth()
  const thisOne = refillIn(year, month, day)
  if (thisOne <= at) return { start: thisOne, end: refillIn(year, month + 1, day) }
  return { start: refillIn(year, month - 1, day), end: thisOne }
}

/** "this cycle Sep 22 – Oct 22", under a focused refill date. */
export function cycleText(cycle: Cycle): string {
  return `this cycle ${utcDay(cycle.start)} – ${utcDay(cycle.end)}`
}

/**
 * What a refill on the 29th to 31st does in a shorter month, said under the
 * field; null for any other day, which every month has.
 */
export function lateDayText(renews: string): string | null {
  const date = readDate(renews)
  if (date.kind !== "ok") return null
  const day = Number(date.value.slice(8, 10))
  if (day < 29) return null
  return `Refills on the ${day}${day === 31 ? "st" : "th"}, or on the last day of shorter months.`
}

// ---------------------------------------------------------------------------
// Reading times

/** How old a new reading may be, and how far ahead of the plugin's clock: internal/overrides. */
export const READING_MAX_AGE = 48 * 3600
export const READING_MAX_AHEAD = 5 * 60
/** The control's own lower bound, a few minutes inside the plugin's so a slow save still lands. */
export const READING_PICK_AGE = 47 * 3600 + 55 * 60

/** A `datetime-local` value, "2026-10-09T13:20", read in the reader's own zone; null when it is not one. */
export function localInstant(value: string): number | null {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(value)) return null
  if (readDate(value.slice(0, 10)).kind !== "ok") return null
  const at = new Date(value)
  const [hour, minute, second = 0] = value.slice(11).split(":").map(Number)
  // Date normalizes an impossible clock time or a skipped DST hour. Never
  // save a different local time from the one the reader picked.
  if (!Number.isFinite(at.getTime()) || at.getHours() !== hour || at.getMinutes() !== minute || at.getSeconds() !== second) return null
  return Math.floor(at.getTime() / 1000)
}

/** An instant as a `datetime-local` value in the reader's zone, to the minute. */
export function localInputValue(epoch: number): string {
  const at = new Date(epoch * 1000)
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}T${pad(at.getHours())}:${pad(at.getMinutes())}`
}

/** An instant as the plugin takes one: RFC 3339 in UTC with `Z`, whole seconds. */
export function rfc3339(epoch: number): string {
  return new Date(Math.floor(epoch) * 1000).toISOString().replace(/\.\d{3}Z$/, "Z")
}

/** "13:20 UTC", under a reading time picked in the reader's zone. */
export function utcClock(epoch: number): string {
  return `${new Date(epoch * 1000).toISOString().slice(11, 16)} UTC`
}

// ---------------------------------------------------------------------------
// Fields

/**
 * A money or date field's edit: a value typed, or the dashboard's value
 * dropped — Use config on a credit or refill date, Use estimate on a renewal.
 */
export type FieldEdit = { kind: "set"; text: string; incomplete?: boolean } | { kind: "drop" }

/** A Console reading's edit: cleared, or a new one typed. `at` is a `datetime-local` value. */
export type ReadingEdit = { kind: "clear" } | { kind: "set"; amount: string; at: string }

/** Where the value a field opens with comes from. */
export type FieldSource = "dashboard" | "config" | "none"

/** One field, as the editor draws it and the batch sends it. */
export interface FieldState {
  /** What the input shows. */
  text: string
  /** Where the untouched value comes from. */
  source: FieldSource
  /** The value set here, "" when none. */
  stored: string
  /** The config's value, "" when none or invalid. */
  config: string
  configInvalid: boolean
  edit: FieldEdit | undefined
  /** The edit asks for something other than what is stored. */
  changed: boolean
  /** Why the value cannot be sent; null when it can. */
  error: string | null
  /** For a comma the page will not guess at: both readings. */
  ambiguous: { decimal: string; thousands: string } | null
  /** What the batch sends for this field: the dashboard's value, or null for none. */
  desired: string | null
}

export const AMOUNT_MESSAGE = "Enter a dollar amount, like 200 or 260.50."
export const READING_AMOUNT_MESSAGE = "Enter the amount Console shows, like 143.20."
export const READING_TIME_MESSAGE = "Enter a time in the last 48 hours."

const ambiguousMessage = (choice: { decimal: string; thousands: string }) =>
  `A comma is ambiguous. Did you mean ${choice.decimal} or ${choice.thousands}?`

/**
 * A field from what is stored, what the config says, and the reader's edit.
 * `read` is readMoney or readDate; `same` decides that a typed value is the
 * one already there, which is then no change at all.
 */
function field(
  stored: string,
  config: string,
  configInvalid: boolean,
  edit: FieldEdit | undefined,
  read: (edit: Extract<FieldEdit, { kind: "set" }>) =>
    | { kind: "empty" }
    | { kind: "ok"; value: string }
    | { kind: "invalid"; message: string }
    | { kind: "ambiguous"; decimal: string; thousands: string },
  same: (a: string, b: string) => boolean,
  emptyMessage: string,
): FieldState {
  const base = stored || config
  const source: FieldSource = stored ? "dashboard" : config ? "config" : "none"
  const current = stored === "" ? null : stored
  const state: FieldState = {
    text: base,
    source,
    stored,
    config,
    configInvalid,
    edit,
    changed: false,
    error: null,
    ambiguous: null,
    desired: current,
  }
  if (edit === undefined) return state
  if (edit.kind === "drop") return { ...state, text: config, desired: null, changed: current !== null }
  const typed = { ...state, text: edit.text }
  const value = read(edit)
  switch (value.kind) {
    case "empty":
      return base === "" ? typed : { ...typed, error: emptyMessage }
    case "invalid":
      return { ...typed, error: value.message }
    case "ambiguous":
      return { ...typed, error: ambiguousMessage(value), ambiguous: { decimal: value.decimal, thousands: value.thousands } }
    case "ok": {
      // Typed back to what it opened with: no change, and a value that came
      // from the config keeps coming from it.
      if (base !== "" && same(value.value, base)) return typed
      return { ...typed, desired: value.value, changed: value.value !== current }
    }
  }
}

const moneyReader = (message: string) => (edit: { text: string }) => {
  const value = readMoney(edit.text)
  return value.kind === "invalid" ? { kind: "invalid" as const, message } : value
}

const dateReader = (edit: { text: string; incomplete?: boolean }) => readDate(edit.text, edit.incomplete === true)

/** The monthly credit field. */
export function creditField(settings: APICreditSettings, edit: FieldEdit | undefined): FieldState {
  return field(
    settings.monthlyUsd,
    settings.configMonthlyUsdInvalid ? "" : settings.configMonthlyUsd,
    settings.configMonthlyUsdInvalid,
    edit,
    moneyReader(AMOUNT_MESSAGE),
    sameAmount,
    "Enter a dollar amount.",
  )
}

/** The refill date field. */
export function refillField(settings: APICreditSettings, edit: FieldEdit | undefined): FieldState {
  return field(
    settings.renews,
    settings.configRenewsInvalid ? "" : settings.configRenews,
    settings.configRenewsInvalid,
    edit,
    dateReader,
    (a, b) => a === b,
    "Enter a full date.",
  )
}

/** A Claude subscription's renewal date field. Its fallback is the estimate, which has no value to show. */
export function renewalField(setting: RenewalSetting | null | undefined, edit: FieldEdit | undefined): FieldState {
  return field(setting?.date ?? "", "", false, edit, dateReader, (a, b) => a === b, "Enter a full date.")
}

/** A Console reading, as the editor draws it and the batch sends it. */
export interface ReadingState {
  stored: APICreditSettings["reading"]
  unusedReason: string
  edit: ReadingEdit | undefined
  changed: boolean
  /** The amount typed, read; null unless a reading is being entered. */
  amount: { text: string; error: string | null; ambiguous: { decimal: string; thousands: string } | null } | null
  /** The instant picked, null when none can be read. */
  at: number | null
  atError: string | null
  /** The picked time falls on the refill day, when Console may not show the new credit yet. */
  onRefillDay: boolean
  desired: { remainingUsd: string; at: string } | null
}

/**
 * A Console reading field. `renews` is the refill date the batch makes
 * effective for the row — its own date when it sends one, else the config's —
 * which is what the plugin checks a new reading against. `now` is the page's
 * clock; the plugin uses its own, and a few minutes either way are allowed
 * for.
 */
export function readingField(
  settings: APICreditSettings,
  edit: ReadingEdit | undefined,
  context: { now: number; renews: string | null },
): ReadingState {
  const stored = settings.reading
  const sent = stored ? { remainingUsd: stored.remainingUsd, at: rfc3339(stored.atEpoch) } : null
  const state: ReadingState = {
    stored,
    unusedReason: settings.readingUnusedReason,
    edit,
    changed: false,
    amount: null,
    at: null,
    atError: null,
    onRefillDay: false,
    desired: sent,
  }
  if (edit === undefined) return state
  if (edit.kind === "clear") return { ...state, desired: null, changed: stored !== null }

  const amount = readMoney(edit.amount)
  const amountState = {
    text: edit.amount,
    error:
      amount.kind === "ok"
        ? null
        : amount.kind === "ambiguous"
          ? ambiguousMessage(amount)
          : amount.kind === "empty"
            ? "Enter the amount Console shows."
            : READING_AMOUNT_MESSAGE,
    ambiguous: amount.kind === "ambiguous" ? { decimal: amount.decimal, thousands: amount.thousands } : null,
  }
  const at = localInstant(edit.at)
  const cycle = context.renews ? cycleOn(context.renews, context.now) : null
  const desired = amount.kind === "ok" && at !== null ? { remainingUsd: amount.value, at: rfc3339(at) } : null
  // The stored reading sent back as it was is not judged against the clock
  // again, by the plugin or here, so a week-old reading does not stop a
  // credit from being changed beside it.
  const same = desired !== null && sent !== null && desired.remainingUsd === sent.remainingUsd && desired.at === sent.at
  let atError: string | null = null
  if (at === null) atError = "Enter a full date and time."
  else if (same) atError = null
  else if (at < context.now - READING_MAX_AGE || at > context.now + READING_MAX_AHEAD) atError = READING_TIME_MESSAGE
  else if (cycle !== null && at < cycle.start) atError = beforeRefillMessage(cycle.start)
  const onRefillDay = at !== null && cycle !== null && at >= cycle.start && at < cycle.start + 86400
  return {
    ...state,
    amount: amountState,
    at,
    atError,
    onRefillDay,
    desired: desired ?? sent,
    changed: !same,
  }
}

const beforeRefillMessage = (start: number) =>
  `The credit refilled on ${utcDay(start)}, after this time. Enter a reading taken since.`

// ---------------------------------------------------------------------------
// The API credit draft

export type CreditField = "monthlyUsd" | "renews" | "reading"

/** What the reader changed on one organization, and the revision it was opened at. */
export interface CreditRowEdit {
  /** Sent back as `baseRevision`, so a save made elsewhere meanwhile is a conflict, never overwritten. */
  baseRevision: string
  /** The account's name when the edit began, for the notice if it disappears. */
  name: string
  monthlyUsd?: FieldEdit
  renews?: FieldEdit
  reading?: ReadingEdit
}

/** The Monthly API Credit card's unsaved changes. */
export interface CreditDraft {
  rows: Record<string, CreditRowEdit>
  /** Orphans marked for removal, by id, with the revision seen. */
  remove: Record<string, string>
}

export const EMPTY_CREDIT_DRAFT: CreditDraft = { rows: {}, remove: {} }

/** One organization in the editor: its three fields, and what they add up to. */
export interface CreditRowState {
  monthlyUsd: FieldState
  renews: FieldState
  reading: ReadingState
  changes: number
  errors: number
}

/** One organization's fields, from its settings and the reader's edit. */
export function creditRow(settings: APICreditSettings, edit: CreditRowEdit | undefined, now: number): CreditRowState {
  const monthlyUsd = creditField(settings, edit?.monthlyUsd)
  const renews = refillField(settings, edit?.renews)
  // The refill date the plugin will check a new reading against: this
  // batch's own, else the config's. Null asks it for the config's.
  const effective = renews.error === null ? (renews.desired ?? (settings.configRenewsInvalid ? null : settings.configRenews || null)) : null
  const reading = readingField(settings, edit?.reading, { now, renews: effective })
  const fields = [monthlyUsd, renews]
  const readingErrors = (reading.amount?.error ? 1 : 0) + (reading.atError ? 1 : 0)
  return {
    monthlyUsd,
    renews,
    reading,
    changes: fields.filter((one) => one.changed).length + (reading.changed ? 1 : 0),
    errors: fields.filter((one) => one.error !== null).length + readingErrors,
  }
}

/**
 * The draft with one field's edit replaced, or removed with `undefined`
 * (Undo). A row's revision is taken when its first edit is made and kept
 * until its last is undone.
 */
export function editCredit(
  draft: CreditDraft,
  account: Pick<APICreditAccount, "id"> & { settings: Pick<APICreditSettings, "revision"> },
  name: string,
  key: CreditField,
  edit: FieldEdit | ReadingEdit | undefined,
): CreditDraft {
  const row: CreditRowEdit = { ...(draft.rows[account.id] ?? { baseRevision: account.settings.revision, name }) }
  if (edit === undefined) delete row[key]
  else (row as Record<CreditField, FieldEdit | ReadingEdit>)[key] = edit
  const rows = { ...draft.rows }
  if (row.monthlyUsd === undefined && row.renews === undefined && row.reading === undefined) delete rows[account.id]
  else rows[account.id] = row
  return { ...draft, rows }
}

/** The draft with an orphan marked for removal, or unmarked. */
export function removeOrphan(draft: CreditDraft, orphan: Pick<APICreditOrphan, "id" | "revision">, remove: boolean): CreditDraft {
  const next = { ...draft.remove }
  if (remove) next[orphan.id] = orphan.revision
  else delete next[orphan.id]
  return { ...draft, remove: next }
}

/** What one Save would send, and whether it can. */
export interface Plan {
  /** Fields changed, plus orphans marked for removal. */
  changes: number
  /** Fields that cannot be sent as they are. */
  errors: number
  /** Null with nothing to send, or anything to fix. */
  batch: SettingsBatch | null
  /** The batch is past the 4096 bytes the plugin reads. */
  tooLarge: boolean
}

/** The most JSON one batch may be, on either door. */
export const MAX_BATCH_BYTES = 4096

function plan(items: object[], changes: number, errors: number, kind: string): Plan {
  if (errors > 0 || items.length === 0) return { changes, errors, batch: null, tooLarge: false }
  const batch: SettingsBatch = { kind, items }
  const tooLarge = items.length > (kind === "apiCredits" ? 16 : 32) ||
    new TextEncoder().encode(JSON.stringify(batch)).length > MAX_BATCH_BYTES
  return { changes, errors, batch: tooLarge ? null : batch, tooLarge }
}

/**
 * The Monthly API Credit card's Save: every changed row, in account order,
 * each with its full desired state, then each orphan marked for removal with
 * every value null. A row whose account is not editable is not sent; the
 * plugin would refuse the whole batch for it.
 */
export function creditPlan(
  accounts: APICreditAccount[],
  draft: CreditDraft,
  now: number,
  settingsOf: (account: APICreditAccount) => APICreditSettings = (account) => account.settings,
): Plan {
  const items: object[] = []
  let changes = 0
  let errors = 0
  for (const account of accounts) {
    const edit = draft.rows[account.id]
    const settings = settingsOf(account)
    if (edit === undefined || !settings.editable) continue
    const row = creditRow(settings, edit, now)
    changes += row.changes
    errors += row.errors
    if (row.changes === 0) continue
    items.push({
      id: account.id,
      baseRevision: edit.baseRevision,
      monthlyUsd: row.monthlyUsd.desired,
      renews: row.renews.desired,
      reading: row.reading.desired,
    })
  }
  for (const [id, revision] of Object.entries(draft.remove)) {
    changes++
    items.push({ id, baseRevision: revision, monthlyUsd: null, renews: null, reading: null })
  }
  return plan(items, changes, errors, "apiCredits")
}

/** Changes in a draft against the current document, for the fold line's "2 unsaved". */
export function creditChanges(accounts: APICreditAccount[], draft: CreditDraft, now: number): number {
  return creditPlan(accounts, draft, now).changes
}

/**
 * The draft after the document changed: rows of accounts no longer listed
 * are dropped, each with the sentence that says so, and removals of orphans
 * already gone are forgotten. Everything else is kept, typed values and the
 * revision they were opened at included.
 */
export function reconcileCredits(
  draft: CreditDraft,
  accounts: Pick<APICreditAccount, "id">[],
  orphans: Pick<APICreditOrphan, "id">[],
): { draft: CreditDraft; dropped: string[] } {
  const listed = new Set(accounts.map((account) => account.id))
  const dropped: string[] = []
  const rows: Record<string, CreditRowEdit> = {}
  for (const [id, row] of Object.entries(draft.rows)) {
    if (listed.has(id)) rows[id] = row
    else dropped.push(`${row.name} is no longer in Quota Cache's config; its unsaved change was dropped.`)
  }
  const orphaned = new Set(orphans.map((orphan) => orphan.id))
  const remove = Object.fromEntries(Object.entries(draft.remove).filter(([id]) => orphaned.has(id)))
  const unchanged =
    dropped.length === 0 && Object.keys(remove).length === Object.keys(draft.remove).length
  return { draft: unchanged ? draft : { rows, remove }, dropped }
}

// ---------------------------------------------------------------------------
// The renewal draft

/** What the reader changed on one Claude subscription's renewal date. */
export interface RenewalRowEdit {
  baseRevision: string
  name: string
  date: FieldEdit
}

/** The Accounts card's unsaved changes. */
export interface RenewalDraft {
  rows: Record<string, RenewalRowEdit>
  /** Renewal orphans marked for removal, by id, with the revision seen. */
  remove: Record<string, string>
}

export const EMPTY_RENEWAL_DRAFT: RenewalDraft = { rows: {}, remove: {} }

/**
 * A draft after a 409 conflict (must-fix 3): each conflicting row's edit,
 * and any removal it marked, is dropped, since its fields now show what the
 * other device saved; every other row's is kept. `retry` says whether
 * anything is left to send. With nothing, Save must not read "Try again"
 * beside "no changes".
 */
export function dropConflicts<D extends { rows: Record<string, unknown>; remove: Record<string, string> }>(
  draft: D,
  ids: readonly string[],
): { draft: D; retry: boolean } {
  const rows = { ...draft.rows }
  const remove = { ...draft.remove }
  for (const id of ids) {
    delete rows[id]
    delete remove[id]
  }
  return { draft: { ...draft, rows, remove }, retry: Object.keys(rows).length > 0 || Object.keys(remove).length > 0 }
}

export function editRenewal(
  draft: RenewalDraft,
  credential: Pick<Credential, "id" | "renewalSetting">,
  name: string,
  edit: FieldEdit | undefined,
): RenewalDraft {
  const rows = { ...draft.rows }
  if (edit === undefined) delete rows[credential.id]
  else {
    const held = rows[credential.id]
    rows[credential.id] = {
      baseRevision: held?.baseRevision ?? credential.renewalSetting?.revision ?? "",
      name: held?.name ?? name,
      date: edit,
    }
  }
  return { ...draft, rows }
}

export function removeRenewalOrphan(draft: RenewalDraft, orphan: Pick<RenewalOrphan, "id" | "revision">, remove: boolean): RenewalDraft {
  const next = { ...draft.remove }
  if (remove) next[orphan.id] = orphan.revision
  else delete next[orphan.id]
  return { ...draft, remove: next }
}

/**
 * The Accounts card's Save: every changed renewal date, in catalog order,
 * then each renewal orphan marked for removal with `date: null`.
 */
export function renewalPlan(
  credentials: Credential[],
  draft: RenewalDraft,
  settingOf: (credential: Credential) => RenewalSetting | null | undefined = (credential) => credential.renewalSetting,
): Plan {
  const items: object[] = []
  let changes = 0
  let errors = 0
  for (const credential of credentials) {
    const edit = draft.rows[credential.id]
    if (edit === undefined || credential.renewalEditable !== true) continue
    const state = renewalField(settingOf(credential), edit.date)
    if (state.error !== null) errors++
    if (!state.changed) continue
    changes++
    items.push({ id: credential.id, baseRevision: edit.baseRevision, date: state.desired })
  }
  for (const [id, revision] of Object.entries(draft.remove)) {
    changes++
    items.push({ id, baseRevision: revision, date: null })
  }
  return plan(items, changes, errors, "renewals")
}

/** As reconcileCredits, for the renewal draft: a credential CPA no longer lists takes its edit with it. */
export function reconcileRenewals(
  draft: RenewalDraft,
  credentials: Pick<Credential, "id">[],
  orphans: Pick<RenewalOrphan, "id">[],
): { draft: RenewalDraft; dropped: string[] } {
  const listed = new Set(credentials.map((credential) => credential.id))
  const dropped: string[] = []
  const rows: Record<string, RenewalRowEdit> = {}
  for (const [id, row] of Object.entries(draft.rows)) {
    if (listed.has(id)) rows[id] = row
    else dropped.push(`${row.name} is no longer in CPA; its unsaved change was dropped.`)
  }
  const orphaned = new Set(orphans.map((orphan) => orphan.id))
  const remove = Object.fromEntries(Object.entries(draft.remove).filter(([id]) => orphaned.has(id)))
  const unchanged = dropped.length === 0 && Object.keys(remove).length === Object.keys(draft.remove).length
  return { draft: unchanged ? draft : { rows, remove }, dropped }
}

/** Whether a revision the plugin sent is newer than another: both decimal, "" for nothing stored. */
export function newerRevision(a: string, b: string): boolean {
  return BigInt(a || "0") > BigInt(b || "0")
}

// ---------------------------------------------------------------------------
// What came of a Save

/** The value codes, each refused with the row and field it is about. */
export const FIELD_CODES: ReadonlySet<string> = new Set([
  "invalid_monthly_usd",
  "invalid_renews",
  "invalid_date",
  "invalid_reading_amount",
  "invalid_reading_time",
  "reading_before_refill",
])

/** What one Save came to, in the terms the editor acts on. */
export type SaveOutcome =
  | { kind: "saved"; unchanged: boolean; revision: string; settings: Record<string, unknown> }
  /** Another device saved these rows first; `current` is what each holds now. */
  | { kind: "conflict"; ids: string[]; current: Record<string, unknown>; text: string }
  /** The plugin refused one value; `text` goes under that field. */
  | { kind: "field"; code: string; id: string; field: string; text: string }
  /** Nothing was saved, and the foot says why. `signIn`: access was lost, and the draft waits for it. */
  | { kind: "failed"; code: string; text: string; signIn: boolean }
  /** No answer the page can read: it may or may not have saved. */
  | { kind: "lost"; text: string }

/** The foot's sentence for a value the plugin refused. */
export const FIELD_FOOT = "Not saved. Fix the marked field."
/** The foot's sentence for a conflict. */
export const CONFLICT_FOOT = "Not saved. Another device saved first."
/** Under each field a conflict replaced. */
export const CONFLICT_FIELD = "Changed from another device. Showing the latest; check and save again."
/** The foot while no door is open: E.6. */
export const SIGN_IN_FOOT = "Sign in again to save. Your changes are kept until you reload."
const LOST =
  "No answer came back, so this may or may not have saved. Saving again is safe: a save that already landed is not applied twice."

/** Under a field, for each value code the plugin can answer with. */
export function fieldMessage(code: string): string {
  switch (code) {
    case "invalid_monthly_usd":
      return AMOUNT_MESSAGE
    case "invalid_renews":
    case "invalid_date":
      return "Enter a real date between 2000 and 2099."
    case "invalid_reading_amount":
      return READING_AMOUNT_MESSAGE
    case "invalid_reading_time":
      return READING_TIME_MESSAGE
    case "reading_before_refill":
      return "This is before the credit last refilled. Enter a reading taken since."
    default:
      return "Check this value."
  }
}

/**
 * The foot's sentence for a Save that did not save, per the plugin's own codes
 * and the refusals access.ts names. Every one of them is proof nothing was
 * written: the plugin answers a code only before it commits, or after a
 * commit that failed and changed nothing.
 */
export function saveErrorText(code: string, status: number): string {
  switch (code) {
    case "settings_unavailable":
      return "Quota Glance could not read settings.json, so nothing was saved."
    case "settings_unwritable":
      return "Quota Glance could not write settings.json. Nothing was saved; try again."
    case "settings_full":
      return "Quota Glance's saved settings are full. Remove saved values for accounts no longer in use, then save again."
    case "too_many_writes":
      return "Too many saves in the last minute. Wait a minute and save again."
    case "not_editable":
      return "Some of these can no longer be edited here. Nothing was saved."
    case "conflict":
      return CONFLICT_FOOT
    case "no_session":
    case "token_refused":
    case "console_refused":
      return SIGN_IN_FOOT
    case "ip_banned":
      return "CPA is refusing management requests from this address for up to 30 minutes after repeated failed sign-ins. Nothing was saved; the dashboard password still works."
    case "remote_disabled":
      return "CPA accepts console sessions only on the machine it runs on. Nothing was saved; use the dashboard password here."
    case "management_off":
      return "CPA's management API is switched off on this server. Nothing was saved; use the dashboard password here."
    case "refused_other":
      return `Something between this page and CPA refused the request (HTTP ${status}). Nothing was saved.`
    case "cross_site":
      return "The plugin refused this request because it did not come from this page. Nothing was saved."
    case "too_early":
      return "The request arrived before the secure connection was confirmed. Nothing was saved; save again."
    case "not_found":
      return "Editing is turned off in Quota Glance's settings (allow-edit). Nothing was saved."
    case "route_missing":
      return "This server did not offer the save route, so nothing was saved. Reload the page; if this persists, check that Quota Glance 0.7.0 or newer is loaded."
    case "disabled":
      return "Quota Glance is switched off, so nothing was saved."
    case "invalid_request":
    case "unsupported_media_type":
      return "The plugin could not read this save, so nothing was saved. Reload the page and try again."
    default:
      if (FIELD_CODES.has(code)) return FIELD_FOOT
      return status >= 400 && status < 500 ? "The save was refused. Nothing was saved." : LOST
  }
}

/** Refusals that close a door: the draft waits for the reader to sign in again. */
const SIGN_IN = new Set(["no_session", "token_refused", "console_refused", "ip_banned", "remote_disabled", "management_off"])

const isObject = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === "object" && !Array.isArray(value)

/** What a Save came to, from saveThrough's answer. */
export function saveOutcome(answer: SaveAnswer): SaveOutcome {
  const failed = (code: string, status: number): SaveOutcome => ({
    kind: "failed",
    code,
    text: saveErrorText(code, status),
    signIn: SIGN_IN.has(code),
  })
  if (!answer.sent) return failed(answer.code, answer.status)
  if (!answer.answered) return { kind: "lost", text: LOST }
  if (answer.refusal) return failed(answer.refusal.code, answer.status)
  const body = answer.json && isObject(answer.body) ? answer.body : null
  if (answer.ok) {
    // The plugin answers 200 only with a body; an unreadable one is that
    // answer lost on the way back.
    if (body === null || body.ok !== true) return { kind: "lost", text: LOST }
    return {
      kind: "saved",
      unchanged: body.unchanged === true,
      revision: typeof body.revision === "string" ? body.revision : "",
      settings: isObject(body.settings) ? body.settings : {},
    }
  }
  const code = body !== null && typeof body.error === "string" ? body.error : ""
  if (!answer.plugin) {
    // Not the plugin's answer: a 4xx is something in front of it turning
    // the request away; anything else is an answer lost.
    return answer.status >= 400 && answer.status < 500 ? failed("", answer.status) : { kind: "lost", text: LOST }
  }
  if (code === "conflict") {
    const ids = Array.isArray(body!.conflicts) ? body!.conflicts.filter((id): id is string => typeof id === "string") : []
    return { kind: "conflict", ids, current: isObject(body!.current) ? body!.current : {}, text: CONFLICT_FOOT }
  }
  if (FIELD_CODES.has(code) && typeof body!.id === "string" && typeof body!.field === "string") {
    return { kind: "field", code, id: body!.id, field: body!.field, text: fieldMessage(code) }
  }
  return failed(code, answer.status)
}

/**
 * The fields a conflict replaced on one row: those the reader had changed,
 * and those whose stored value moved under them. Each is drawn with
 * CONFLICT_FIELD.
 */
export function conflictFields(
  before: APICreditSettings,
  current: APICreditSettings,
  edit: CreditRowEdit | undefined,
): CreditField[] {
  const moved: Record<CreditField, boolean> = {
    monthlyUsd: before.monthlyUsd !== current.monthlyUsd,
    renews: before.renews !== current.renews,
    reading:
      before.reading?.remainingUsd !== current.reading?.remainingUsd || before.reading?.atEpoch !== current.reading?.atEpoch,
  }
  return (["monthlyUsd", "renews", "reading"] as const).filter((key) => moved[key] || edit?.[key] !== undefined)
}
