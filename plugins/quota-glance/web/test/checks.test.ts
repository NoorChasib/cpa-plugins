// After a press the plugin answered with refreshRequested: the account's rows
// say "Checking the new limits…" and the summary is read every five seconds,
// until the summary carries an observation of the account from the instant
// Quota Cache was asked, by CPA's clock, or for three minutes by this
// browser's, whichever comes first.
//
// src/lib/checks.ts imports nothing, so node loads it directly. Its list of
// checks is one per page and carries from one test to the next, so each test
// that starts one uses an account and an instant of its own.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { describe, test } from "node:test"

import {
  anyChecking,
  CHECK_POLL_MS,
  CHECK_SECONDS,
  CHECKING_LABEL,
  type Check,
  checkedInterval,
  getChecks,
  isChecking,
  startCheck,
  subscribe,
} from "../src/lib/checks.ts"

/** When Quota Cache was asked, by CPA's clock. */
const ASKED = 1_800_000_000
/** When the page started the check, by this browser's clock: here, the same. */
const STARTED = ASKED
const MINUTE_POLL = 60_000

/** One account as the summary lists it, last observed at `observedAt`, by CPA's clock. */
function credential(id: string, observedAt: number) {
  return { id, lastObservedEpoch: observedAt }
}

describe("the pill", () => {
  const checks = [{ id: "codex-a", since: ASKED, startedAt: STARTED }]

  test("is worded as the page says it", () => {
    assert.equal(CHECKING_LABEL, "Checking the new limits…")
  })

  test("shows on the account pressed, from the start on, while its last observation is from before", () => {
    const before = credential("codex-a", ASKED - 300)
    assert.equal(isChecking(checks, before, STARTED), true)
    assert.equal(isChecking(checks, before, STARTED + 90), true)
    // Another account, with the same old reading, is not being checked.
    assert.equal(isChecking(checks, credential("codex-b", ASKED - 300), STARTED + 5), false)
    assert.equal(isChecking(checks, undefined, STARTED + 5), false)
  })

  test("goes at the first observation from the second Quota Cache was asked in", () => {
    assert.equal(isChecking(checks, credential("codex-a", ASKED - 1), STARTED + 4), true)
    // The poll asked for can begin in that same second.
    assert.equal(isChecking(checks, credential("codex-a", ASKED), STARTED + 4), false)
    assert.equal(isChecking(checks, credential("codex-a", ASKED + 6), STARTED + 4), false)
  })

  test("goes after three minutes even when no new observation came", () => {
    const stuck = credential("codex-a", ASKED - 300)
    assert.equal(CHECK_SECONDS, 180)
    assert.equal(isChecking(checks, stuck, STARTED + CHECK_SECONDS - 1), true)
    assert.equal(isChecking(checks, stuck, STARTED + CHECK_SECONDS), false)
    assert.equal(isChecking(checks, stuck, STARTED + 3600), false)
  })
})

// The browser's clock and CPA's are different machines' clocks as often as
// not: a remote CPA, the menu bar's popover, a VM that drifted while asleep.
// Neither the press nor anything else on this browser's clock is compared with
// an observation, so how far apart they are changes nothing but where the
// three minutes fall.
describe("a browser clock off CPA's", () => {
  for (const [label, skew] of [
    ["30 seconds ahead", 30],
    ["two minutes behind", -120],
  ] as const) {
    // The browser starts the check two seconds after Quota Cache was asked,
    // reading its own clock.
    const started = ASKED + 2 + skew
    const checks = [{ id: "codex-a", since: ASKED, startedAt: started }]

    test(`${label}: an observation from before the request still shows the pill`, () => {
      // Quota Cache's last scheduled poll, 40 seconds before the request.
      assert.equal(isChecking(checks, credential("codex-a", ASKED - 40), started), true)
      assert.equal(isChecking(checks, credential("codex-a", ASKED - 40), started + 60), true)
    })

    test(`${label}: the poll asked for, twelve seconds on, ends it`, () => {
      assert.equal(isChecking(checks, credential("codex-a", ASKED + 12), started + 12), false)
    })

    test(`${label}: the three minutes run from the start, by this browser's clock`, () => {
      const stuck = credential("codex-a", ASKED - 40)
      assert.equal(isChecking(checks, stuck, started + CHECK_SECONDS - 1), true)
      assert.equal(isChecking(checks, stuck, started + CHECK_SECONDS), false)
    })
  }
})

// A press whose answer was lost, pressed again minutes later, is answered with
// the first copy's answer, and that names the first copy's request. Quota
// Cache acted on it long ago, so the figures on screen are already the new
// ones and there is nothing to wait for.
describe("a replayed answer", () => {
  test("ends at once when the poll it asked for has already landed", () => {
    const repeatedAt = STARTED + 120
    const checks = [{ id: "codex-a", since: ASKED, startedAt: repeatedAt }]
    assert.equal(isChecking(checks, credential("codex-a", ASKED + 9), repeatedAt), false)
    assert.equal(checkedInterval(MINUTE_POLL, anyChecking(checks, [credential("codex-a", ASKED + 9)], repeatedAt)), MINUTE_POLL)
  })

  test("still waits when that poll brought no observation", () => {
    const repeatedAt = STARTED + 120
    const checks = [{ id: "codex-a", since: ASKED, startedAt: repeatedAt }]
    assert.equal(isChecking(checks, credential("codex-a", ASKED - 300), repeatedAt + 60), true)
  })
})

describe("the summary's interval", () => {
  const interval = (checks: Check[], credentials: ReturnType<typeof credential>[] | undefined, now: number) =>
    checkedInterval(MINUTE_POLL, anyChecking(checks, credentials, now))

  test("is the usual minute with nothing to check", () => {
    assert.equal(interval([], [credential("codex-a", ASKED - 300)], STARTED), MINUTE_POLL)
  })

  test("is five seconds from the start until the account's new figures arrive", () => {
    const checks = [{ id: "codex-a", since: ASKED, startedAt: STARTED }]
    assert.equal(CHECK_POLL_MS, 5_000)
    assert.equal(interval(checks, [credential("codex-a", ASKED - 300)], STARTED), CHECK_POLL_MS)
    assert.equal(interval(checks, [credential("codex-a", ASKED - 300)], STARTED + 55), CHECK_POLL_MS)
    assert.equal(interval(checks, [credential("codex-a", ASKED + 12)], STARTED + 15), MINUTE_POLL)
  })

  test("returns to the minute after three minutes, new figures or not", () => {
    const checks = [{ id: "codex-a", since: ASKED, startedAt: STARTED }]
    const stuck = [credential("codex-a", ASKED - 300)]
    assert.equal(interval(checks, stuck, STARTED + CHECK_SECONDS - 1), CHECK_POLL_MS)
    assert.equal(interval(checks, stuck, STARTED + CHECK_SECONDS), MINUTE_POLL)
  })

  test("waits on every account still being checked, and on one the summary does not list yet", () => {
    const checks = [
      { id: "codex-a", since: ASKED, startedAt: STARTED },
      { id: "claude-b", since: ASKED + 20, startedAt: STARTED + 20 },
    ]
    const aDone = [credential("codex-a", ASKED + 8), credential("claude-b", ASKED - 600)]
    assert.equal(interval(checks, aDone, STARTED + 30), CHECK_POLL_MS)
    assert.equal(interval(checks, [credential("codex-a", ASKED + 8), credential("claude-b", ASKED + 25)], STARTED + 30), MINUTE_POLL)
    assert.equal(interval(checks, undefined, STARTED + 1), CHECK_POLL_MS)
    assert.equal(interval(checks, [], STARTED + 1), CHECK_POLL_MS)
  })

  test("stays off while no way in is open, check or no check", () => {
    assert.equal(checkedInterval(false, true), false)
    assert.equal(checkedInterval(false, false), false)
    // Never slower than the page's own pace.
    assert.equal(checkedInterval(2_000, true), 2_000)
  })
})

describe("starting a check", () => {
  test("tells every listener, and lists it with both instants", () => {
    let heard = 0
    const stop = subscribe(() => heard++)
    const at = STARTED + 10_000
    startCheck("listened", ASKED + 9_970, at)
    stop()
    startCheck("unheard", ASKED + 9_971, at + 1)
    assert.equal(heard, 1)
    assert.ok(getChecks().some((check) => check.id === "listened" && check.since === ASKED + 9_970 && check.startedAt === at))
  })

  test("a second press on the account replaces its check rather than stacking", () => {
    const at = STARTED + 20_000
    startCheck("pressed-twice", ASKED + 20_000, at)
    startCheck("pressed-twice", ASKED + 20_040, at + 40)
    const mine = getChecks().filter((check) => check.id === "pressed-twice")
    assert.deepEqual(mine, [{ id: "pressed-twice", since: ASKED + 20_040, startedAt: at + 40 }])
  })

  test("drops checks whose three minutes are up, by this browser's clock", () => {
    const at = STARTED + 30_000
    // Asked long before by CPA's clock, which does not make it old.
    startCheck("kept", ASKED - 3_600, at)
    startCheck("old", ASKED + 30_000, at - CHECK_SECONDS)
    startCheck("new", ASKED + 30_000, at)
    const ids = getChecks().map((check) => check.id)
    assert.ok(ids.includes("kept") && !ids.includes("old") && ids.includes("new"), ids.join(", "))
  })

  test("hands React a new list each time, and the same one between", () => {
    const first = getChecks()
    assert.equal(getChecks(), first)
    startCheck("identity", ASKED + 40_000, STARTED + 40_000)
    assert.notEqual(getChecks(), first)
  })
})
