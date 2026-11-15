// How the banked-resets card lays out an account's resets by kind.
//
// The card's one job here is that a Fable reset can never be taken for a
// full one: each kind its own group, a button only where the kind is one a
// press can name, and nothing offered on a Claude account whose kinds are not
// known. These pin the layout those rules come from.
//
// src/lib/resetKinds.ts has only type imports, so node loads it directly.

import assert from "node:assert/strict"
import { describe, test } from "node:test"

import {
  anyRedeemable,
  groupOf,
  isSpendableKind,
  kindEntry,
  kindRank,
  kindsOf,
  layoutResets,
  othersHeld,
  SPENDABLE_KINDS,
} from "../src/lib/resetKinds.ts"
import type { Credential, ResetCredits, ResetKindCredits } from "../src/lib/types.ts"

function kind(name: string, fields: Partial<ResetKindCredits> = {}): ResetKindCredits {
  return {
    kind: name,
    availableCount: 1,
    expiresAtEpoch: 2000,
    expiresInSeconds: 1000,
    redeemable: true,
    hold: "",
    holdUntilEpoch: null,
    ...fields,
  }
}

function credits(kinds: ResetKindCredits[] | undefined, fields: Partial<ResetCredits> = {}): ResetCredits {
  // Tolerant of the malformed entries some tests hand in on purpose.
  const total = (kinds ?? []).reduce((sum, one) => sum + (typeof one?.availableCount === "number" ? one.availableCount : 0), 0)
  const base: ResetCredits = {
    availableCount: total || 1,
    expiresAtEpoch: 2000,
    expiresInSeconds: 1000,
    redeemable: false,
    hold: "",
    holdUntilEpoch: null,
    ...fields,
  }
  return kinds === undefined ? base : { ...base, kinds }
}

function credential(id: string, provider: string, resetCredits: ResetCredits | null): Credential {
  return {
    id,
    email: `${id}@example.com`,
    provider,
    plan: "",
    status: "ok",
    lastObservedEpoch: 0,
    activity: null,
    resetCredits,
    renewalAtEpoch: null,
    credits: null,
  }
}

describe("the kinds", () => {
  test("a press can name four, in Quota Cache's order", () => {
    assert.deepEqual([...SPENDABLE_KINDS], ["full", "fable", "weekly", "session"])
    for (const name of SPENDABLE_KINDS) assert.equal(isSpendableKind(name), true)
    for (const name of ["unknown", "monthly", "", "Full"]) assert.equal(isSpendableKind(name), false, name)
  })

  test("rank as ResetKindRank does: full, Fable, weekly, 5-hour, then anything else together", () => {
    const order = ["full", "fable", "weekly", "session", "unknown"]
    for (let i = 1; i < order.length; i++) assert.ok(kindRank(order[i - 1]!) < kindRank(order[i]!), order[i])
    assert.equal(kindRank("monthly"), kindRank("unknown"))
  })

  test("anything unrecognised is shown as unknown", () => {
    assert.equal(groupOf("fable"), "fable")
    assert.equal(groupOf("monthly"), "unknown")
    assert.equal(groupOf(""), "unknown")
  })
})

describe("kindsOf: reading the document defensively", () => {
  test("a missing or malformed list is none", () => {
    assert.deepEqual(kindsOf(null), [])
    assert.deepEqual(kindsOf(credits(undefined)), [])
    assert.deepEqual(kindsOf({ ...credits(undefined), kinds: "full" } as unknown as ResetCredits), [])
  })

  test("entries that are not entries, or hold none, are dropped", () => {
    const raw = [null, 3, { availableCount: 2 }, kind("full", { availableCount: 0 }), kind("fable")] as unknown as ResetKindCredits[]
    assert.deepEqual(
      kindsOf(credits(raw)).map((entry) => entry.kind),
      ["fable"],
    )
  })

  test("sorted by rank whatever order they came in", () => {
    const got = kindsOf(credits([kind("unknown"), kind("session"), kind("fable"), kind("full"), kind("weekly")]))
    assert.deepEqual(
      got.map((entry) => entry.kind),
      ["full", "fable", "weekly", "session", "unknown"],
    )
  })

  test("an unknown kind is never redeemable, whatever the document says", () => {
    const [entry] = kindsOf(credits([kind("unknown", { redeemable: true })]))
    assert.equal(entry!.redeemable, false)
  })

  test("kinds newer than the page merge into one unknown entry, counts summed", () => {
    const got = kindsOf(
      credits([kind("monthly", { availableCount: 2, expiresAtEpoch: 5000 }), kind("unknown", { expiresAtEpoch: 3000 })]),
    )
    assert.equal(got.length, 1)
    assert.equal(got[0]!.kind, "unknown")
    assert.equal(got[0]!.availableCount, 3)
    assert.equal(got[0]!.expiresAtEpoch, 3000)
    assert.equal(got[0]!.redeemable, false)
  })

  test("a kind listed twice is one entry: summed, sooner deadline, held only if both are", () => {
    const got = kindsOf(
      credits([
        kind("full", { expiresAtEpoch: 4000, hold: "cooldown", holdUntilEpoch: 1500 }),
        kind("full", { availableCount: 2, expiresAtEpoch: 3000, hold: "" }),
      ]),
    )
    assert.deepEqual(got, [
      { ...kind("full"), availableCount: 3, expiresAtEpoch: 3000, hold: "", holdUntilEpoch: null },
    ])
  })
})

describe("layoutResets", () => {
  const charlie = credential("charlie", "claude", credits([kind("full"), kind("fable", { availableCount: 2 })]))
  const sierra = credential("sierra", "claude", credits([kind("fable")]))
  const noor = credential("noor", "claude", credits([kind("unknown", { redeemable: false })]))
  const older = credential("older", "claude", credits([]))
  const absent = credential("absent", "claude", credits(undefined, { redeemable: true }))
  const codex = credential("codex", "codex", credits([], { redeemable: true }))
  const codexOld = credential("codex-old", "codex", credits(undefined, { redeemable: true }))

  test("one group per kind held, in rank order, each account in every group it holds", () => {
    const layout = layoutResets([sierra, noor, charlie])
    assert.deepEqual(
      layout.groups.map((group) => [group.kind, group.rows.map((row) => row.credential.id)]),
      [
        ["full", ["charlie"]],
        ["fable", ["sierra", "charlie"]],
        ["unknown", ["noor"]],
      ],
    )
  })

  test("each row carries its own kind's figures, never the account's total", () => {
    const fable = layoutResets([charlie]).groups.find((group) => group.kind === "fable")!
    assert.equal(fable.rows[0]!.credits.availableCount, 2)
    assert.equal(fable.rows[0]!.credits.kind, "fable")
  })

  test("never two groups for one kind", () => {
    const layout = layoutResets([charlie, sierra, credential("x", "claude", credits([kind("fable"), kind("fable")]))])
    const kinds = layout.groups.map((group) => group.kind)
    assert.deepEqual(kinds, [...new Set(kinds)])
  })

  test("a Claude account whose kinds are not reported is set apart, not guessed full", () => {
    const layout = layoutResets([older, absent, charlie])
    assert.deepEqual(
      layout.unsorted.map((one) => one.id),
      ["older", "absent"],
    )
    assert.deepEqual(layout.plain, [])
    assert.ok(layout.groups.every((group) => group.rows.every((row) => row.credential.id === "charlie")))
  })

  test("Codex stays as it was: one plain row each, with or without the empty list", () => {
    const layout = layoutResets([codex, codexOld])
    assert.deepEqual(
      layout.plain.map((one) => one.id),
      ["codex", "codex-old"],
    )
    assert.deepEqual(layout.groups, [])
    assert.deepEqual(layout.unsorted, [])
  })

  test("an account holding none is not laid out", () => {
    const layout = layoutResets([credential("none", "claude", null)])
    assert.deepEqual(layout, { plain: [], groups: [], unsorted: [] })
  })
})

describe("what the card and the dialog ask of an account", () => {
  const charlie = credential(
    "charlie",
    "claude",
    credits([kind("full"), kind("fable", { availableCount: 2 }), kind("unknown", { redeemable: false })]),
  )

  test("the other kinds it holds, which a press of one leaves banked", () => {
    assert.deepEqual(othersHeld(charlie.resetCredits, "fable"), [
      { kind: "full", count: 1 },
      { kind: "unknown", count: 1 },
    ])
    assert.deepEqual(othersHeld(credential("x", "claude", credits([kind("full")])).resetCredits, "full"), [])
  })

  test("its entry for one kind, or none", () => {
    assert.equal(kindEntry(charlie, "fable")!.availableCount, 2)
    assert.equal(kindEntry(charlie, "weekly"), null)
  })

  test("whether anything would have a button: a spendable kind, or a Codex row", () => {
    assert.equal(anyRedeemable([charlie]), true)
    assert.equal(anyRedeemable([credential("u", "claude", credits([kind("unknown", { redeemable: true })]))]), false)
    assert.equal(anyRedeemable([credential("f", "claude", credits([kind("fable", { redeemable: false })]))]), false)
    // Claude's top level is never what offers a button, even from a document
    // that says it is redeemable.
    assert.equal(anyRedeemable([credential("o", "claude", credits(undefined, { redeemable: true }))]), false)
    assert.equal(anyRedeemable([credential("c", "codex", credits([], { redeemable: true }))]), true)
  })
})
