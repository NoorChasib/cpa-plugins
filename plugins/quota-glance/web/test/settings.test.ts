// The editor's model: what a reader types, checked as the plugin will check
// it (E.3), the cycle a refill date gives (E.10), the draft each card keeps,
// the one batch a Save sends, and what the page says of every answer (E.8).
//
// The shared vectors are the spec's: internal/overrides tests the same ones
// on the plugin's side.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"

import type { SaveAnswer } from "../src/lib/access.ts"
import {
  AMOUNT_MESSAGE,
  CONFLICT_FIELD,
  CONFLICT_FOOT,
  conflictFields,
  creditChanges,
  creditField,
  creditPlan,
  creditRow,
  cycleOn,
  cycleText,
  dropConflicts,
  EMPTY_CREDIT_DRAFT,
  EMPTY_RENEWAL_DRAFT,
  editCredit,
  editRenewal,
  FIELD_CODES,
  FIELD_FOOT,
  fieldMessage,
  lateDayText,
  localInputValue,
  localInstant,
  MAX_BATCH_BYTES,
  moneyText,
  newerRevision,
  readDate,
  readingField,
  readMoney,
  reconcileCredits,
  reconcileRenewals,
  refillField,
  removeOrphan,
  removeRenewalOrphan,
  renewalField,
  renewalPlan,
  rfc3339,
  sameAmount,
  saveErrorText,
  saveOutcome,
  SIGN_IN_FOOT,
  utcClock,
} from "../src/lib/settings.ts"
import type { APICreditAccount, APICreditSettings, Summary } from "../src/lib/types.ts"

function document(name: string): Summary {
  return JSON.parse(readFileSync(new URL(`../../testdata/golden/${name}.json`, import.meta.url), "utf8")) as Summary
}

const golden = document("summary")
const degraded = document("summary-degraded")
const NOW = golden.generatedAtEpoch
const accounts = golden.apiCredits!.accounts

function account(set: APICreditAccount[], label: string): APICreditAccount {
  const found = set.find((item) => item.label === label)
  assert.ok(found, label)
  return found
}

const utc = (text: string) => Date.parse(text) / 1000

describe("amounts (E.3)", () => {
  test("the shared vectors: what the page sends, and what it asks about", () => {
    const sends: [string, string][] = [
      ["200", "200"],
      ["$200", "200"],
      [" 200 ", "200"],
      ["260.5", "260.5"],
      ["260.50", "260.50"],
      ["$1,200.50", "1200.50"],
      ["1,200", "1200"],
      ["0", "0"],
      ["$ 1,234,567.89", "1234567.89"],
    ]
    for (const [input, value] of sends) assert.deepEqual(readMoney(input), { kind: "ok", value }, input)
    assert.deepEqual(readMoney("12,50"), { kind: "ambiguous", decimal: "12.50", thousands: "1,250.00" })
    assert.deepEqual(readMoney("1,2"), { kind: "ambiguous", decimal: "1.20", thousands: "12.00" })
    for (const input of ["200.123", "-1", "1e3", "12345678", ".5", "5.", "1,2345", "abc", "$"]) {
      assert.notEqual(readMoney(input).kind, "ok", input)
    }
    assert.deepEqual(readMoney(""), { kind: "empty" })
  })

  test("printed as money, from the string alone", () => {
    assert.equal(moneyText("1200.5"), "$1,200.50")
    assert.equal(moneyText("200"), "$200.00")
    assert.equal(moneyText("0"), "$0.00")
    assert.equal(moneyText("007"), "$7.00")
  })

  test("the same number of cents", () => {
    assert.equal(sameAmount("200", "200.00"), true)
    assert.equal(sameAmount("260.5", "260.50"), true)
    assert.equal(sameAmount("260.5", "260.05"), false)
  })
})

describe("dates (E.3, E.7)", () => {
  test("a day that exists, in 2000 to 2099", () => {
    assert.deepEqual(readDate("2026-10-29"), { kind: "ok", value: "2026-10-29" })
    assert.deepEqual(readDate("2028-02-29"), { kind: "ok", value: "2028-02-29" })
    assert.deepEqual(readDate("2026-11-31"), { kind: "invalid", message: "November has 30 days." })
    assert.deepEqual(readDate("2026-02-30"), { kind: "invalid", message: "February 2026 has 28 days." })
    assert.deepEqual(readDate("1999-12-31"), { kind: "invalid", message: "Enter a date between 2000 and 2099." })
    assert.deepEqual(readDate("2026-1-5"), { kind: "invalid", message: "Enter a full date." })
    assert.deepEqual(readDate(""), { kind: "empty" })
  })

  test("a date half typed into the control is not a date", () => {
    assert.deepEqual(readDate("", true), { kind: "invalid", message: "Enter a full date." })
  })
})

describe("cycleOn, the page's port of client.CreditCycleAt (E.10)", () => {
  const vectors: [string, string, string, string][] = [
    ["2026-10-29", "2026-10-09T12:00:00Z", "2026-09-29", "2026-10-29"],
    ["2026-10-29", "2026-10-29T00:00:00Z", "2026-10-29", "2026-11-29"],
    ["2026-10-31", "2026-11-15T00:00:00Z", "2026-10-31", "2026-11-30"],
    ["2026-10-31", "2027-03-01T00:00:00Z", "2027-02-28", "2027-03-31"],
    ["2026-10-31", "2028-03-01T00:00:00Z", "2028-02-29", "2028-03-31"],
    ["2026-01-30", "2028-02-29T12:00:00Z", "2028-02-29", "2028-03-30"],
    ["2026-01-01", "2026-12-31T23:59:00Z", "2026-12-01", "2027-01-01"],
    // The rest of client/apicredit_test.go's table.
    ["2026-10-29", "2026-10-28T23:59:59Z", "2026-09-29", "2026-10-29"],
    ["2026-09-10", "2026-09-10T04:00:00Z", "2026-09-10", "2026-10-10"],
    ["2031-03-29", "2026-10-09T12:00:00Z", "2026-09-29", "2026-10-29"],
    ["2026-10-31", "2027-02-27T23:00:00Z", "2027-01-31", "2027-02-28"],
    ["2026-01-29", "2027-03-01T00:00:00Z", "2027-02-28", "2027-03-29"],
    ["2026-12-31", "2027-01-05T00:00:00Z", "2026-12-31", "2027-01-31"],
  ]
  for (const [renews, at, start, end] of vectors) {
    test(`${renews} at ${at}`, () => {
      assert.deepEqual(cycleOn(renews, utc(at)), { start: utc(`${start}T00:00:00Z`), end: utc(`${end}T00:00:00Z`) })
    })
  }

  test("a date the plugin would not accept has no cycle", () => {
    assert.equal(cycleOn("2026-02-30", NOW), null)
    assert.equal(cycleOn("", NOW), null)
  })

  test("the notes under a focused refill date (must-fix 9)", () => {
    assert.equal(cycleText(cycleOn("2026-10-22", utc("2026-10-09T12:00:00Z"))!), "this cycle Sep 22 – Oct 22")
    assert.equal(lateDayText("2026-10-31"), "Refills on the 31st, or on the last day of shorter months.")
    assert.equal(lateDayText("2026-10-29"), "Refills on the 29th, or on the last day of shorter months.")
    assert.equal(lateDayText("2026-10-28"), null)
  })
})

describe("reading times (E.7)", () => {
  test("sent as RFC 3339 in UTC, whole seconds", () => {
    assert.equal(rfc3339(utc("2026-10-09T13:20:00Z")), "2026-10-09T13:20:00Z")
    assert.equal(rfc3339(utc("2026-10-09T13:20:00Z") + 0.7), "2026-10-09T13:20:00Z")
    assert.equal(utcClock(utc("2026-10-09T13:20:00Z")), "13:20 UTC")
  })

  test("the control's value, in the reader's zone, round-trips to the minute", () => {
    const at = utc("2026-10-09T13:20:00Z")
    assert.match(localInputValue(at), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/)
    assert.equal(localInstant(localInputValue(at)), at)
    assert.equal(localInstant(""), null)
    assert.equal(localInstant("2026-10-09"), null)
    assert.equal(localInstant("2026-02-30T13:20"), null)
    assert.equal(localInstant("2026-10-09T24:00"), null)
  })
})

describe("fields", () => {
  const alpha = account(accounts, "alpha").settings
  const bravo = account(accounts, "bravo").settings

  test("untouched, a field shows the config's value and sends nothing set here", () => {
    const field = creditField(alpha, undefined)
    assert.deepEqual([field.text, field.source, field.desired, field.changed], ["200", "config", null, false])
  })

  test("typing the value it opened with is no change, and keeps following the config", () => {
    const field = creditField(alpha, { kind: "set", text: "$200.00" })
    assert.deepEqual([field.desired, field.changed, field.error], [null, false, null])
  })

  test("a new amount is sent as typed, normalized", () => {
    const field = creditField(alpha, { kind: "set", text: "$1,260.50" })
    assert.deepEqual([field.desired, field.changed], ["1260.50", true])
  })

  test("a value set here: Use config sends null, and shows the config's value", () => {
    const untouched = creditField(bravo, undefined)
    assert.deepEqual([untouched.text, untouched.source, untouched.desired], ["250", "dashboard", "250"])
    const dropped = creditField(bravo, { kind: "drop" })
    assert.deepEqual([dropped.text, dropped.desired, dropped.changed], ["100", null, true])
  })

  test("a comma is asked about, a bad amount explained, and a cleared one refused", () => {
    const ambiguous = creditField(alpha, { kind: "set", text: "12,50" })
    assert.equal(ambiguous.error, "A comma is ambiguous. Did you mean 12.50 or 1,250.00?")
    assert.deepEqual(ambiguous.ambiguous, { decimal: "12.50", thousands: "1,250.00" })
    assert.equal(creditField(alpha, { kind: "set", text: "-1" }).error, AMOUNT_MESSAGE)
    assert.equal(creditField(alpha, { kind: "set", text: "" }).error, "Enter a dollar amount.")
  })

  test("a row with nothing set and nothing typed sends nothing (needsSettings)", () => {
    const charlie = account(degraded.apiCredits!.accounts, "charlie").settings
    const field = refillField(charlie, { kind: "set", text: "" })
    assert.deepEqual([field.source, field.error, field.changed], ["none", null, false])
  })

  test("a refill date: the same rules, as dates", () => {
    assert.deepEqual(refillField(alpha, { kind: "set", text: "2026-11-31" }).error, "November has 30 days.")
    assert.deepEqual(refillField(alpha, { kind: "set", text: "" }).error, "Enter a full date.")
    const moved = refillField(alpha, { kind: "set", text: "2026-10-02" })
    assert.deepEqual([moved.desired, moved.changed], ["2026-10-02", true])
  })
})

describe("Console readings (E.3, E.7)", () => {
  const bravo = account(accounts, "bravo").settings
  const alpha = account(accounts, "alpha").settings
  const context = { now: NOW, renews: "2026-09-29" }

  test("untouched, a stored reading is sent back as stored", () => {
    const reading = readingField(bravo, undefined, context)
    assert.deepEqual(reading.desired, { remainingUsd: "180.00", at: rfc3339(1788968400) })
    assert.equal(reading.changed, false)
  })

  test("Clear sends null", () => {
    const reading = readingField(bravo, { kind: "clear" }, context)
    assert.deepEqual([reading.desired, reading.changed], [null, true])
  })

  test("a new reading in the last 48 hours, since the refill", () => {
    const at = NOW - 3600
    const reading = readingField(alpha, { kind: "set", amount: "$143.20", at: localInputValue(at) }, context)
    assert.deepEqual([reading.atError, reading.amount?.error], [null, null])
    assert.deepEqual(reading.desired, { remainingUsd: "143.20", at: rfc3339(Math.floor(at / 60) * 60) })
    assert.equal(reading.changed, true)
  })

  test("too old, in the future, or before the refill is refused under its field", () => {
    const set = (at: number, renews = "2026-09-29") =>
      readingField(alpha, { kind: "set", amount: "100", at: localInputValue(at) }, { now: NOW, renews })
    assert.equal(set(NOW - 49 * 3600).atError, "Enter a time in the last 48 hours.")
    assert.equal(set(NOW + 3600).atError, "Enter a time in the last 48 hours.")
    // Refilled at 00:00 UTC on Sep 10, four hours before NOW.
    assert.equal(set(NOW - 6 * 3600, "2026-09-10").atError, "The credit refilled on Sep 10, after this time. Enter a reading taken since.")
  })

  test("a reading taken on the refill day says so", () => {
    const reading = readingField(alpha, { kind: "set", amount: "100", at: localInputValue(NOW - 3600) }, { now: NOW, renews: "2026-09-10" })
    assert.equal(reading.onRefillDay, true)
  })

  test("the stored reading resent unchanged is not judged against the clock again", () => {
    const old = { ...bravo, reading: { remainingUsd: "180.00", atEpoch: NOW - 7 * 86400, enteredAtEpoch: NOW - 7 * 86400 } }
    const reading = readingField(old, { kind: "set", amount: "180.00", at: localInputValue(NOW - 7 * 86400) }, context)
    assert.deepEqual([reading.atError, reading.changed], [null, false])
  })
})

describe("the draft and the batch (E.5)", () => {
  const alpha = account(accounts, "alpha")
  const bravo = account(accounts, "bravo")

  test("one Save sends every changed row, each with its full desired state", () => {
    let draft = editCredit(EMPTY_CREDIT_DRAFT, alpha, "alpha", "monthlyUsd", { kind: "set", text: "260.50" })
    draft = editCredit(draft, bravo, "bravo", "renews", { kind: "set", text: "2026-10-02" })
    const plan = creditPlan(accounts, draft, NOW)
    assert.deepEqual(plan.batch, {
      kind: "apiCredits",
      items: [
        { id: alpha.id, baseRevision: "", monthlyUsd: "260.50", renews: null, reading: null },
        {
          id: bravo.id,
          baseRevision: "2",
          monthlyUsd: "250",
          renews: "2026-10-02",
          reading: { remainingUsd: "180.00", at: rfc3339(1788968400) },
        },
      ],
    })
    assert.equal(plan.changes, 2)
    assert.equal(creditChanges(accounts, draft, NOW), 2)
  })

  test("a row typed back to what it was is not sent, and Undo forgets it", () => {
    let draft = editCredit(EMPTY_CREDIT_DRAFT, alpha, "alpha", "monthlyUsd", { kind: "set", text: "200.00" })
    assert.equal(creditPlan(accounts, draft, NOW).batch, null)
    draft = editCredit(draft, alpha, "alpha", "monthlyUsd", undefined)
    assert.deepEqual(draft, EMPTY_CREDIT_DRAFT)
  })

  test("anything to fix sends nothing", () => {
    const draft = editCredit(EMPTY_CREDIT_DRAFT, alpha, "alpha", "monthlyUsd", { kind: "set", text: "12,50" })
    const plan = creditPlan(accounts, draft, NOW)
    assert.deepEqual([plan.batch, plan.errors, plan.changes], [null, 1, 0])
  })

  test("a row keeps the revision it was opened at, whatever a refresh says (E.6)", () => {
    const draft = editCredit(EMPTY_CREDIT_DRAFT, bravo, "bravo", "monthlyUsd", { kind: "set", text: "300" })
    const refreshed = accounts.map((item) =>
      item.id === bravo.id ? { ...item, settings: { ...item.settings, revision: "9", renews: "2026-10-05" } } : item,
    )
    const item = creditPlan(refreshed, draft, NOW).batch!.items[0] as Record<string, unknown>
    assert.equal(item.baseRevision, "2")
    // An untouched field takes the refreshed value.
    assert.equal(item.renews, "2026-10-05")
  })

  test("an orphan is cleared with every value null, and a row that cannot be edited is never sent", () => {
    const orphan = degraded.apiCredits!.orphans[0]!
    const draft = removeOrphan(EMPTY_CREDIT_DRAFT, orphan, true)
    assert.deepEqual(creditPlan(degraded.apiCredits!.accounts, draft, NOW).batch, {
      kind: "apiCredits",
      items: [{ id: "org-3f2a9c1d0b7e", baseRevision: "4", monthlyUsd: null, renews: null, reading: null }],
    })
    const locked = account(degraded.apiCredits!.accounts, "alpha")
    const blocked = editCredit(EMPTY_CREDIT_DRAFT, locked, "alpha", "monthlyUsd", { kind: "set", text: "300" })
    assert.equal(creditPlan(degraded.apiCredits!.accounts, blocked, NOW).batch, null)
  })

  test("sixteen rows fit the plugin's 4096 bytes", () => {
    const many: APICreditAccount[] = Array.from({ length: 16 }, (_, n) => ({
      ...alpha,
      id: `org-${n.toString(16).padStart(12, "0")}`,
    }))
    let draft = EMPTY_CREDIT_DRAFT
    for (const item of many) {
      draft = editCredit(draft, item, item.id, "monthlyUsd", { kind: "set", text: "1234567.89" })
      draft = editCredit(draft, item, item.id, "renews", { kind: "set", text: "2026-10-31" })
      draft = editCredit(draft, item, item.id, "reading", { kind: "set", amount: "1234567.89", at: localInputValue(NOW - 60) })
    }
    const plan = creditPlan(many, draft, NOW)
    assert.equal(plan.tooLarge, false)
    assert.ok(JSON.stringify(plan.batch).length <= MAX_BATCH_BYTES)
  })

  test("the row caps include orphan removals, not just edits", () => {
    const removals = Object.fromEntries(Array.from({ length: 17 }, (_, i) => [`org-${i.toString(16).padStart(12, "0")}`, "1"]))
    const credit = creditPlan([], { rows: {}, remove: removals }, NOW)
    assert.equal(credit.tooLarge, true)
    assert.equal(credit.batch, null)
    const renewals = Object.fromEntries(Array.from({ length: 33 }, (_, i) => [i.toString(16).padStart(16, "0"), "1"]))
    const renewal = renewalPlan([], { rows: {}, remove: renewals })
    assert.equal(renewal.tooLarge, true)
    assert.equal(renewal.batch, null)
  })

  test("a refresh without an account drops its draft row, and says so (E.6)", () => {
    const draft = editCredit(EMPTY_CREDIT_DRAFT, alpha, "alpha", "monthlyUsd", { kind: "set", text: "300" })
    const { draft: next, dropped } = reconcileCredits(draft, accounts.slice(1), [])
    assert.deepEqual(next.rows, {})
    assert.deepEqual(dropped, ["alpha is no longer in Quota Cache's config; its unsaved change was dropped."])
    const kept = reconcileCredits(draft, accounts, [])
    assert.equal(kept.draft, draft)
  })

  test("revisions compare as numbers", () => {
    assert.equal(newerRevision("10", "9"), true)
    assert.equal(newerRevision("2", ""), true)
    assert.equal(newerRevision("", "1"), false)
    assert.equal(newerRevision("9007199254740993", "9007199254740992"), true)
  })
})

describe("renewal dates (F.3)", () => {
  const set = golden.credentials.find((credential) => credential.id === "5f2b8c41d09e7a36")!

  test("a date set here: shown, kept, and dropped with Use estimate", () => {
    assert.deepEqual(
      [renewalField(set.renewalSetting, undefined).text, renewalField(set.renewalSetting, undefined).source],
      ["2026-09-10", "dashboard"],
    )
    const draft = editRenewal(EMPTY_RENEWAL_DRAFT, set, "noorchasib", { kind: "drop" })
    assert.deepEqual(renewalPlan(golden.credentials, draft).batch, {
      kind: "renewals",
      items: [{ id: "5f2b8c41d09e7a36", baseRevision: "3", date: null }],
    })
  })

  test("an empty field stores nothing; a credential the page may not key is never sent", () => {
    const estimated = golden.credentials.find((credential) => credential.id === "claude-agency@example.com.json")!
    assert.equal(renewalField(estimated.renewalSetting, { kind: "set", text: "" }).changed, false)
    const draft = editRenewal(EMPTY_RENEWAL_DRAFT, estimated, "agency", { kind: "set", text: "2026-10-27" })
    assert.equal(renewalPlan(golden.credentials, draft).batch, null)
  })

  test("a renewal orphan is cleared with date null", () => {
    const orphan = degraded.renewalOrphans![0]!
    const draft = removeRenewalOrphan(EMPTY_RENEWAL_DRAFT, orphan, true)
    assert.deepEqual(renewalPlan(degraded.credentials, draft).batch, {
      kind: "renewals",
      items: [{ id: "0123456789abcdef", baseRevision: "9", date: null }],
    })
  })

  test("a credential CPA no longer lists takes its edit with it", () => {
    const draft = editRenewal(EMPTY_RENEWAL_DRAFT, set, "noorchasib", { kind: "set", text: "2026-10-27" })
    const { dropped } = reconcileRenewals(draft, [], [])
    assert.deepEqual(dropped, ["noorchasib is no longer in CPA; its unsaved change was dropped."])
  })
})

describe("what a Save came to (E.8)", () => {
  const answered = (status: number, body: unknown, plugin = true): SaveAnswer => ({
    sent: true,
    door: "token",
    answered: true,
    status,
    ok: status >= 200 && status < 300,
    json: body !== undefined,
    body,
    refusal: null,
    plugin,
  })

  test("the foot's copy for each of the plugin's codes", () => {
    const copy: [string, string][] = [
      ["settings_unavailable", "Quota Glance could not read settings.json, so nothing was saved."],
      ["settings_unwritable", "Quota Glance could not write settings.json. Nothing was saved; try again."],
      [
        "settings_full",
        "Quota Glance's saved settings are full. Remove saved values for accounts no longer in use, then save again.",
      ],
      ["too_many_writes", "Too many saves in the last minute. Wait a minute and save again."],
      ["not_editable", "Some of these can no longer be edited here. Nothing was saved."],
      ["conflict", "Not saved. Another device saved first."],
    ]
    for (const [code, text] of copy) {
      assert.equal(saveErrorText(code, 400), text, code)
      const outcome = saveOutcome(answered(code === "conflict" ? 409 : 503, { error: code }))
      assert.equal("text" in outcome ? outcome.text : "", text, code)
    }
    for (const code of FIELD_CODES) assert.equal(saveErrorText(code, 400), FIELD_FOOT, code)
  })

  test("a field code goes under its field, with the field's own message", () => {
    assert.deepEqual(saveOutcome(answered(400, { error: "invalid_reading_time", id: "org-1", field: "reading.at" })), {
      kind: "field",
      code: "invalid_reading_time",
      id: "org-1",
      field: "reading.at",
      text: "Enter a time in the last 48 hours.",
    })
    for (const code of FIELD_CODES) assert.notEqual(fieldMessage(code), "Check this value.", code)
  })

  test("saved, and saved already", () => {
    assert.deepEqual(saveOutcome(answered(200, { ok: true, unchanged: false, revision: "8", settings: { a: null } })), {
      kind: "saved",
      unchanged: false,
      revision: "8",
      settings: { a: null },
    })
    assert.equal((saveOutcome(answered(200, { ok: true, unchanged: true, revision: "8" })) as { unchanged: boolean }).unchanged, true)
  })

  test("a conflict carries the rows and what each holds now", () => {
    const outcome = saveOutcome(
      answered(409, { error: "conflict", revision: "9", conflicts: ["org-1"], current: { "org-1": { revision: "9" } } }),
    )
    assert.deepEqual(outcome, { kind: "conflict", ids: ["org-1"], current: { "org-1": { revision: "9" } }, text: CONFLICT_FOOT })
  })

  test("access lost keeps the draft and asks for a sign-in (E.6)", () => {
    for (const code of ["no_session", "token_refused", "console_refused"] as const) {
      const outcome = saveOutcome({ sent: false, code, status: 401 })
      assert.deepEqual(outcome, { kind: "failed", code, text: SIGN_IN_FOOT, signIn: true })
    }
  })

  test("an answer that is not the plugin's: a 4xx refused it, anything else was lost", () => {
    assert.equal(saveOutcome(answered(400, { error: "nope" }, false)).kind, "failed")
    assert.equal(saveOutcome(answered(502, undefined, false)).kind, "lost")
    assert.equal(saveOutcome({ sent: true, door: "console", answered: false }).kind, "lost")
    // A 200 whose body cannot be read is the answer lost on the way back.
    assert.equal(saveOutcome(answered(200, undefined, false)).kind, "lost")
  })

  test("a refusal before the plugin ran is named", () => {
    const outcome = saveOutcome({
      sent: true,
      door: "token",
      answered: true,
      status: 403,
      ok: false,
      json: true,
      body: { error: "cross_site" },
      refusal: { code: "cross_site", latch: null, banSeconds: null },
      plugin: false,
    })
    assert.deepEqual(outcome, {
      kind: "failed",
      code: "cross_site",
      text: "The plugin refused this request because it did not come from this page. Nothing was saved.",
      signIn: false,
    })
  })

  test("a conflict replaces the fields the reader changed and those that moved (must-fix 3)", () => {
    const before = account(accounts, "bravo").settings
    const current: APICreditSettings = { ...before, revision: "5", renews: "2026-10-01" }
    assert.deepEqual(conflictFields(before, current, { baseRevision: "2", name: "bravo", monthlyUsd: { kind: "set", text: "1" } }), [
      "monthlyUsd",
      "renews",
    ])
    assert.equal(CONFLICT_FIELD, "Changed from another device. Showing the latest; check and save again.")
  })
})

describe("the draft after a conflict (must-fix 3)", () => {
  const edit = (text: string) => ({ baseRevision: "2", name: "x", monthlyUsd: { kind: "set" as const, text } })

  test("drops each conflicting row and keeps the rest, so Try again resends them", () => {
    const draft = { rows: { alpha: edit("250"), bravo: edit("310") }, remove: { "org-3f2a9c1d0b7e": "1" } }
    const next = dropConflicts(draft, ["alpha"])
    assert.deepEqual(Object.keys(next.draft.rows), ["bravo"])
    assert.deepEqual(next.draft.remove, { "org-3f2a9c1d0b7e": "1" })
    assert.equal(next.retry, true)
    assert.deepEqual(Object.keys(draft.rows), ["alpha", "bravo"], "the draft it was given is not changed")
  })

  test("with every edited row in conflict nothing is left, so there is nothing to try again", () => {
    const next = dropConflicts({ rows: { alpha: edit("250"), bravo: edit("310") }, remove: {} }, ["alpha", "bravo"])
    assert.deepEqual(next.draft, { rows: {}, remove: {} })
    assert.equal(next.retry, false)
    const orphan = dropConflicts({ rows: {}, remove: { "0123456789abcdef": "2" } }, ["0123456789abcdef"])
    assert.equal(orphan.retry, false)
  })
})

describe("one organization's editor row", () => {
  test("counts its changes and its errors", () => {
    const alpha = account(accounts, "alpha").settings
    const row = creditRow(
      alpha,
      { baseRevision: "", name: "alpha", monthlyUsd: { kind: "set", text: "300" }, renews: { kind: "set", text: "2026-02-30" } },
      NOW,
    )
    assert.deepEqual([row.changes, row.errors], [1, 1])
  })
})
