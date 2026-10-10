// What a card says when its pool counts an account as less than the account's
// own reading: the chip on the account's row, and the fold's count of who the
// mean leaves out.
//
// src/lib/pool.ts imports nothing but types, which node strips, so node loads
// it directly and these tests call the same weeklyNote and foldNotes the cards
// call. In each case the fields those two read are as the server would send
// them, including from a plugin that predates the new ones, which must read
// exactly as it always did. Fields neither reads keep entry()'s defaults.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { describe, test } from "node:test"

import { cappedByWeekly, foldFlags, foldNotes, weeklyNote } from "../src/lib/pool.ts"
import type { RowEntry } from "../src/lib/types.ts"

/** An entry as an older plugin sends it: none of heldOut, pooledFraction, pooledPercent. */
function entry(fields: Partial<RowEntry> = {}): RowEntry {
  return {
    credentialId: "noor",
    hasReading: true,
    remainingFraction: 1,
    remainingPercent: 100,
    level: "ok",
    poolShare: 0.25,
    recoveryShare: 0,
    resetsNext: false,
    resetAtEpoch: 1_800_000_000,
    resetInSeconds: 3600,
    resetDisplayHint: "countdown",
    observedAtEpoch: 1_799_996_400,
    nextAttemptEpoch: 1_799_996_700,
    sourceWindowKey: "session",
    sourceModel: null,
    dataIssues: [],
    state: "ok",
    ...fields,
  }
}

describe("weeklyNote", () => {
  test("an older plugin's entry has no note", () => {
    assert.equal(weeklyNote(entry()), null)
    assert.equal(weeklyNote(entry({ remainingFraction: 0.1, remainingPercent: 10, level: "critical" })), null)
  })

  test("an older plugin's entry with no reading has no note", () => {
    assert.equal(
      weeklyNote(entry({ hasReading: false, sourceWindowKey: "", remainingFraction: 0, remainingPercent: 0, level: "" })),
      null,
    )
  })

  test("a session held out of the mean says its weekly is spent, whatever its own figure", () => {
    // The case the change is for: a session at 100% the weekly will not let
    // anything use. The server sends its own reading as its pooled value.
    const held = entry({ heldOut: true, pooledFraction: 1, pooledPercent: 100, poolShare: 0 })
    assert.deepEqual(weeklyNote(held), { text: "weekly spent", spent: true })
    // Held out on a degraded reading too: the gate is the weekly figure, not
    // the session's state.
    const stale = entry({
      heldOut: true,
      remainingFraction: 0.4,
      remainingPercent: 40,
      level: "low",
      pooledFraction: 0.4,
      pooledPercent: 40,
      poolShare: 0,
      dataIssues: ["stale"],
      state: "stale",
    })
    assert.deepEqual(weeklyNote(stale), { text: "weekly spent", spent: true })
  })

  test("a session counted at its own reading has no note", () => {
    assert.equal(weeklyNote(entry({ heldOut: false, pooledFraction: 1, pooledPercent: 100 })), null)
    const empty = entry({
      heldOut: false,
      remainingFraction: 0,
      remainingPercent: 0,
      level: "critical",
      pooledFraction: 0,
      pooledPercent: 0,
      poolShare: 0,
    })
    assert.equal(weeklyNote(empty), null)
  })

  test("Fable capped by a smaller weekly says what it caps at", () => {
    // claude-chasibnoor in the golden fixture: Fable 45% left, weekly 24%.
    const capped = entry({
      sourceWindowKey: "weekly_fable",
      remainingFraction: 0.45,
      remainingPercent: 45,
      heldOut: false,
      pooledFraction: 0.24,
      pooledPercent: 24,
    })
    assert.deepEqual(weeklyNote(capped), { text: "weekly caps at 24%", spent: false })
  })

  test("Fable capped to nothing says the weekly is spent", () => {
    const spent = entry({
      sourceWindowKey: "weekly_fable",
      remainingFraction: 0.7,
      remainingPercent: 70,
      heldOut: false,
      pooledFraction: 0,
      pooledPercent: 0,
    })
    assert.deepEqual(weeklyNote(spent), { text: "weekly spent", spent: true })
  })

  test("Fable under its weekly, or a row the weekly does not gate, has no note", () => {
    // claude-siphorchannel: Fable already spent, weekly 76% left. The lesser
    // is Fable's own, so the pool counts it at its own figure.
    assert.equal(
      weeklyNote(
        entry({
          sourceWindowKey: "weekly_fable",
          remainingFraction: 0,
          remainingPercent: 0,
          pooledFraction: 0,
          pooledPercent: 0,
        }),
      ),
      null,
    )
    assert.equal(
      weeklyNote(
        entry({
          sourceWindowKey: "weekly_fable",
          remainingFraction: 0.88,
          remainingPercent: 88,
          pooledFraction: 0.88,
          pooledPercent: 88,
        }),
      ),
      null,
    )
  })

  test("a reading with no figure at all has no note", () => {
    const none = entry({
      hasReading: false,
      sourceWindowKey: "",
      remainingFraction: 0,
      remainingPercent: 0,
      level: "",
      heldOut: false,
      pooledFraction: 0,
      pooledPercent: 0,
    })
    assert.equal(weeklyNote(none), null)
  })
})

describe("foldNotes", () => {
  test("an older plugin's exclusions are all missing readings", () => {
    assert.deepEqual(foldNotes({ excludedCount: 2 }), ["2 without a reading"])
    assert.deepEqual(foldNotes({ excludedCount: 0 }), [])
  })

  test("a mean over every account says nothing", () => {
    assert.deepEqual(foldNotes({ excludedCount: 0, heldOutCount: 0 }), [])
  })

  test("held-out accounts are named apart from those with no reading", () => {
    assert.deepEqual(foldNotes({ excludedCount: 1, heldOutCount: 1 }), ["1 weekly spent"])
    assert.deepEqual(foldNotes({ excludedCount: 3, heldOutCount: 2 }), ["2 weekly spent", "1 without a reading"])
    assert.deepEqual(foldNotes({ excludedCount: 2, heldOutCount: 0 }), ["2 without a reading"])
  })

  test("every account held out still counts them, not a missing reading", () => {
    // The user's "0% session": five accounts reporting, every weekly spent.
    assert.deepEqual(foldNotes({ excludedCount: 5, heldOutCount: 5 }), ["5 weekly spent"])
  })
})

describe("cappedByWeekly", () => {
  test("counts the Fable accounts the pool counts at their weekly", () => {
    // The golden Fable row: chasibnoor 45% capped at 24%, noor 70% at 60%.
    const fable = [
      entry({ remainingPercent: 0, pooledPercent: 0 }),
      entry({ remainingPercent: 88, pooledPercent: 88 }),
      entry({ remainingPercent: 45, pooledPercent: 24 }),
      entry({ remainingPercent: 70, pooledPercent: 60 }),
      entry({ remainingPercent: 10, pooledPercent: 10 }),
    ]
    assert.equal(cappedByWeekly(fable), 2)
  })

  test("a held-out session is not a cap, and an older plugin caps nothing", () => {
    assert.equal(cappedByWeekly([entry({ heldOut: true, pooledPercent: 100 })]), 0)
    assert.equal(cappedByWeekly([entry(), entry({ remainingPercent: 30 })]), 0)
  })
})

describe("foldFlags", () => {
  const names = ["siphorchannel", "agency", "chasibnoor"]

  test("a card whose accounts are all fine names nobody", () => {
    assert.deepEqual(foldFlags([entry(), entry(), entry()], names), [])
  })

  test("names low and out accounts with the level in words", () => {
    const flags = foldFlags(
      [
        entry({ credentialId: "a", remainingPercent: 0, level: "critical" }),
        entry({ credentialId: "b", remainingPercent: 76 }),
        entry({ credentialId: "c", remainingPercent: 24, level: "low" }),
      ],
      names,
    )
    assert.deepEqual(flags, [
      { id: "a", name: "siphorchannel", figure: "0%", word: "out", tone: "critical" },
      { id: "c", name: "chasibnoor", figure: "24%", word: "low", tone: "low" },
    ])
  })

  test("critical above zero says critical, not out", () => {
    const [flag] = foldFlags([entry({ remainingPercent: 10, level: "critical" })], ["noorchasib"])
    assert.equal(flag?.word, "critical")
    assert.equal(flag?.figure, "10%")
  })

  test("a reading the server no longer vouches for is named, with its state", () => {
    assert.deepEqual(foldFlags([entry({ credentialId: "s", state: "stale" })], ["stale"]), [
      { id: "s", name: "stale", figure: "", word: "stale", tone: "" },
    ])
    const [failing] = foldFlags([entry({ remainingPercent: 5, level: "critical", state: "error" })], ["failing"])
    assert.equal(failing?.word, "critical · failed")
  })

  test("an account with no reading is left to the fold's count", () => {
    const none = entry({ hasReading: false, remainingPercent: 0, level: "", state: "pending" })
    assert.deepEqual(foldFlags([none], ["pending"]), [])
  })

  test("falls back to the id when a name is missing", () => {
    const [flag] = foldFlags([entry({ credentialId: "x", remainingPercent: 20, level: "low" })], [])
    assert.equal(flag?.name, "x")
  })
})
