// What the page says once the plugin has answered a press: the provider's
// outcome, and what became of CPA's cooldown on the credential after a reset —
// and, on Claude, which kind of reset each sentence is about.
//
// src/lib/outcome.ts imports nothing, so node loads it directly.

import assert from "node:assert/strict"
import { describe, test } from "node:test"

import {
  CLAUDE_RULE,
  countOf,
  describeOutcome,
  fellShort,
  KIND_REFUSALS,
  KIND_WORDS,
  kindRefusal,
  kindWords,
  KINDS_UNREPORTED,
  othersSentence,
  partlyApplied,
  pressName,
  type RedeemResult,
  spendsSentence,
  UNRECOGNISED_WORDS,
} from "../src/lib/outcome.ts"
import { SPENDABLE_KINDS } from "../src/lib/resetKinds.ts"

function result(fields: Partial<RedeemResult>): RedeemResult {
  return {
    outcome: "reset",
    windowsReset: 2,
    remainingCount: 1,
    snapshotPending: true,
    refreshRequested: false,
    refreshRequestedAtEpoch: null,
    provider: "claude",
    kind: null,
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

describe("a Claude outcome names the kind it spent", () => {
  test("a full reset, in Claude Code's own done line, counted by kind", () => {
    assert.equal(
      describeOutcome(result({ kind: "full", snapshotPending: false }), "claude"),
      "Full reset used — limits reset. 1 full reset left.",
    )
  })

  test("a Fable reset says what it gave back, and never that it reset the limits", () => {
    const text = describeOutcome(result({ kind: "fable", remainingCount: 0, snapshotPending: false }), "claude")
    assert.equal(text, "Fable reset used — what Fable used is back in the weekly limit. No Fable resets left.")
    assert.doesNotMatch(text, /limits reset|5-hour/)
  })

  test("a weekly and a 5-hour reset, each in its own words", () => {
    assert.equal(
      describeOutcome(result({ kind: "weekly", remainingCount: 2, snapshotPending: false }), "claude"),
      "Weekly reset used — weekly limits reset. 2 weekly resets left.",
    )
    assert.equal(
      describeOutcome(result({ kind: "session", snapshotPending: false }), "claude"),
      "5-hour reset used — 5-hour limit reset. 1 5-hour reset left.",
    )
  })

  test("every outcome names the kind, and none names another", () => {
    for (const outcome of ["reset", "nothingToReset", "noCredit", "alreadyUsed", "notLimited", "paused", "failed"]) {
      const text = describeOutcome(result({ outcome, kind: "fable" }), "claude")
      assert.match(text, /Fable reset/, outcome)
      assert.doesNotMatch(text, /full reset|weekly reset|5-hour reset/, outcome)
    }
  })

  test("a refusal under Claude's rule says it per kind", () => {
    assert.equal(
      describeOutcome(result({ outcome: "notLimited", kind: "weekly" }), "claude"),
      "Claude allows a weekly reset only once the account has reached a limit it refills, and this one has not. Nothing was spent.",
    )
  })

  test("the remedy after a spend points at the kind's own button, not at Use one", () => {
    const text = describeOutcome(result({ kind: "fable", cooldown: "failed" }), "claude")
    assert.match(text, /Don’t press “Use Fable reset” again — that would spend another Fable reset\./)
    assert.doesNotMatch(text, /Use one/)
    const unconfirmed = describeOutcome(result({ kind: "full", cooldown: "unconfirmed" }), "claude")
    assert.match(unconfirmed, /^Claude took the full reset but did not confirm what it cleared/)
    assert.match(unconfirmed, /don’t press “Use full reset” again/)
  })

  test("Codex, and a plugin too old to name a kind, read exactly as before", () => {
    const codex = result({ provider: "codex", kind: null, snapshotPending: false })
    assert.equal(describeOutcome(codex, "codex"), "Reset applied. 1 banked reset left.")
    assert.match(describeOutcome(result({ provider: "codex", cooldown: "failed" }), "codex"), REMEDY)
    assert.equal(describeOutcome(result({ kind: null, snapshotPending: false }), "claude"), "Reset applied. 1 banked reset left.")
  })

  test("a kind the page cannot name is not guessed at", () => {
    for (const kind of ["unknown", "monthly", ""]) {
      assert.equal(
        describeOutcome(result({ kind, snapshotPending: false }), "claude"),
        "Reset applied. 1 banked reset left.",
        kind,
      )
    }
  })
})

describe("the plugin's refusals about a kind", () => {
  const clock = (epoch: number) => `@${epoch}`
  const detail = (fields: Partial<Parameters<typeof kindRefusal>[1]> = {}) => ({
    kind: "full",
    pendingKind: null,
    retryUntilEpoch: null,
    nowSeconds: 1000,
    clock,
    ...fields,
  })

  test("kind_required and invalid_kind each say nothing was spent", () => {
    for (const code of ["kind_required", "invalid_kind"]) {
      const text = kindRefusal(code, detail())
      assert.ok(text, code)
      assert.match(text, /Nothing was spent\./, code)
    }
  })

  test("other_kind_unresolved names the earlier kind, its button, the kind pressed, and the deadline", () => {
    assert.equal(
      kindRefusal("other_kind_unresolved", detail({ pendingKind: "fable", retryUntilEpoch: 1600 })),
      "An earlier Fable reset on this account has an unknown outcome. Press “Use Fable reset” again to settle it before using a full reset, or wait until @1600. Nothing was spent.",
    )
  })

  test("the other way round, too", () => {
    assert.equal(
      kindRefusal("other_kind_unresolved", detail({ kind: "fable", pendingKind: "full", retryUntilEpoch: 1600 })),
      "An earlier full reset on this account has an unknown outcome. Press “Use full reset” again to settle it before using a Fable reset, or wait until @1600. Nothing was spent.",
    )
  })

  test("a deadline already past, or none, is not offered as a wait", () => {
    for (const retryUntilEpoch of [null, 900]) {
      const text = kindRefusal("other_kind_unresolved", detail({ pendingKind: "fable", retryUntilEpoch }))!
      assert.doesNotMatch(text, /wait until/)
      assert.match(text, /Press “Use Fable reset” again to settle it before using a full reset\. Nothing was spent\.$/)
    }
  })

  test("an earlier kind this page cannot name is not given a button", () => {
    const text = kindRefusal("other_kind_unresolved", detail({ pendingKind: "monthly", retryUntilEpoch: 1600 }))!
    assert.doesNotMatch(text, /Press “/)
    assert.match(text, /^An earlier reset of another kind on this account has an unknown outcome\./)
  })

  test("any other code is not this function's", () => {
    for (const code of ["not_redeemable", "outcome_unknown", ""]) assert.equal(kindRefusal(code, detail()), null)
  })

  // KIND_REFUSALS is what redeem.ts mayHaveSpent reads to give a notice the
  // refusal's tone rather than the unknown's. other_kind_unresolved among
  // them: the notice is tagged with the kind just pressed, and an amber
  // "may be spent" on that tag would say the full reset may be gone when it
  // is the Fable one that is unknown.
  test("every kind refusal spent nothing, and is toned as a refusal", () => {
    assert.deepEqual([...KIND_REFUSALS].sort(), ["invalid_kind", "kind_required", "other_kind_unresolved"])
    for (const code of KIND_REFUSALS) {
      for (const pendingKind of [null, "fable"]) {
        const text = kindRefusal(code, detail({ pendingKind, retryUntilEpoch: 1600 }))
        assert.ok(text, code)
        assert.match(text, /Nothing was spent\./, code)
      }
    }
  })
})

describe("the words for each kind", () => {
  test("every kind a press can name has words, and nothing else does", () => {
    assert.deepEqual(Object.keys(KIND_WORDS).sort(), [...SPENDABLE_KINDS].sort())
    for (const kind of SPENDABLE_KINDS) {
      const words = kindWords(kind)
      assert.notEqual(words, UNRECOGNISED_WORDS, kind)
      assert.match(words.button, /^Use .+ reset$/, kind)
      assert.ok(words.heading.endsWith("resets"), kind)
    }
    assert.equal(kindWords("unknown"), UNRECOGNISED_WORDS)
    assert.equal(kindWords("monthly"), UNRECOGNISED_WORDS)
    assert.equal(UNRECOGNISED_WORDS.button, "")
  })

  test("the buttons and headings are the ones the card uses", () => {
    assert.deepEqual(
      SPENDABLE_KINDS.map((kind) => [kindWords(kind).button, kindWords(kind).heading]),
      [
        ["Use full reset", "Full resets"],
        ["Use Fable reset", "Fable resets"],
        ["Use weekly reset", "Weekly resets"],
        ["Use 5-hour reset", "5-hour resets"],
      ],
    )
    assert.equal(UNRECOGNISED_WORDS.heading, "Unrecognised resets")
    assert.match(UNRECOGNISED_WORDS.effect, /\/limit-reset/)
  })

  test("Fable's effect says it is not a full reset, and claims nothing about the 5-hour limit", () => {
    const { effect } = kindWords("fable")
    assert.equal(effect, "Gives back to the weekly limit what Fable used this week. Not a full reset.")
    assert.doesNotMatch(effect, /5-hour/)
  })

  test("the rule and the note for a Quota Cache older than the kinds", () => {
    assert.match(CLAUDE_RULE, /reached a limit it refills/)
    assert.match(KINDS_UNREPORTED, /^Update Quota Cache/)
    assert.match(KINDS_UNREPORTED, /won’t spend one until it can tell full and Fable resets apart/)
  })

  test("a press is named by its kind's button, and Codex's by Use one", () => {
    assert.equal(pressName("fable"), "“Use Fable reset”")
    assert.equal(pressName(""), "Use one")
    assert.equal(pressName(null), "Use one")
    assert.equal(pressName("unknown"), "Use one")
  })

  test("counts, with none said as none", () => {
    assert.equal(countOf(1, "full"), "1 full reset")
    assert.equal(countOf(2, "fable"), "2 Fable resets")
    assert.equal(countOf(0, "fable"), "No Fable resets")
    assert.equal(countOf(1, "unknown"), "1 unrecognised reset")
  })
})

describe("the dialog's sentences", () => {
  test("it spends one of the kind pressed, from the account named", () => {
    assert.equal(spendsSentence("fable", 1, "a@example.com"), "This spends the only Fable reset that a@example.com holds, straight away.")
    assert.equal(spendsSentence("full", 3, "a@example.com"), "This spends one of the 3 full resets that a@example.com holds, straight away.")
  })

  test("what else the account holds stays banked", () => {
    assert.equal(othersSentence([{ kind: "full", count: 1 }]), "Its 1 full reset stays banked — this is not one of them.")
    assert.equal(othersSentence([{ kind: "full", count: 2 }]), "Its 2 full resets stay banked — this is not one of them.")
    assert.equal(
      othersSentence([
        { kind: "full", count: 1 },
        { kind: "weekly", count: 1 },
        { kind: "unknown", count: 2 },
      ]),
      "Its 1 full reset, 1 weekly reset and 2 unrecognised resets stay banked — this is not one of them.",
    )
  })

  test("nothing at all when it holds no other kind", () => {
    assert.equal(othersSentence([]), "")
    assert.equal(othersSentence([{ kind: "full", count: 0 }]), "")
  })
})
