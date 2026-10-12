// What the page says once the plugin has answered a press: the provider's
// outcome, and what became of CPA's cooldown on the credential after a reset.
//
// src/lib/outcome.ts imports nothing, so node loads it directly.

import assert from "node:assert/strict"
import { describe, test } from "node:test"

import { describeOutcome, fellShort, partlyApplied, type RedeemResult } from "../src/lib/outcome.ts"

function result(fields: Partial<RedeemResult>): RedeemResult {
  return {
    outcome: "reset",
    windowsReset: 2,
    remainingCount: 1,
    snapshotPending: true,
    refreshRequested: false,
    refreshRequestedAtEpoch: null,
    provider: "claude",
    replayed: false,
    cooldown: "",
    ...fields,
  }
}

const REMEDY = /use Clear cooldown on this credential in the CPA console\. Don’t press Use one again — that would spend another reset\./

describe("CPA's cooldown after a reset", () => {
  test("an answer from a plugin too old to clear one reads exactly as it always did", () => {
    const text = describeOutcome(result({}), "claude")
    assert.equal(text, "Reset applied. 1 banked reset left. The card updates at the next quota-cache poll.")
    assert.equal(partlyApplied(result({})), false)
  })

  test("cleared is a plain success", () => {
    const answer = result({ cooldown: "cleared" })
    assert.match(describeOutcome(answer, "claude"), /^Reset applied\. .* CPA’s cooldown on this credential was cleared, so it is back in use\.$/)
    assert.equal(partlyApplied(answer), false)
    assert.equal(fellShort(answer), false)
  })

  test("failed says the reset is applied, that CPA did not confirm the clear, and what to do instead of pressing", () => {
    const answer = result({ cooldown: "failed" })
    const text = describeOutcome(answer, "claude")
    assert.match(text, /^Reset applied\./)
    assert.match(text, /But CPA did not confirm that it cleared its cooldown on this credential\./)
    assert.match(text, REMEDY)
    assert.equal(partlyApplied(answer), true)
    assert.equal(fellShort(answer), false)
  })

  test("unsupported names the CPA version that can, and the console remedy that works without it", () => {
    const answer = result({ cooldown: "unsupported" })
    const text = describeOutcome(answer, "codex")
    assert.match(text, /^Reset applied\./)
    assert.match(text, /older than v8\.0\.12, does not let a plugin clear a credential’s cooldown/)
    assert.match(text, REMEDY)
    assert.equal(partlyApplied(answer), true)
  })

  test("unconfirmed does not claim the reset was applied, but does say it is spent", () => {
    const answer = result({ provider: "codex", cooldown: "unconfirmed", windowsReset: 0 })
    const text = describeOutcome(answer, "codex")
    assert.doesNotMatch(text, /Reset applied/)
    assert.match(text, /^Codex took the reset but did not confirm what it cleared/)
    assert.match(text, /1 banked reset left\. The card updates at the next quota-cache poll\./)
    assert.match(text, /The reset is spent: don’t press Use one again\./)
    assert.match(text, /use Clear cooldown on it in the CPA console\./)
    assert.equal(partlyApplied(answer), true)
  })

  test("a replayed answer keeps both halves, marked as the earlier press's", () => {
    const text = describeOutcome(result({ outcome: "alreadyUsed", replayed: true, cooldown: "cleared" }), "claude")
    assert.match(text, /^From your earlier press: Claude reports that reset as already used/)
    assert.match(text, /CPA’s cooldown on this credential was cleared, so it is back in use\.$/)
  })

  test("a state this page does not know is neither a warning nor a sentence", () => {
    const answer = result({ cooldown: "deferred" })
    assert.equal(partlyApplied(answer), false)
    assert.equal(describeOutcome(answer, "claude"), describeOutcome(result({}), "claude"))
  })

  test("outcomes that are not a reset say nothing about CPA", () => {
    for (const outcome of ["nothingToReset", "noCredit", "failed", "notLimited", "cooldown", "paused", "ineligible"]) {
      const answer = result({ outcome })
      assert.doesNotMatch(describeOutcome(answer, "claude"), /CPA’s cooldown|Clear cooldown/, outcome)
      assert.equal(partlyApplied(answer), false, outcome)
    }
  })
})

describe("after Quota Cache was asked to refresh", () => {
  const REFRESHING = "Quota Cache is checking this account now, then every other one. The card updates within a minute."

  test("a spent reset says the card updates within a minute, in place of the next poll", () => {
    const text = describeOutcome(result({ refreshRequested: true }), "claude")
    assert.equal(text, `Reset applied. 1 banked reset left. ${REFRESHING}`)
    assert.doesNotMatch(text, /next quota-cache poll/)
  })

  test("every outcome that spent says so, cooldown and all", () => {
    for (const answer of [
      result({ outcome: "nothingToReset", remainingCount: 0, refreshRequested: true }),
      result({ outcome: "alreadyUsed", cooldown: "cleared", refreshRequested: true }),
      result({ provider: "codex", cooldown: "unconfirmed", refreshRequested: true }),
      result({ cooldown: "failed", refreshRequested: true }),
    ]) {
      const text = describeOutcome(answer, answer.provider)
      assert.ok(text.includes(REFRESHING), text)
      assert.doesNotMatch(text, /next quota-cache poll/)
    }
  })

  test("without it, the sentence is the one it always was", () => {
    for (const fields of [{}, { cooldown: "cleared" }, { outcome: "nothingToReset" }] as Partial<RedeemResult>[]) {
      const text = describeOutcome(result(fields), "claude")
      assert.match(text, /The card updates at the next quota-cache poll\./)
      assert.ok(!text.includes(REFRESHING), text)
    }
  })

  // The earlier press may be minutes old, and Quota Cache may have polled the
  // account long since: "now" and "within a minute" would not be true of it.
  test("a replayed answer says it as what the earlier press came to, when it was asked", () => {
    const text = describeOutcome(result({ replayed: true, refreshRequested: true }), "claude")
    assert.equal(
      text,
      "From your earlier press: Reset applied. 1 banked reset left. Quota Cache was asked then to check this account, then every other one.",
    )
    assert.ok(!text.includes(REFRESHING), text)
  })
})
