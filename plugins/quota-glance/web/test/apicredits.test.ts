// What the Monthly API Credit card says about each Console organization, in
// words: the chips beside a label, the shut fold's flags, the bar or what
// stands in its place, the figures with their ≤ and ≥, the lines under a row
// and under the pool, and when a credit refills.
//
// Driven by the two golden documents, whose accounts between them carry the
// states the contract defines — ok, low, out, stale, needsSettings, pending,
// misconfigured, cacheTooOld — and by small patches of them for the row
// states neither reaches, and for the card Quota Glance builds itself when
// Quota Cache publishes no credit state. Every amount asserted is the server's
// own text: nothing here does money. Nothing is edited here since 0.8.0, so
// the copy says where it is: Quota Cache's page.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"

import {
  anySetHere,
  BRIEF_GAP_SECONDS,
  creditAnchor,
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
  editsInQuotaCache,
  ENTERED_HERE,
  markText,
  meterLine,
  meterStopped,
  migrationText,
  moneyText,
  notEditableText,
  poolNotes,
  refillWhen,
  setHere,
  setHereTitles,
  sharedIssues,
  sharedWho,
  shortOrganization,
  wantsReading,
} from "../src/lib/apicredits.ts"
import type { APICreditAccount, APICredits, Summary } from "../src/lib/types.ts"

function document(name: string): Summary {
  return JSON.parse(readFileSync(new URL(`../../testdata/golden/${name}.json`, import.meta.url), "utf8")) as Summary
}

function credits(name: string): APICredits {
  const doc = document(name)
  assert.ok(doc.apiCredits, name)
  return doc.apiCredits
}

const golden = credits("summary")
const degraded = credits("summary-degraded")
const NOW = document("summary").generatedAtEpoch

function account(set: APICredits, label: string): APICreditAccount {
  const found = set.accounts.find((item) => item.label === label)
  assert.ok(found, label)
  return found
}

/** A golden account with some fields replaced, for a row neither document holds. */
const patched = (set: APICredits, label: string, patch: Partial<APICreditAccount>): APICreditAccount => ({
  ...account(set, label),
  ...patch,
})

/** The sentence the server writes for meterGap. */
const GAP =
  "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it."

/** The pool's own line for a gap, shorter than the rows' sentence. */
const GAP_LINE = "Quota Cache was not counting for part of this period, so some spend may be missing."

/**
 * The golden pool with Quota Cache stopped two hours ago, as the dev
 * scenario meter-stopped builds it: every account a lower bound for the gap,
 * each with the server's one sentence for it.
 */
const stoppedCard: APICredits = {
  ...golden,
  meter: { ...golden.meter!, stoppedAtEpoch: NOW - 7200 },
  pool: { ...golden.pool, lowerBound: true },
  accounts: golden.accounts.map((item) => ({ ...item, lowerBound: true, dataIssues: ["meterGap"], issue: GAP })),
}

/** The fallback's sentence when Quota Cache has published nothing, which Quota Glance writes itself. */
const MISSING =
  "Quota Cache has not published this organization's credit. Update Quota Cache to 0.2.0 or newer and check that it is enabled."

/**
 * The card as Quota Glance builds it without Quota Cache's credit state, for
 * the golden organizations: listed from the snapshot, none counted, and the
 * values saved in Quota Glance awaiting Quota Cache's first start.
 */
const fallback: APICredits = {
  ...golden,
  pricing: { asOf: "", source: "", cacheWrites: "" },
  meter: null,
  editing: { available: false, reason: "quotaCache", migration: "awaitingImport" },
  pool: {
    ...golden.pool,
    hasEstimate: false,
    monthlyCredit: 0,
    monthlyCreditText: "",
    used: 0,
    usedText: "",
    left: 0,
    leftText: "",
    overageText: "",
    remainingFraction: 0,
    remainingPercent: 0,
    level: "",
    nextRefill: null,
    fullAtEpoch: null,
    fullInSeconds: null,
    countedCount: 0,
    missingCount: 5,
  },
  accounts: golden.accounts.map((item) => ({
    ...item,
    state: "cacheTooOld",
    counted: false,
    hasEstimate: false,
    basis: "",
    monthlyCreditText: "",
    monthlyCreditSource: "none",
    renewsSource: "none",
    leftText: "",
    usedText: "",
    spentText: "",
    spentSinceEpoch: null,
    level: "",
    cycleStartEpoch: null,
    renewsAtEpoch: null,
    renewsInSeconds: null,
    reading: null,
    cacheWriteExtra: 0,
    cacheWriteExtraText: "",
    dataIssues: ["cacheTooOld"],
    issue: MISSING,
    settings: { ...item.settings, revision: "", monthlyUsd: "", renews: "", reading: null, updatedAtEpoch: null },
  })),
  unlinked: [],
  orphans: [],
}

/** Golden accounts with an issue sentence each, by label; the rest unchanged. */
const withIssues = (issues: Record<string, string>, first = "meterGap"): APICreditAccount[] =>
  golden.accounts.map((item) =>
    item.label in issues ? { ...item, lowerBound: true, dataIssues: [first], issue: issues[item.label]! } : item,
  )

describe("creditLevel", () => {
  test("a healthy account says nothing", () => {
    for (const label of ["alpha", "bravo", "delta", "echo"]) assert.equal(creditLevel(account(golden, label)), null, label)
  })

  test("low carries what is left, as the server printed it", () => {
    assert.deepEqual(creditLevel(account(golden, "charlie")), { figure: "$32.00", word: "low", tone: "low" })
  })

  test("out is said with what is left, in red", () => {
    assert.deepEqual(creditLevel(account(degraded, "alpha")), { figure: "$0.00", word: "out", tone: "critical" })
  })

  test("critical with money left says critical, not out", () => {
    const nearly = patched(golden, "charlie", { level: "critical", leftText: "$7.00" })
    assert.deepEqual(creditLevel(nearly), { figure: "$7.00", word: "critical", tone: "critical" })
  })

  test("spend past the credit, not out, is a word in amber", () => {
    const over = patched(golden, "alpha", { overage: 40.1, overageText: "$40.10", level: "critical", dataIssues: ["overCredit"] })
    assert.deepEqual(creditLevel(over), { figure: "", word: "past credit", tone: "low" })
  })

  test("zero credit has no chip, even when there is spend past it", () => {
    const zero = patched(golden, "alpha", { level: "", overage: 12, dataIssues: ["zeroCredit", "overCredit"] })
    assert.equal(creditLevel(zero), null)
  })

  test("no estimate has no level to speak of", () => {
    for (const label of ["charlie", "delta", "echo", "foxtrot", "golf", "india", "hotel"]) {
      assert.equal(creditLevel(account(degraded, label)), null, label)
    }
  })
})

describe("creditCondition", () => {
  test("a counted, complete estimate says nothing", () => {
    for (const item of golden.accounts) assert.equal(creditCondition(item, golden.meter), null, item.label)
  })

  test("a stale meter carries the age of its last save", () => {
    assert.deepEqual(creditCondition(account(degraded, "bravo"), degraded.meter), {
      figure: "",
      word: "",
      tone: "warn",
      since: degraded.meter!.updatedAtEpoch,
    })
  })

  test("an out account adds nothing to its level", () => {
    assert.equal(creditCondition(account(degraded, "alpha"), degraded.meter), null)
  })

  test("a bound says incomplete", () => {
    assert.deepEqual(creditCondition(patched(golden, "echo", { lowerBound: true }), golden.meter), {
      figure: "",
      word: "incomplete",
      tone: "warn",
    })
  })

  test("an account left out says why in a word or two", () => {
    const words = Object.fromEntries(
      degraded.accounts.map((item) => [item.label, creditCondition(item, degraded.meter)]),
    )
    assert.deepEqual(words.charlie, { figure: "", word: "not set", tone: "warn" })
    assert.deepEqual(words.delta, { figure: "", word: "not set", tone: "warn" })
    assert.deepEqual(words.echo, { figure: "", word: "not counted yet", tone: "quiet" })
    for (const label of ["foxtrot", "golf", "hotel"]) assert.deepEqual(words[label], { figure: "", word: "not set up", tone: "warn" })
    // Neutral: the row's own sentence says why, which differs by cause.
    assert.deepEqual(words.india, { figure: "", word: "no figures", tone: "warn" })
  })

  test("an unknown state that is not counted is not passed off as fine", () => {
    const odd = patched(degraded, "echo", { state: "somethingNew", counted: false })
    assert.deepEqual(creditCondition(odd, degraded.meter), { figure: "", word: "not counted", tone: "quiet" })
  })
})

describe("markText", () => {
  test("an aged mark says its age", () => {
    assert.equal(markText({ figure: "", word: "", tone: "warn", since: 1 }, "2h"), "2h old")
    assert.equal(markText({ figure: "", word: "stale", tone: "warn" }, "2h"), "stale")
  })
})

describe("creditFlags", () => {
  const age = (since: number) => `${Math.round((NOW - since) / 3600)}h`
  const said = (flags: ReturnType<typeof creditFlags>) =>
    flags.map((flag) => [flag.name, flag.figure, flag.word, flag.tone].filter(Boolean).join(" "))

  test("names the low account, with the server's figure", () => {
    assert.deepEqual(said(creditFlags(golden.accounts, golden.meter, age)), ["charlie $32.00 low low"])
  })

  test("names every account worth opening the card for; one not counted yet is not named", () => {
    // charlie and delta lack different settings, and foxtrot, golf and hotel
    // have different problems, but each pair or trio is one chip.
    assert.deepEqual(said(creditFlags(degraded.accounts, degraded.meter, age)), [
      "alpha $0.00 out critical",
      "bravo 2h old low",
      "india no figures low",
      "2 not set low",
      "3 not set up low",
    ])
  })

  test("a condition every account shares is one chip with a count; levels stay their own", () => {
    const flags = creditFlags(stoppedCard.accounts, stoppedCard.meter, age)
    assert.deepEqual(said(flags), ["charlie $32.00 low low", "5 incomplete low"])
    // The count is the chip's figure, with no name before it.
    assert.deepEqual(flags[1], { id: "condition:incomplete", name: "", figure: "5", word: "incomplete", tone: "low" })
  })

  test("a condition one account carries stays on its own chip, beside its level", () => {
    const accounts = golden.accounts.map((item) => (item.label === "charlie" ? { ...item, lowerBound: true } : item))
    assert.deepEqual(said(creditFlags(accounts, golden.meter, age)), ["charlie $32.00 low · incomplete low"])
    const two = golden.accounts.map((item) =>
      item.label === "charlie" || item.label === "echo" ? { ...item, lowerBound: true } : item,
    )
    assert.deepEqual(said(creditFlags(two, golden.meter, age)), ["charlie $32.00 low low", "2 incomplete low"])
  })

  test("aged figures several accounts share say stale, once", () => {
    const accounts = golden.accounts.map((item) => ({ ...item, state: "stale", lowerBound: true }))
    assert.deepEqual(said(creditFlags(accounts, golden.meter, age)), ["charlie $32.00 low low", "5 stale low"])
    assert.equal(conditionKey({ figure: "", word: "", tone: "warn", since: 1 }), "stale")
  })
})

describe("creditBar", () => {
  test("a counted estimate draws its fraction in its level's ink", () => {
    assert.deepEqual(creditBar(account(golden, "alpha")), { kind: "bar", fraction: 0.8, level: "ok", empty: "" })
    assert.deepEqual(creditBar(account(golden, "charlie")), { kind: "bar", fraction: 0.32, level: "low", empty: "" })
  })

  test("out and past the credit are empty, on a red track", () => {
    assert.deepEqual(creditBar(account(degraded, "alpha")), { kind: "bar", fraction: 0, level: "critical", empty: "out" })
    const over = patched(golden, "alpha", { overage: 40.1, remainingFraction: 0, level: "critical", dataIssues: ["overCredit"] })
    assert.deepEqual(creditBar(over), { kind: "bar", fraction: 0, level: "critical", empty: "over" })
  })

  test("a reading above the credit is full, never past it", () => {
    const above = patched(golden, "bravo", { remainingFraction: 1.2 })
    assert.equal((creditBar(above) as { fraction: number }).fraction, 1)
  })

  test("says what to set, or why there is nothing to draw", () => {
    const text = (item: APICreditAccount) => {
      const bar = creditBar(item)
      return bar.kind === "none" ? bar.text : "bar"
    }
    assert.equal(text(account(degraded, "charlie")), "set the refill date")
    assert.equal(text(account(degraded, "delta")), "set the monthly credit")
    assert.equal(
      text(patched(degraded, "charlie", { dataIssues: ["needsCredit", "needsRefillDate"] })),
      "set credit and refill date",
    )
    assert.equal(text(account(degraded, "echo")), "waiting for the meter")
    assert.equal(text(account(degraded, "foxtrot")), "configuration needs fixing")
    assert.equal(text(account(degraded, "india")), "no figures from Quota Cache")
    assert.equal(text(patched(golden, "delta", { dataIssues: ["zeroCredit", "overCredit"] })), "no credit this month")
  })
})

describe("creditFigures", () => {
  test("a complete estimate: what is left of the credit, and what is used", () => {
    assert.deepEqual(creditFigures(account(golden, "alpha")), {
      left: "$160.00",
      leftMark: "",
      of: "$200.00",
      leftAlone: false,
      used: "$40.00",
      usedMark: "",
      usedWord: "used",
      past: "",
    })
  })

  test("a bound carries ≤ on what is left and ≥ on what is used", () => {
    const figures = creditFigures(account(degraded, "bravo"))
    assert.deepEqual([figures.leftMark, figures.left, figures.of, figures.usedMark, figures.used], ["≤", "$90.00", "$100.00", "≥", "$10.00"])
  })

  test("an account Anthropic refused prints what it is out of, with no bound marks", () => {
    const figures = creditFigures(account(degraded, "alpha"))
    assert.deepEqual([figures.leftMark, figures.left, figures.of, figures.usedMark, figures.used], ["", "$0.00", "$200.00", "", "$200.00"])
  })

  test("past the credit says by how much", () => {
    const over = patched(golden, "alpha", { left: 0, leftText: "$0.00", used: 200, usedText: "$200.00", overage: 40.1, overageText: "$40.10" })
    assert.equal(creditFigures(over).past, "$40.10")
  })

  test("needsSettings: a dash, and what was spent when it counts from somewhere", () => {
    const delta = creditFigures(account(degraded, "delta"))
    assert.deepEqual([delta.left, delta.used, delta.usedWord, delta.usedMark], [null, "$50.45", "spent", "≥"])
    const charlie = creditFigures(account(degraded, "charlie"))
    assert.deepEqual([charlie.left, charlie.used], [null, null])
  })

  test("needsSettings with a reading prints what is left, alone", () => {
    const reading = creditFigures(
      patched(degraded, "delta", { hasEstimate: true, leftText: "$106.00", monthlyCreditText: "", lowerBound: false }),
    )
    assert.deepEqual([reading.left, reading.leftAlone, reading.of], ["$106.00", true, ""])
  })

  test("a credit of $0.00 prints it, and what was spent", () => {
    const zero = creditFigures(
      patched(golden, "delta", {
        monthlyCreditText: "$0.00",
        leftText: "$0.00",
        spentText: "$12.00",
        dataIssues: ["zeroCredit", "overCredit"],
      }),
    )
    assert.deepEqual([zero.left, zero.of, zero.used, zero.usedWord], ["$0.00", "$0.00", "$12.00", "spent"])
  })

  test("nothing counted is dashes, never $0.00", () => {
    for (const label of ["echo", "foxtrot", "india", "hotel"]) {
      const figures = creditFigures(account(degraded, label))
      assert.deepEqual([figures.left, figures.used], [null, null], label)
    }
  })
})

describe("creditNotes", () => {
  test("a healthy account earns no line", () => {
    for (const label of ["alpha", "bravo", "charlie", "echo"]) assert.deepEqual(creditNotes(account(golden, label)), [], label)
  })

  test("the cache-write note, in the page's words around the server's figure", () => {
    assert.deepEqual(creditNotes(account(golden, "delta")), [
      {
        text: "Cache writes are priced at the 5-minute rate; at the 1-hour rate this would be $12.00 more.",
        tone: "quiet",
      },
    ])
  })

  test("an out account is red, dated by its refusal, and asks for a Console reading in Quota Cache", () => {
    const alpha = account(degraded, "alpha")
    assert.deepEqual(creditNotes(alpha), [
      { text: alpha.issue, tone: "bad", at: alpha.refusals.lastAtEpoch!, link: "reading" },
    ])
  })

  test("a Claude Code-based refusal that made it out is dated by that refusal", () => {
    const nearly = patched(degraded, "alpha", {
      dataIssues: ["refusedNearlySpent"],
      refusals: { total: 0, lastAtEpoch: null, claudeCodeTotal: 2, claudeCodeLastAtEpoch: 1789000000 },
    })
    assert.equal(creditNotes(nearly)[0]!.at, 1789000000)
  })

  test("the server's sentence, toned by the issue it is about", () => {
    const tone = (item: APICreditAccount) => creditNotes(item)[0]?.tone
    assert.equal(tone(account(degraded, "bravo")), "warn")
    assert.equal(tone(account(degraded, "charlie")), "warn")
    assert.equal(tone(account(degraded, "echo")), "quiet")
    assert.equal(tone(account(degraded, "foxtrot")), "warn")
    assert.equal(tone(account(degraded, "india")), "warn")
    assert.equal(tone(patched(golden, "alpha", { issue: "x", dataIssues: ["zeroCredit"] })), "quiet")
    assert.equal(tone(patched(golden, "alpha", { issue: "x", dataIssues: ["readingOnRefillDay"] })), "quiet")
    assert.equal(tone(patched(golden, "alpha", { issue: "x", dataIssues: ["claudeCodeRefused"] })), "warn")
    assert.equal(tone(patched(golden, "alpha", { issue: "x", dataIssues: ["noTraffic"] })), "quiet")
    for (const label of ["foxtrot", "golf", "india", "hotel"]) {
      assert.equal(creditNotes(account(degraded, label))[0]?.text, account(degraded, label).issue, label)
    }
  })

  test("a sentence the pool says once for several rows is not under them; the rest of the row's lines are", () => {
    const shared = sharedIssues(stoppedCard.accounts)
    assert.deepEqual(creditNotes(account(stoppedCard, "alpha"), shared), [])
    assert.deepEqual(creditNotes(account(stoppedCard, "delta"), shared), [
      {
        text: "Cache writes are priced at the 5-minute rate; at the 1-hour rate this would be $12.00 more.",
        tone: "quiet",
      },
    ])
    // A sentence one row carries stays on it.
    const one = withIssues({ alpha: GAP })
    assert.equal(creditNotes(one[0]!, sharedIssues(one))[0]!.text, GAP)
  })

  test("rows a Console reading would settle ask for one", () => {
    assert.equal(wantsReading(account(degraded, "alpha")), true)
    assert.equal(wantsReading(account(degraded, "bravo")), true)
    assert.equal(creditNotes(account(degraded, "bravo"))[0]!.link, "reading")
    for (const issue of ["meterStartedLate", "meterGap", "readingOnRefillDay", "claudeCodeRefused"]) {
      assert.equal(wantsReading(patched(golden, "alpha", { dataIssues: [issue] })), true, issue)
    }
    // A reading stands in for a missing refill date; it does not for a missing credit.
    assert.equal(wantsReading(account(degraded, "charlie")), true)
    for (const label of ["delta", "echo", "india"]) assert.equal(wantsReading(account(degraded, label)), false, label)
  })

  test("a row that lacks a setting asks for it to be set in Quota Cache", () => {
    // charlie lacks only its refill date, which a reading would stand in for;
    // delta lacks its credit. Both are set on Quota Cache's page.
    assert.equal(creditNotes(account(degraded, "charlie"))[0]!.link, "settings")
    assert.equal(creditNotes(account(degraded, "delta"))[0]!.link, "settings")
    // Nothing to set or enter: no link.
    for (const label of ["echo", "foxtrot", "india"]) assert.equal(creditNotes(account(degraded, label))[0]!.link, undefined, label)
  })

  test("no link where Quota Cache's page cannot edit the row either", () => {
    // A reason no page could lift, or Quota Cache unable to use its saved
    // settings: the line keeps its sentence and loses its link.
    for (const reason of ["noOrganization", "duplicateOrganization", "overLimit", "cacheTooOld", "settingsUnreadable", "legacyUnreadable"]) {
      const alpha = account(degraded, "alpha")
      const locked = { ...alpha, settings: { ...alpha.settings, notEditableReason: reason } }
      assert.equal(editsInQuotaCache(locked), false, reason)
      assert.deepEqual(creditNotes(locked), [{ text: alpha.issue, tone: "bad", at: alpha.refusals.lastAtEpoch! }], reason)
    }
    assert.equal(editsInQuotaCache(account(degraded, "alpha")), true)
  })
})

describe("sharedIssues", () => {
  test("groups the rows' sentences by exact string, in the order of the first row to carry each", () => {
    const groups = sharedIssues(stoppedCard.accounts)
    assert.equal(groups.length, 1)
    assert.deepEqual(groups[0]!.ids, stoppedCard.accounts.map((item) => item.id))
    assert.deepEqual(groups[0]!.note, { text: GAP, tone: "warn" })
    assert.equal(groups[0]!.issue, "meterGap")
    assert.equal(groups[0]!.link, "reading")
  })

  test("a sentence one row carries is not shared", () => {
    assert.deepEqual(sharedIssues(withIssues({ alpha: GAP })), [])
    assert.deepEqual(sharedIssues(golden.accounts), [])
    // The degraded document's sentences are each its own.
    assert.deepEqual(sharedIssues(degraded.accounts), [])
  })

  test("only the exact string: a different letter, space or stop is another sentence", () => {
    const groups = sharedIssues(
      withIssues({ alpha: GAP, bravo: GAP, charlie: `${GAP} `, delta: GAP.replace("Quota", "quota"), echo: GAP.slice(0, -1) }),
    )
    assert.deepEqual(groups.map((group) => group.ids.map((id) => golden.accounts.find((item) => item.id === id)!.label)), [
      ["alpha", "bravo"],
    ])
  })

  test("two refusals group only at the same instant", () => {
    const out = account(degraded, "alpha")
    const twin = (at: number): APICreditAccount => ({
      ...out,
      id: `org-${at}`,
      issue: "Anthropic refused a request for low credit, so this credit is spent.",
      refusals: { ...out.refusals, lastAtEpoch: at },
    })
    assert.deepEqual(sharedIssues([twin(1), twin(2)]), [])
    const same = sharedIssues([twin(1), { ...twin(1), id: "org-other" }])
    assert.equal(same.length, 1)
    assert.equal(same[0]!.note.at, 1)
  })

  test("its link is one a row's line would have ended with: a row Quota Cache's page can edit", () => {
    const accounts = stoppedCard.accounts.map((item) =>
      item.label === "alpha" ? { ...item, settings: { ...item.settings, notEditableReason: "overLimit" } } : item,
    )
    assert.equal(sharedIssues(accounts)[0]!.link, "reading")
    const none = stoppedCard.accounts.map((item) => ({ ...item, settings: { ...item.settings, notEditableReason: "settingsUnreadable" } }))
    assert.equal(sharedIssues(none)[0]!.link, null)
    // A sentence no reading settles has no link at all.
    const stale = withIssues({ alpha: "x", bravo: "x" }, "stale")
    assert.deepEqual(sharedIssues(stale)[0]!.note, { text: "x", tone: "warn" })
    assert.equal(sharedIssues(stale)[0]!.link, null)
  })

  test("a sentence about an organization Quota Cache has no figures for stays on its row", () => {
    assert.deepEqual(sharedIssues(fallback.accounts), [])
  })
})

describe("sharedWho", () => {
  const ids = (...labels: string[]) => labels.map((label) => account(golden, label).id)

  test("two or three accounts by name, as a sentence lists them", () => {
    assert.equal(sharedWho(ids("alpha", "bravo"), golden.accounts), "alpha and bravo")
    assert.equal(sharedWho(ids("alpha", "bravo", "charlie"), golden.accounts), "alpha, bravo and charlie")
    // In the card's order, whatever order they were given in.
    assert.equal(sharedWho(ids("charlie", "alpha"), golden.accounts), "alpha and charlie")
  })

  test("every account, by count", () => {
    assert.equal(sharedWho(ids("alpha", "bravo", "charlie", "delta", "echo"), golden.accounts), "All 5 accounts")
    assert.equal(sharedWho(ids("alpha", "bravo"), golden.accounts.slice(0, 2)), "Both accounts")
  })

  test("every counted account, when the card has others the pool leaves out", () => {
    const accounts = golden.accounts.map((item) => (item.label === "echo" ? { ...item, counted: false } : item))
    assert.equal(sharedWho(ids("alpha", "bravo", "charlie", "delta"), accounts), "All 4 counted accounts")
    assert.equal(sharedWho(ids("alpha", "bravo", "charlie"), accounts), "alpha, bravo and charlie")
  })
})

describe("conditionsSaidOnce", () => {
  test("a condition every counted row carries, and the pool explains, leaves the rows", () => {
    const shared = sharedIssues(stoppedCard.accounts)
    assert.deepEqual([...conditionsSaidOnce(stoppedCard.accounts, stoppedCard.meter, shared)], ["incomplete"])
  })

  test("a condition only some rows carry keeps their chips", () => {
    const accounts = withIssues({ alpha: GAP, bravo: GAP, charlie: GAP })
    assert.deepEqual([...conditionsSaidOnce(accounts, golden.meter, sharedIssues(accounts))], [])
  })

  test("every row incomplete, but one says why on its own row: the chips stay", () => {
    const accounts = withIssues({ alpha: GAP, bravo: GAP, charlie: GAP, delta: GAP, echo: "Some requests used a model with no listed price." })
    assert.deepEqual([...conditionsSaidOnce(accounts, golden.meter, sharedIssues(accounts))], [])
  })

  test("with nothing counted, a condition every row carries", () => {
    const pending = golden.accounts.map((item) => ({
      ...item,
      state: "pending",
      counted: false,
      dataIssues: ["meterMissing"],
      issue: "Quota Cache has not saved an API meter yet.",
    }))
    assert.deepEqual([...conditionsSaidOnce(pending, golden.meter, sharedIssues(pending))], ["not counted yet"])
  })
})

describe("the basis and the marks for values entered in Quota Cache", () => {
  test("what the estimate starts from, for the bar's hover", () => {
    assert.deepEqual(creditBasis(account(golden, "bravo")), {
      kind: "reading",
      remainingText: "$180.00",
      atEpoch: 1788968400,
      spentSinceText: "$8.00",
    })
    assert.deepEqual(creditBasis(account(golden, "alpha")), { kind: "credit", monthlyCreditText: "$200.00", spentText: "$40.00" })
    assert.equal(creditBasis(account(degraded, "charlie")), null)
  })

  test("a value entered on Quota Cache's page says what the config has instead", () => {
    assert.deepEqual(setHere(account(golden, "bravo")), { credit: "100", refill: null })
    assert.deepEqual(setHere(account(golden, "alpha")), { credit: null, refill: null })
    const both = patched(golden, "alpha", { renewsSource: "dashboard", monthlyCreditSource: "dashboard" })
    assert.deepEqual(setHere(both), { credit: "200", refill: "2026-09-29" })
    assert.equal(anySetHere(golden.accounts), true)
    assert.equal(anySetHere(degraded.accounts), false)
  })

  test("its hover names the config's value, so Glance still tells config from entered", () => {
    const day = (epoch: number) => `day ${epoch}`
    assert.deepEqual(setHereTitles(account(golden, "bravo"), day), {
      credit: "Entered in Quota Cache. Its config says $100.00.",
      refill: null,
    })
    const both = patched(golden, "alpha", { renewsSource: "dashboard", monthlyCreditSource: "dashboard" })
    assert.deepEqual(setHereTitles(both, day), {
      credit: "Entered in Quota Cache. Its config says $200.00.",
      refill: `Entered in Quota Cache. Its config says day ${Date.UTC(2026, 8, 29) / 1000}.`,
    })
    // Nothing usable in the config.
    const bare = patched(golden, "bravo", {
      renewsSource: "dashboard",
      settings: { ...account(golden, "bravo").settings, configMonthlyUsd: "", configRenewsInvalid: true },
    })
    assert.deepEqual(setHereTitles(bare, day), {
      credit: "Entered in Quota Cache. Not in its config.",
      refill: "Entered in Quota Cache. Not in its config.",
    })
    assert.equal(ENTERED_HERE, "entered in Quota Cache")
  })

  test("money and dates as the hover prints them, by string, with no float", () => {
    assert.equal(moneyText("1200.5"), "$1,200.50")
    assert.equal(moneyText("0"), "$0.00")
    assert.equal(moneyText("007"), "$7.00")
    assert.equal(dateEpoch("2026-10-09"), 1791504000)
  })
})

describe("poolNotes", () => {
  test("every account counted, no gap: only the unlinked organization", () => {
    assert.deepEqual(poolNotes(golden, NOW), [
      {
        text: "CPA sent traffic from Console organization 0000…000f (37 requests, last {age} ago), which no organization in Quota Cache names. If it is yours, add it on Quota Cache's page.",
        tone: "warn",
        since: golden.unlinked[0]!.lastSeenEpoch,
      },
    ])
  })

  test("the degraded pool: coverage, both unlinked reasons, and the gap — in that order", () => {
    assert.deepEqual(
      poolNotes(degraded, NOW).map((note) => note.text),
      [
        "counts 2 of 9 accounts · 7 are left out",
        "CPA sent traffic from Console organization 0000…000f (37 requests, last {age} ago), which is in an item past the first 16 in claude-api-credits, so it is not counted.",
        "CPA sent traffic from Console organization 0000…000e (3 requests, last {age} ago), which no organization in Quota Cache names. If it is yours, add it on Quota Cache's page.",
        "Quota Cache was not counting for part of this period, so some spend may be missing.",
      ],
    )
  })

  test("one account left out is named, with what to set", () => {
    const accounts = golden.accounts.map((item) =>
      item.label === "delta" ? { ...item, state: "needsSettings", counted: false, dataIssues: ["needsCredit"] } : item,
    )
    const pool = { ...golden.pool, countedCount: 4, missingCount: 1 }
    assert.equal(
      poolNotes({ ...golden, accounts, pool }, NOW)[0]!.text,
      "counts 4 of 5 accounts · delta is left out: set its monthly credit",
    )
  })

  test("past three unlinked organizations, the rest are counted", () => {
    const one = golden.unlinked[0]!
    const unlinked = [1, 2, 3, 4, 5].map((n) => ({ ...one, organizationId: `00000000-0000-4000-8000-00000000000${n}`, requests: 1 }))
    const texts = poolNotes({ ...golden, unlinked }, NOW).map((note) => note.text)
    assert.equal(texts.length, 4)
    assert.match(texts[0]!, /\(1 request, last/)
    assert.equal(texts[3], "and 2 more organizations")
  })

  test("a gap, or a stop, before the oldest anchor says nothing", () => {
    const meter = { ...golden.meter!, gaps: [{ fromEpoch: 1, toEpoch: 2, reason: "shutdown" }] }
    assert.equal(poolNotes({ ...golden, meter }, NOW).length, 1)
  })

  test("a meter stopped for 5 minutes or more is a gap; a briefer stop is not (decision 1)", () => {
    const stopped = (ago: number) => ({ ...golden.meter!, stoppedAtEpoch: NOW - ago })
    const gapLine = "Quota Cache was not counting for part of this period, so some spend may be missing."
    assert.ok(poolNotes({ ...golden, meter: stopped(BRIEF_GAP_SECONDS) }, NOW).some((note) => note.text === gapLine))
    assert.ok(!poolNotes({ ...golden, meter: stopped(BRIEF_GAP_SECONDS - 1) }, NOW).some((note) => note.text === gapLine))
  })

  test("records lost after the anchor say so, the first that applies", () => {
    const after = NOW - 60
    const line = (meter: Partial<NonNullable<APICredits["meter"]>>) =>
      poolNotes({ ...golden, meter: { ...golden.meter!, ...meter } }, NOW).at(-1)!.text
    assert.equal(
      line({ lastDroppedEpoch: after, lastRejectedEpoch: after, lastUnattributedEpoch: after }),
      "Quota Cache dropped usage records it could not keep up with, so some spend is missing.",
    )
    assert.equal(
      line({ lastRejectedEpoch: after, lastUnattributedEpoch: after }),
      "Some failed requests could not be matched to an organization, so some spend may be missing.",
    )
    assert.equal(line({ lastRejectedEpoch: after }), "Quota Cache could not read some usage records from CPA, so some spend may be missing.")
    // The degraded meter rejected records before either counted anchor.
    assert.ok(!poolNotes(degraded, NOW).some((note) => note.text.includes("could not read")))
  })

  test("a shared sentence is said once, after whom it is about, with one link", () => {
    const notes = poolNotes(stoppedCard, NOW)
    assert.deepEqual(notes[0], {
      text: `All 5 accounts: ${GAP}`,
      tone: "warn",
      link: "reading",
    })
    assert.match(notes[1]!.text, /^CPA sent traffic from Console organization/)
    assert.equal(notes.length, 2)
  })

  test("never the same thing twice: a shared gap sentence stands for the pool's gap line", () => {
    assert.ok(!poolNotes(stoppedCard, NOW).some((note) => note.text === GAP_LINE))
    // Shared by some rows only, it still stands for it.
    const some = { ...stoppedCard, accounts: withIssues({ alpha: GAP, bravo: GAP }) }
    const texts = poolNotes(some, NOW).map((note) => note.text)
    assert.ok(texts.includes(`alpha and bravo: ${GAP}`))
    assert.ok(!texts.includes(GAP_LINE))
    // One row's sentence is under that row, so the pool keeps its own line.
    const one = { ...stoppedCard, accounts: withIssues({ alpha: GAP }) }
    assert.ok(poolNotes(one, NOW).some((note) => note.text === GAP_LINE))
  })

  test("a shared record-loss sentence stands for the pool's line about the same loss", () => {
    const dropped = "Quota Cache dropped usage records it could not keep up with, so some spend is missing."
    const meter = { ...golden.meter!, lastDroppedEpoch: NOW - 60 }
    const shared = { ...golden, meter, accounts: withIssues({ alpha: dropped, bravo: dropped }, "meterDropped") }
    assert.deepEqual(
      poolNotes(shared, NOW).filter((note) => note.text.endsWith(dropped)).map((note) => note.text),
      [`alpha and bravo: ${dropped}`],
    )
    // A different loss is not the same thing, and is still said.
    const unread = { ...shared, meter: { ...meter, lastDroppedEpoch: null, lastRejectedEpoch: NOW - 60 } }
    assert.ok(poolNotes(unread, NOW).some((note) => note.text.startsWith("Quota Cache could not read")))
  })

  test("an account's anchor: its reading, its cycle, else what its spend counts from", () => {
    assert.equal(creditAnchor(account(golden, "bravo")), 1788968400)
    assert.equal(creditAnchor(account(golden, "alpha")), account(golden, "alpha").cycleStartEpoch)
    assert.equal(creditAnchor(account(degraded, "delta")), account(degraded, "delta").spentSinceEpoch)
    assert.equal(creditAnchor(account(degraded, "charlie")), null)
  })
})

describe("the source line", () => {
  test("updated, not updated, or stopped", () => {
    assert.deepEqual(meterLine(golden.meter, NOW), { kind: "updated", since: golden.meter!.updatedAtEpoch })
    assert.deepEqual(meterLine(degraded.meter, NOW), { kind: "stale", since: degraded.meter!.updatedAtEpoch })
    assert.deepEqual(meterLine({ ...degraded.meter!, stoppedAtEpoch: NOW - 7200 }, NOW), { kind: "stopped", since: NOW - 7200 })
    assert.deepEqual(meterLine(null, NOW), { kind: "none" })
  })

  test("a stop under 5 minutes is a restart, and says nothing yet", () => {
    const meter = { ...golden.meter!, stoppedAtEpoch: NOW - 30 }
    assert.equal(meterStopped(meter, NOW), false)
    assert.equal(meterLine(meter, NOW).kind, "updated")
  })
})

describe("names", () => {
  test("an item with no usable label goes by its id", () => {
    assert.equal(creditName({ id: "item-3", label: "" }), "item-3")
    assert.equal(creditName(account(golden, "alpha")), "alpha")
  })

  test("an organization id is named by its first and last four hex digits", () => {
    assert.equal(shortOrganization("12345678-1234-5678-1234-567812345678"), "1234…5678")
    assert.equal(shortOrganization("00000000-0000-4000-8000-00000000000a"), "0000…000a")
  })
})

describe("editing copy: why nothing is edited here, and where it is", () => {
  test("each reason no page could lift, minus here", () => {
    assert.equal(notEditableText("noOrganization"), "Add this organization's organization-id in Quota Cache's config to edit it.")
    assert.equal(
      notEditableText("duplicateOrganization"),
      "Another item in Quota Cache's config has the same organization-id; edit that one.",
    )
    assert.equal(notEditableText("overLimit"), "Only the first 16 items in claude-api-credits can be edited.")
    assert.equal(notEditableText("cacheTooOld"), "Update Quota Cache to edit this organization.")
    // The card's own reasons are the card's to say.
    for (const reason of ["quotaCache", "settingsUnreadable", "legacyUnreadable", ""]) assert.equal(notEditableText(reason), null, reason)
    // The degraded document carries one of each, and quotaCache everywhere else.
    const reasons = degraded.accounts.map((item) => item.settings.notEditableReason)
    for (const reason of ["noOrganization", "duplicateOrganization", "overLimit", "cacheTooOld", "quotaCache"]) assert.ok(reasons.includes(reason), reason)
  })

  test("nothing on either card is editable here", () => {
    for (const set of [golden, degraded]) {
      assert.equal(set.editing.available, false)
      assert.equal(set.editing.reason, "quotaCache")
      for (const item of set.accounts) assert.equal(item.settings.editable, false, item.label)
    }
  })

  test("why nothing is edited here, by reason", () => {
    assert.equal(editingOffText(golden.editing), "Change these on Quota Cache's page in CPA's management console.")
    assert.equal(
      editingOffText({ available: false, reason: "settingsUnreadable" }),
      "Quota Cache cannot read its saved settings, so only config values apply. See Quota Cache's page.",
    )
    assert.equal(
      editingOffText({ available: false, reason: "legacyUnreadable" }),
      "Quota Glance's settings.json could not be read, so Quota Cache imported nothing and only config values apply. Fix it or move it aside.",
    )
    // Only a plugin older than 0.8.0 offered editing here.
    assert.equal(editingOffText({ available: true, reason: "" }), null)
  })

  test("the foot says more only while Quota Cache cannot use its saved settings", () => {
    assert.equal(editingTrouble(golden.editing), null)
    assert.equal(editingTrouble({ available: false, reason: "settingsUnreadable" }), editingOffText({ available: false, reason: "settingsUnreadable" }))
    assert.equal(editingTrouble({ available: false, reason: "legacyUnreadable" }), editingOffText({ available: false, reason: "legacyUnreadable" }))
  })
})

describe("the migration line: whether Quota Glance's saved settings reached Quota Cache", () => {
  const MIGRATION = {
    notImported:
      "Quota Cache has not imported the settings saved in Quota Glance. Import them on Quota Cache's page; its README says what to do if the page does not offer it.",
    changedSinceImport: "Quota Glance's saved settings changed after Quota Cache imported them; open Quota Cache's page.",
    awaitingImport:
      "Your monthly credits, refill dates, Console readings and renewal dates saved in Quota Glance are kept, and Quota Cache 0.2.0 imports them when it starts.",
  }

  test("each value's copy, and nothing to say for the rest", () => {
    for (const [migration, text] of Object.entries(MIGRATION)) assert.equal(migrationText(migration), text, migration)
    assert.equal(migrationText(""), null)
    assert.equal(migrationText("somethingNew"), null)
  })

  test("a pool line after the coverage line: amber while values are not in Quota Cache, quiet while kept for it", () => {
    const with_ = (migration: string) => ({ ...golden, editing: { ...golden.editing, migration } })
    assert.deepEqual(poolNotes(with_("notImported"), NOW)[0], { text: MIGRATION.notImported, tone: "warn" })
    assert.deepEqual(poolNotes(with_("changedSinceImport"), NOW)[0], { text: MIGRATION.changedSinceImport, tone: "warn" })
    // The degraded pool leaves accounts out: the coverage line comes first.
    const texts = poolNotes({ ...degraded, editing: { ...degraded.editing, migration: "notImported" } }, NOW).map((note) => note.text)
    assert.deepEqual(texts.slice(0, 2), ["counts 2 of 9 accounts · 7 are left out", MIGRATION.notImported])
    // Nothing to say, or an older plugin that sends no migration at all.
    assert.equal(poolNotes(golden, NOW).length, 1)
    const { migration: _, ...older } = golden.editing
    assert.equal(poolNotes({ ...golden, editing: older }, NOW).length, 1)
  })
})

describe("without Quota Cache's credit state: the card Quota Glance builds itself", () => {
  test("every organization listed, none counted, each with its own sentence", () => {
    assert.equal(fallback.accounts.length, 5)
    for (const item of fallback.accounts) {
      assert.deepEqual(creditCondition(item, fallback.meter), { figure: "", word: "no figures", tone: "warn" })
      assert.deepEqual(creditBar(item), { kind: "none", text: "no figures from Quota Cache" })
      assert.deepEqual([creditFigures(item).left, creditFigures(item).used], [null, null])
      assert.deepEqual(creditNotes(item), [{ text: MISSING, tone: "warn" }])
    }
    // The chips stay on every row: no line says it for them.
    assert.deepEqual([...conditionsSaidOnce(fallback.accounts, fallback.meter, sharedIssues(fallback.accounts))], [])
  })

  test("the shut fold names them as one chip", () => {
    assert.deepEqual(
      creditFlags(fallback.accounts, fallback.meter, () => "").map((flag) => [flag.name, flag.figure, flag.word, flag.tone]),
      [["", "5", "no figures", "low"]],
    )
  })

  test("the pool counts none of them, then says the saved settings are kept for Quota Cache's first start", () => {
    assert.deepEqual(poolNotes(fallback, NOW), [
      { text: "counts 0 of 5 accounts · 5 are left out", tone: "warn" },
      {
        text: "Your monthly credits, refill dates, Console readings and renewal dates saved in Quota Glance are kept, and Quota Cache 0.2.0 imports them when it starts.",
        tone: "quiet",
      },
    ])
    // One organization: named, and why in the neutral words.
    const one = { ...fallback, accounts: fallback.accounts.slice(0, 1), pool: { ...fallback.pool, accountCount: 1, missingCount: 1 } }
    assert.equal(poolNotes(one, NOW)[0]!.text, "counts 0 of 1 accounts · alpha is left out: Quota Cache has no figures for it")
  })

  test("nothing entered there is known, so nothing carries the mark", () => {
    assert.equal(anySetHere(fallback.accounts), false)
  })
})

describe("refillWhen", () => {
  const DAY = 86400
  const ref = 1791547200 // 2026-10-09T12:00Z

  test("calendar days while a day or more is left, agreeing with the UTC date beside them", () => {
    const oct28 = 1793145600 // 2026-10-28T00:00Z
    assert.deepEqual(refillWhen(oct28, null, ref), { kind: "days", days: 19 })
    assert.deepEqual(refillWhen(oct28, null, ref - 11 * 3600), { kind: "days", days: 19 })
    assert.deepEqual(refillWhen(oct28, null, ref + 11 * 3600), { kind: "days", days: 19 })
  })

  test("the span itself inside the last day", () => {
    assert.deepEqual(refillWhen(ref + 3600, null, ref), { kind: "within", seconds: 3600 })
  })

  test("the first day of a cycle says it refilled today", () => {
    const start = ref - 12 * 3600
    assert.deepEqual(refillWhen(start + 30 * DAY, start, ref), { kind: "refilledToday" })
    assert.deepEqual(refillWhen(start + 30 * DAY, start, start + DAY), { kind: "days", days: 29 })
  })

  test("no usable date, and a date passed before the next document", () => {
    assert.deepEqual(refillWhen(null, null, ref), { kind: "none" })
    assert.deepEqual(refillWhen(ref - 1, null, ref), { kind: "due" })
  })
})


test("a brief gap sent by an older document is still ignored by pool notes", () => {
  const credits = structuredClone(golden)
  credits.unlinked = []
  credits.meter!.gaps = [{ fromEpoch: NOW - 300, toEpoch: NOW - 1, reason: "shutdown" }]
  assert.equal(poolNotes(credits, NOW).some((note) => note.text.includes("not counting")), false)
  credits.meter!.gaps[0]!.fromEpoch--
  assert.equal(poolNotes(credits, NOW).some((note) => note.text.includes("not counting")), true)
})
