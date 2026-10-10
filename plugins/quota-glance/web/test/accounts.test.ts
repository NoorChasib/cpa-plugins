// What the shut Accounts card names: "siphorchannel 2 failed · 11m ago", and
// any account CPA has parked. The ring counts per bucket, so the only instant
// to go on is the server's lastRequestAtEpoch — the end of the newest busy
// bucket — moved back by whole buckets.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"

import {
  accountFlags,
  accountsCardNeeded,
  lastFailure,
  renewalEstimateAt,
  renewalFoot,
  renewalMark,
  renewalOrphanText,
  shortCredentialId,
  yearlyEstimate,
} from "../src/lib/accounts.ts"
import type { Activity, ActivityBucket, Credential, Summary } from "../src/lib/types.ts"

const quiet: ActivityBucket = { success: 0, failed: 0, intensity: 0 }
const ok: ActivityBucket = { success: 3, failed: 0, intensity: 1 }
const bad: ActivityBucket = { success: 0, failed: 2, intensity: 1 }

function ring(buckets: ActivityBucket[], lastRequestAtEpoch: number | null): Activity {
  return {
    bucketSeconds: 600,
    windowSeconds: 600 * buckets.length,
    buckets,
    success: buckets.reduce((sum, bucket) => sum + bucket.success, 0),
    failed: buckets.reduce((sum, bucket) => sum + bucket.failed, 0),
    lastRequestAtEpoch,
    live: false,
  }
}

describe("lastFailure", () => {
  test("no failure in the window is nothing to flag", () => {
    assert.equal(lastFailure(ring([quiet, ok, quiet], 1_000)), null)
  })

  test("a failure in the newest busy bucket is dated by the server's own instant", () => {
    assert.deepEqual(lastFailure(ring([ok, bad, quiet, quiet], 1_000)), { live: false, atEpoch: 1_000 })
  })

  test("an older failure is moved back by whole buckets", () => {
    // The golden siphorchannel: a failure, then a success a bucket later.
    assert.deepEqual(lastFailure(ring([quiet, bad, quiet, ok, quiet], 10_000)), {
      live: false,
      atEpoch: 10_000 - 2 * 600,
    })
  })

  test("a failure in the bucket in progress is now", () => {
    assert.deepEqual(lastFailure(ring([ok, quiet, bad], 2_000)), { live: true, atEpoch: 2_000 })
  })

  test("totals the ring cannot place are left undated", () => {
    const odd = { ...ring([ok, quiet], 1_000), failed: 1 }
    assert.deepEqual(lastFailure(odd), { live: false, atEpoch: null })
  })
})

describe("accountFlags", () => {
  function held(name: string, provider: string): { now: number; credentials: Credential[] } {
    const doc = JSON.parse(
      readFileSync(new URL(`../../testdata/golden/${name}.json`, import.meta.url), "utf8"),
    ) as Summary
    return {
      now: doc.generatedAtEpoch,
      credentials: doc.credentials.filter((credential) => credential.provider === provider),
    }
  }
  const local = (credentials: Credential[]) => credentials.map((credential) => credential.email.split("@")[0]!)
  const ago = (now: number) => (epoch: number) => `${Math.round((now - epoch) / 60)}m ago`
  const said = (flags: ReturnType<typeof accountFlags>) =>
    flags.map((flag) => [flag.name, flag.figure, flag.word, flag.tone].filter(Boolean).join(" "))

  test("names the account whose requests failed, how many and when", () => {
    const { now, credentials } = held("summary", "claude")
    assert.deepEqual(said(accountFlags(credentials, local(credentials), ago(now))), [
      "siphorchannel 2 failed · 10m ago critical",
    ])
  })

  test("names an account CPA has parked, failing or not", () => {
    const { now, credentials } = held("summary-degraded", "claude")
    assert.deepEqual(said(accountFlags(credentials, local(credentials), ago(now))), [
      "disabled off",
      "unavailable 6 failed · 10m ago · cooldown critical",
    ])
  })

  test("a calm provider has a calm fold line", () => {
    const { now, credentials } = held("summary", "codex")
    assert.deepEqual(accountFlags(credentials, local(credentials), ago(now)), [])
  })
})

describe("renewal marks (F.3)", () => {
  const doc = (name: string) =>
    JSON.parse(readFileSync(new URL(`../../testdata/golden/${name}.json`, import.meta.url), "utf8")) as Summary
  const golden = doc("summary")
  const degraded = doc("summary-degraded")
  const credential = (id: string) => golden.credentials.find((one) => one.id === id)!

  test("a date set here carries the dot and is read on the UTC calendar", () => {
    const mark = renewalMark(credential("5f2b8c41d09e7a36"))
    assert.deepEqual(mark, { atEpoch: 1791590400, source: "dashboard", estimated: false, setHere: true, utc: true })
  })

  test("an estimate carries ~, in the reader's own zone", () => {
    const mark = renewalMark(credential("claude-siphorchannel@example.com.json"))
    assert.deepEqual([mark?.source, mark?.estimated, mark?.setHere, mark?.utc], ["estimated", true, false, false])
  })

  test("the provider's own date is neither", () => {
    const mark = renewalMark(credential("codex-noor@example.com.json"))
    assert.deepEqual([mark?.source, mark?.estimated, mark?.setHere], ["reported", false, false])
  })

  test("no renewal, or a plugin too old to say, is no mark", () => {
    assert.equal(renewalMark(credential("claude-noor@example.com.json")), null)
    assert.equal(renewalMark({ renewalAtEpoch: undefined as unknown as null }), null)
    // A plugin before renewalSource: renewalEstimated alone still says estimate.
    assert.equal(renewalMark({ renewalAtEpoch: 1, renewalEstimated: true })?.estimated, true)
  })

  test("a date set here names the estimate Use estimate returns to (F.3)", () => {
    // set here · estimate ~Oct 3 · Use estimate
    assert.equal(renewalEstimateAt(credential("5f2b8c41d09e7a36")), Date.UTC(2026, 9, 3, 11, 20) / 1000)
    // An estimate names itself; a known date with no start names none.
    const estimated = credential("claude-siphorchannel@example.com.json")
    assert.equal(renewalEstimateAt(estimated), estimated.renewalAtEpoch)
    assert.equal(renewalEstimateAt(credential("codex-noor@example.com.json")), null)
    assert.equal(renewalEstimateAt(credential("claude-noor@example.com.json")), null)
    // A plugin too old to send one beside a date set here: none to name.
    assert.equal(renewalEstimateAt({ renewalAtEpoch: 1791590400, renewalSource: "dashboard" }), null)
  })

  test("an estimate more than a month off is a yearly plan's", () => {
    const now = golden.generatedAtEpoch
    assert.equal(yearlyEstimate(credential("claude-agency@example.com.json"), now), true)
    assert.equal(yearlyEstimate(credential("claude-siphorchannel@example.com.json"), now), false)
    assert.equal(yearlyEstimate(credential("5f2b8c41d09e7a36"), now), false)
  })

  test("the foot explains what the rows show, and offers the editor only when it may", () => {
    const claude = (set: Summary) => set.credentials.filter((one) => one.provider === "claude")
    assert.deepEqual(renewalFoot(claude(golden)), { estimated: true, setHere: true, editable: true })
    assert.deepEqual(renewalFoot(claude(degraded)), { estimated: true, setHere: false, editable: false })
  })

  test("a renewal orphan is named by its id's ends, with Remove beside it (E.8 #4)", () => {
    const orphan = degraded.renewalOrphans![0]!
    assert.equal(renewalOrphanText(orphan), "Saved renewal date for an account no longer in CPA: 0123…cdef")
    assert.equal(shortCredentialId("5f2b8c41d09e7a36"), "5f2b…7a36")
    assert.deepEqual(golden.renewalOrphans, [])
  })
})


describe("the renewal editor's card", () => {
  test("one Claude account still has the editor", () => {
    assert.equal(accountsCardNeeded("claude", 1, 0), true)
    assert.equal(accountsCardNeeded("codex", 1, 0), false)
    assert.equal(accountsCardNeeded("claude", 0, 0), false)
  })

  test("renewal orphans keep the editor when the last Claude account disappears", () => {
    assert.equal(accountsCardNeeded("claude", 0, 1), true)
    assert.equal(accountsCardNeeded("codex", 0, 1), false)
  })
})
