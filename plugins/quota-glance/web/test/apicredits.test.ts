// What the Monthly API Credit card says about each Console organization, in
// words: the chips beside a label, the shut fold's flags, the lines under a
// row and under the pool, and when a credit refills.
//
// Driven by the two golden documents, whose accounts between them carry every
// state the contract defines — ok, low, out, overspent, stale, failed with and
// without a reading, today not reported, misconfigured, duplicate, pending —
// so these assert what the card prints for the documents the server writes.
// Every amount asserted is the server's own text: nothing here does money.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"

import {
  creditCondition,
  creditFlags,
  creditLevel,
  creditName,
  creditNoBar,
  creditNotes,
  lastRead,
  markText,
  poolNotes,
  refillWhen,
} from "../src/lib/apicredits.ts"
import type { APICreditAccount, APICredits, Summary } from "../src/lib/types.ts"

function credits(name: string): APICredits {
  const doc = JSON.parse(
    readFileSync(new URL(`../../testdata/golden/${name}.json`, import.meta.url), "utf8"),
  ) as Summary
  assert.ok(doc.apiCredits, name)
  return doc.apiCredits
}

const golden = credits("summary")
const degraded = credits("summary-degraded")

function account(set: APICredits, label: string): APICreditAccount {
  const found = set.accounts.find((item) => item.label === label)
  assert.ok(found, label)
  return found
}

describe("creditLevel", () => {
  test("a healthy account says nothing", () => {
    assert.equal(creditLevel(account(golden, "siphorchannel")), null)
    assert.equal(creditLevel(account(golden, "agency-team")), null)
  })

  test("low and out carry what is left, as the server printed it", () => {
    assert.deepEqual(creditLevel(account(golden, "chasibnoor")), { figure: "$32.00", word: "low", tone: "low" })
    assert.deepEqual(creditLevel(account(golden, "noor")), { figure: "$0.00", word: "out", tone: "critical" })
  })

  test("critical with money left says critical, not out", () => {
    assert.deepEqual(creditLevel(account(degraded, "chasibnoor")), {
      figure: "$7.00",
      word: "critical",
      tone: "critical",
    })
  })

  test("spent past the credit shows the overage, not the zero left", () => {
    assert.deepEqual(creditLevel(account(golden, "noorchasib")), { figure: "$36.40", word: "over", tone: "critical" })
  })

  test("no reading has no level to speak of", () => {
    for (const label of ["noor", "agency-team", "siphorchannel-2", "new-org"]) {
      assert.equal(creditLevel(account(degraded, label)), null, label)
    }
  })
})

describe("creditCondition", () => {
  test("a current, counted reading says nothing — today not reported included", () => {
    for (const item of golden.accounts) assert.equal(creditCondition(item), null, item.label)
    assert.equal(creditCondition(account(degraded, "noorchasib")), null)
  })

  test("a stale or failed reading that is still counted carries its age", () => {
    const stale = account(degraded, "siphorchannel")
    assert.deepEqual(creditCondition(stale), { figure: "", word: "", tone: "warn", since: stale.observedAtEpoch! })
    const failed = account(degraded, "chasibnoor")
    assert.deepEqual(creditCondition(failed), { figure: "", word: "", tone: "warn", since: failed.observedAtEpoch! })
  })

  test("a refused key is said as one even while the last reading stays counted", () => {
    // Rule 3: the failure that only the reader can fix keeps the reading, and
    // must not read as an ordinary wait.
    const failed = account(degraded, "chasibnoor")
    for (const [issue, word] of [
      ["keyRejected", "key rejected"],
      ["keyForbidden", "key not allowed"],
      ["costReportUnavailable", "no cost report"],
    ] as const) {
      assert.deepEqual(
        creditCondition({ ...failed, dataIssues: ["observeError", issue] }),
        { figure: "", word, tone: "bad", since: failed.observedAtEpoch! },
        issue,
      )
    }
    // A wait stays a wait.
    assert.deepEqual(creditCondition({ ...failed, dataIssues: ["observeError", "rateLimited"] }), {
      figure: "",
      word: "",
      tone: "warn",
      since: failed.observedAtEpoch!,
    })
  })

  test("an account left out says why in a word or two", () => {
    assert.deepEqual(creditCondition(account(degraded, "noor")), { figure: "", word: "key rejected", tone: "bad" })
    assert.deepEqual(creditCondition(account(degraded, "agency-team")), {
      figure: "",
      word: "not set up",
      tone: "warn",
    })
    assert.deepEqual(creditCondition(account(degraded, "siphorchannel-2")), {
      figure: "",
      word: "duplicate",
      tone: "quiet",
    })
    assert.deepEqual(creditCondition(account(degraded, "new-org")), {
      figure: "",
      word: "no reading yet",
      tone: "quiet",
    })
  })

  test("each read failure the contract names has its own words", () => {
    const failing = (issue: string) =>
      creditCondition({ hasReading: false, state: "error", dataIssues: ["observeError", issue], observedAtEpoch: null })
    assert.equal(failing("keyForbidden")?.word, "key not allowed")
    assert.equal(failing("costReportUnavailable")?.word, "no cost report")
    assert.deepEqual(failing("rateLimited"), { figure: "", word: "rate limited", tone: "warn" })
    assert.equal(failing("unsupportedCurrency")?.word, "not in USD")
    assert.equal(failing("amountInvalid")?.word, "unreadable report")
    assert.deepEqual(failing("somethingNew"), { figure: "", word: "read failed", tone: "bad" })
  })

  test("an unknown state with no reading is not passed off as fine", () => {
    assert.equal(
      creditCondition({ hasReading: false, state: "future", dataIssues: [], observedAtEpoch: null })?.word,
      "no reading",
    )
  })
})

describe("markText", () => {
  test("an aged mark says its age, after the failure when there is one", () => {
    assert.equal(markText({ figure: "", word: "", tone: "warn", since: 1 }, "40m"), "40m old")
    assert.equal(markText({ figure: "", word: "key rejected", tone: "bad", since: 1 }, "40m"), "key rejected · 40m old")
    assert.equal(markText({ figure: "", word: "duplicate", tone: "quiet" }, ""), "duplicate")
  })
})

describe("creditFlags", () => {
  const age = (now: number) => (since: number) => `${Math.round((now - since) / 60)}m`

  test("names the low, out and overspent accounts, with the server's figure", () => {
    assert.deepEqual(
      creditFlags(golden.accounts, age(0)).map((flag) => [flag.name, flag.figure, flag.word, flag.tone]),
      [
        ["chasibnoor", "$32.00", "low", "low"],
        ["noor", "$0.00", "out", "critical"],
        ["noorchasib", "$36.40", "over", "critical"],
      ],
    )
  })

  test("an aged level carries its age; a duplicate or an organization not read yet is not named", () => {
    const now = 1_789_012_800
    assert.deepEqual(
      creditFlags(degraded.accounts, age(now)).map((flag) => [flag.name, flag.figure, flag.word, flag.tone]),
      [
        ["siphorchannel", "$70.00", "low · 120m old", "low"],
        ["chasibnoor", "$7.00", "critical · 40m old", "critical"],
        ["noor", "", "key rejected", "critical"],
        ["agency-team", "", "not set up", "low"],
      ],
    )
  })

  test("a refused key on a counted reading is red, beside its level", () => {
    const now = 1_789_012_800
    const failed = { ...account(degraded, "chasibnoor"), dataIssues: ["observeError", "keyRejected"] }
    const [flag] = creditFlags([failed], age(now))
    assert.equal(flag?.word, "critical · key rejected · 40m old")
    assert.equal(flag?.tone, "critical")
  })
})

describe("creditNoBar", () => {
  test("says why there is no bar to draw", () => {
    // Its renews is set but not a date, so there is no refill instant.
    assert.equal(creditNoBar(account(degraded, "agency-team")), "refill date needs fixing")
    const team = account(degraded, "agency-team")
    assert.equal(creditNoBar({ ...team, monthlyCreditText: "", renewsAtEpoch: 1 }), "monthly credit needs fixing")
    assert.equal(creditNoBar({ ...team, monthlyCreditText: "" }), "credit and refill date need fixing")
    assert.equal(creditNoBar({ ...team, renewsAtEpoch: 1 }), "configuration needs fixing")
    assert.equal(creditNoBar(account(degraded, "siphorchannel-2")), "counted under another account")
    assert.equal(creditNoBar(account(degraded, "new-org")), "waiting for a reading")
    assert.equal(creditNoBar(account(degraded, "noor")), "no reading")
  })
})

describe("creditNotes", () => {
  test("a healthy account earns no line", () => {
    assert.deepEqual(creditNotes(account(golden, "siphorchannel")), [])
    assert.deepEqual(creditNotes(account(golden, "noor")), [])
  })

  test("an overspent account says by how much, in the server's figures", () => {
    assert.deepEqual(creditNotes(account(golden, "noorchasib")), [
      { text: "$36.40 spent past its $200.00 monthly credit.", tone: "bad" },
    ])
  })

  test("an aged reading adds its age to the server's sentence", () => {
    const stale = account(degraded, "siphorchannel")
    assert.deepEqual(creditNotes(stale), [
      {
        text: "This reading is out of date; Quota Cache has not refreshed it. These figures are from {age} ago.",
        tone: "warn",
        since: stale.observedAtEpoch!,
      },
    ])
    const [failed] = creditNotes(account(degraded, "chasibnoor"))
    assert.equal(failed?.tone, "warn")
    assert.ok(failed?.text.startsWith("The last read failed;"))
  })

  test("a rejected key, a misconfiguration and today not reported say what the server said", () => {
    const [rejected] = creditNotes(account(degraded, "noor"))
    assert.equal(rejected?.tone, "bad")
    assert.match(rejected!.text, /rejected this admin key/)
    // The server's sentence names what to set.
    assert.deepEqual(creditNotes(account(degraded, "agency-team")), [
      { text: 'renews must be a date like "2026-10-29".', tone: "warn" },
    ])
    const [today] = creditNotes(account(degraded, "noorchasib"))
    assert.equal(today?.tone, "quiet")
    assert.match(today!.text, /not reported today's spend/)
    const [duplicate] = creditNotes(account(degraded, "siphorchannel-2"))
    assert.equal(duplicate?.tone, "quiet")
  })

  test("a refused key on a counted reading is red, with the reading's age", () => {
    const failed = {
      ...account(degraded, "chasibnoor"),
      dataIssues: ["observeError", "keyRejected"],
      issue: "Anthropic rejected this admin key.",
    }
    assert.deepEqual(creditNotes(failed), [
      {
        text: "Anthropic rejected this admin key. These figures are from {age} ago.",
        tone: "bad",
        since: failed.observedAtEpoch!,
      },
    ])
  })
})

describe("poolNotes", () => {
  test("every account counted on a current reading needs no line", () => {
    assert.deepEqual(poolNotes(golden), [])
  })

  test("says how many it counts and which readings have aged", () => {
    // The duplicate is said apart: its organization is counted, under the
    // account listed first, so it is not one of the missing three.
    assert.deepEqual(poolNotes(degraded), [
      {
        text: "counts 3 of 7 accounts · noor, agency-team and new-org are left out · siphorchannel-2 is a duplicate, counted under another",
        tone: "bad",
      },
      { text: "2 accounts' figures are out of date", tone: "warn" },
    ])
  })

  test("one account left out is named, with why", () => {
    const one: APICredits = {
      ...golden,
      pool: { ...golden.pool, countedCount: 4, missingCount: 1 },
      accounts: golden.accounts.map((item) =>
        item.label === "noorchasib"
          ? { ...account(degraded, "noor"), id: item.id, label: "noorchasib" }
          : item,
      ),
    }
    assert.deepEqual(poolNotes(one), [
      { text: "counts 4 of 5 accounts · noorchasib is left out: its admin key was rejected", tone: "bad" },
    ])
  })

  test("only a duplicate is not a warning", () => {
    const twice: APICredits = {
      ...golden,
      pool: { ...golden.pool, countedCount: 4, duplicateCount: 1 },
      accounts: golden.accounts.map((item) =>
        item.label === "noorchasib" ? { ...account(degraded, "siphorchannel-2"), id: item.id } : item,
      ),
    }
    assert.deepEqual(poolNotes(twice), [
      { text: "counts 4 of 5 accounts · siphorchannel-2 is a duplicate, counted under another", tone: "quiet" },
    ])
  })

  test("an aged account whose key was refused says so, in red", () => {
    const failed = { ...account(degraded, "chasibnoor"), dataIssues: ["observeError", "keyRejected"] }
    const aged: APICredits = { ...golden, accounts: [failed, ...golden.accounts.slice(1)] }
    assert.deepEqual(poolNotes(aged), [
      {
        text: "chasibnoor's figures are from {age} ago; its admin key was rejected",
        tone: "bad",
        since: failed.observedAtEpoch!,
      },
    ])
  })

  test("among several aged accounts, the ones with a refused key are named, in red", () => {
    const refused = (label: string) => ({ ...account(degraded, "chasibnoor"), label, dataIssues: ["observeError", "keyRejected"] })
    const stale = account(degraded, "siphorchannel")
    const one: APICredits = { ...golden, accounts: [stale, refused("chasibnoor"), ...golden.accounts.slice(2)] }
    assert.deepEqual(poolNotes(one), [
      { text: "2 accounts' figures are out of date · chasibnoor needs attention: its admin key was rejected", tone: "bad" },
    ])
    const two: APICredits = { ...golden, accounts: [stale, refused("chasibnoor"), refused("noor"), ...golden.accounts.slice(3)] }
    assert.deepEqual(poolNotes(two), [
      { text: "3 accounts' figures are out of date · chasibnoor and noor need attention", tone: "bad" },
    ])
  })

  test("one aged account is named, with its age to fill in", () => {
    const stale = account(degraded, "siphorchannel")
    const aged: APICredits = { ...golden, accounts: [stale, ...golden.accounts.slice(1)] }
    assert.deepEqual(poolNotes(aged), [
      { text: "siphorchannel's figures are from {age} ago", tone: "warn", since: stale.observedAtEpoch! },
    ])
  })
})

describe("refillWhen", () => {
  const day = 86400
  const now = 1_789_012_800

  test("calendar days while a day or more is left, agreeing with the UTC date beside them", () => {
    assert.deepEqual(refillWhen(now + 6 * day + 3600, now - 20 * day, now), { kind: "days", days: 6 })
    // Sep 15 00:00 UTC from Sep 10 04:00 UTC is 4.8 days of elapsed time and
    // five dates on: "Sep 15 · in 5d".
    assert.deepEqual(refillWhen(1_789_430_400, now - 20 * day, now), { kind: "days", days: 5 })
    // The same date counts the same however late in today it is read.
    assert.deepEqual(refillWhen(1_789_430_400, now - 20 * day, now + 19 * 3600), { kind: "days", days: 5 })
  })

  test("the span itself inside the last day", () => {
    assert.deepEqual(refillWhen(now + 9 * 3600, now - 29 * day, now), { kind: "within", seconds: 9 * 3600 })
  })

  test("the first day of a cycle says it refilled today", () => {
    // agency-team in the golden document: its cycle began 4h before the build.
    const team = account(golden, "agency-team")
    assert.deepEqual(refillWhen(team.renewsAtEpoch, team.cycleStartEpoch, now), { kind: "refilledToday" })
  })

  test("no usable date, and a date passed before the next document", () => {
    assert.deepEqual(refillWhen(null, null, now), { kind: "none" })
    assert.deepEqual(refillWhen(now - 5, now - 30 * day, now), { kind: "due" })
  })
})

describe("names and freshness", () => {
  test("an item with no usable label goes by its id", () => {
    assert.equal(creditName({ id: "item-3", label: "" }), "item-3")
    assert.equal(creditName({ id: "label-ab", label: "noor" }), "noor")
  })

  test("updated is the newest counted reading, never an uncounted one", () => {
    assert.equal(lastRead(golden.accounts), 1_789_012_500)
    // The degraded duplicate was read at the build's last poll but is not counted.
    assert.equal(lastRead(degraded.accounts), 1_789_012_500)
    assert.equal(lastRead(degraded.accounts.filter((item) => !item.hasReading)), null)
  })
})
