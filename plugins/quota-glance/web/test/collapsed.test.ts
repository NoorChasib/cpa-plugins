// Which cards are open: shut by default for everyone, the opened ones kept in
// this browser, and the old list of folded cards dropped on first read so a
// reader who had every card open meets the calmer default too.
//
// src/lib/collapsed.ts reads `globalThis.localStorage` and nothing else, so a
// Map-backed stand-in there is the whole browser it needs. Defined rather than
// assigned: node has a localStorage of its own that wants a file to back it.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { beforeEach, describe, test } from "node:test"

import { CARD, cardKey, openedKeys, setOpened } from "../src/lib/collapsed.ts"

const store = new Map<string, string>()
const storage = {
  getItem: (key: string) => store.get(key) ?? null,
  setItem: (key: string, value: string) => void store.set(key, value),
  removeItem: (key: string) => void store.delete(key),
}
Object.defineProperty(globalThis, "localStorage", { value: storage, configurable: true })

beforeEach(() => store.clear())

describe("opened cards", () => {
  test("every card starts shut", () => {
    assert.deepEqual([...openedKeys()], [])
  })

  test("the old list of folded cards is removed and not read", () => {
    store.set("quota-glance.collapsed", JSON.stringify(["claude:session", "codex:weekly"]))
    assert.deepEqual([...openedKeys()], [])
    assert.equal(store.has("quota-glance.collapsed"), false)
  })

  test("an opened card stays open, and shutting it forgets it", () => {
    const key = cardKey("claude", "weekly")
    assert.deepEqual([...setOpened(key, true)], [key])
    assert.deepEqual([...openedKeys()], [key])
    assert.equal(store.get("quota-glance.opened"), JSON.stringify([key]))
    assert.deepEqual([...setOpened(key, false)], [])
    assert.deepEqual([...openedKeys()], [])
  })

  test("the same row id in two providers is two cards", () => {
    setOpened(cardKey("claude", "session"), true)
    const opened = openedKeys()
    assert.equal(opened.has(cardKey("claude", "session")), true)
    assert.equal(opened.has(cardKey("codex", "session")), false)
  })

  test("the cards that are not windows have keys no row id can take", () => {
    for (const name of Object.values(CARD)) assert.ok(name.startsWith("@"), name)
    setOpened(cardKey("claude", CARD.apiCredits), true)
    assert.equal(openedKeys().has(cardKey("claude", "api-credits")), false)
  })

  test("an unreadable value reads as nothing opened", () => {
    store.set("quota-glance.opened", "{not json")
    assert.deepEqual([...openedKeys()], [])
    store.set("quota-glance.opened", JSON.stringify({ claude: true }))
    assert.deepEqual([...openedKeys()], [])
    store.set("quota-glance.opened", JSON.stringify(["claude:weekly", 3, null]))
    assert.deepEqual([...openedKeys()], ["claude:weekly"])
  })

  test("the list is capped, keeping the most recent", () => {
    for (let index = 0; index < 70; index++) setOpened(cardKey("p", `row${index}`), true)
    const opened = openedKeys()
    assert.equal(opened.size, 64)
    assert.equal(opened.has("p:row69"), true)
    assert.equal(opened.has("p:row0"), false)
  })
})
