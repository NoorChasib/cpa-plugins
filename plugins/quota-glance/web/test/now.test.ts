// The page's one clock: when it ticks, when it stops, and what it shows the
// moment the page is seen again.
//
// useNowSeconds (src/lib/now.ts) is useSyncExternalStore over the subscribe
// and getSnapshot in src/lib/clock.ts. clock.ts imports nothing and reads
// `globalThis.document` for its visibility alone, so a stand-in page is the
// whole browser it needs, and node's mock timers stand in for the interval and
// the wall clock. Defined rather than assigned, like collapsed.test.ts's
// storage.
//
// The clock is one per page, so it carries from one test to the next. Each
// test starts an hour later than the one before and stops every reader it
// started, which leaves the clock stopped and always somewhere new to go.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { afterEach, beforeEach, describe, mock, test } from "node:test"

import { getSnapshot, subscribe } from "../src/lib/clock.ts"

const handlers = new Set<() => void>()
const page = {
  visibilityState: "visible",
  addEventListener: (type: string, handler: () => void) => {
    if (type === "visibilitychange") handlers.add(handler)
  },
  removeEventListener: (type: string, handler: () => void) => {
    if (type === "visibilitychange") handlers.delete(handler)
  },
}
Object.defineProperty(globalThis, "document", { value: page, configurable: true })

/** The browser hiding or showing the page, as the menu bar's popover closing or opening does. */
function become(state: "visible" | "hidden"): void {
  page.visibilityState = state
  for (const handler of [...handlers]) handler()
}

let start = 1_800_000_000
const stops: Array<() => void> = []

/** A reader, as one countdown row is: how many times it has been told the second changed. */
function reader(): { heard: number } {
  const ear = { heard: 0 }
  stops.push(subscribe(() => ear.heard++))
  return ear
}

beforeEach(() => {
  start += 3600
  page.visibilityState = "visible"
  mock.timers.enable({ apis: ["setInterval", "Date"], now: start * 1000 })
})

afterEach(() => {
  for (const stop of stops.splice(0)) stop()
  mock.timers.reset()
})

describe("while the page is visible", () => {
  test("the first reader starts the clock at the wall clock's second, and each second after moves it one", () => {
    const row = reader()
    assert.equal(getSnapshot(), start)
    row.heard = 0

    mock.timers.tick(1000)
    assert.equal(getSnapshot(), start + 1)
    assert.equal(row.heard, 1)

    mock.timers.tick(1000)
    mock.timers.tick(1000)
    assert.equal(getSnapshot(), start + 3)
    assert.equal(row.heard, 3)
  })

  test("every reader hears the same tick", () => {
    const first = reader()
    const second = reader()
    first.heard = 0
    second.heard = 0

    mock.timers.tick(1000)
    assert.equal(first.heard, 1)
    assert.equal(second.heard, 1)
  })

  test("a laptop that slept for an hour comes back showing the hour", () => {
    reader()
    // Asleep, no timer fires; only the wall clock moves.
    mock.timers.setTime((start + 3600) * 1000)
    mock.timers.tick(1000)
    assert.equal(getSnapshot(), start + 3601)
  })
})

describe("while the page is hidden", () => {
  test("the clock stops when the page is hidden, and nobody is told anything", () => {
    const row = reader()
    become("hidden")
    row.heard = 0

    mock.timers.tick(90_000)
    assert.equal(getSnapshot(), start)
    assert.equal(row.heard, 0)
  })

  test("a reader that arrives while the page is hidden does not start it", () => {
    page.visibilityState = "hidden"
    const before = getSnapshot()
    const row = reader()

    mock.timers.tick(90_000)
    assert.equal(getSnapshot(), before)
    assert.equal(row.heard, 0)
  })

  test("shown again, the page has the right second before any timer fires, and ticks on from there", () => {
    const row = reader()
    become("hidden")
    mock.timers.tick(90_000)
    row.heard = 0

    become("visible")
    assert.equal(getSnapshot(), start + 90)
    assert.equal(row.heard, 1)

    mock.timers.tick(1000)
    assert.equal(getSnapshot(), start + 91)
    assert.equal(row.heard, 2)
  })
})

describe("with no readers", () => {
  test("the last reader leaving stops the clock and stops listening to the page", () => {
    reader()
    reader()
    for (const stop of stops.splice(0)) stop()
    assert.equal(handlers.size, 0)

    mock.timers.tick(5000)
    become("hidden")
    become("visible")
    assert.equal(getSnapshot(), start)
  })

  test("one reader leaving keeps it running for the other", () => {
    const stay = reader()
    reader()
    stops.pop()!()
    stay.heard = 0

    mock.timers.tick(1000)
    assert.equal(getSnapshot(), start + 1)
    assert.equal(stay.heard, 1)
    assert.equal(handlers.size, 1)
  })

  test("a reader arriving later starts it again at the wall clock's second", () => {
    reader()
    for (const stop of stops.splice(0)) stop()
    mock.timers.tick(30_000)

    reader()
    assert.equal(getSnapshot(), start + 30)
    mock.timers.tick(1000)
    assert.equal(getSnapshot(), start + 31)
  })
})
