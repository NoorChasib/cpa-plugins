// What the shut Accounts card names: "siphorchannel 2 failed · 11m ago", and
// any account CPA has parked. The ring counts per bucket, so the only instant
// to go on is the server's lastRequestAtEpoch — the end of the newest busy
// bucket — moved back by whole buckets.
//
// Run: make -C plugins/quota-glance web-test

import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { describe, test } from "node:test"

import { accountFlags, lastFailure } from "../src/lib/accounts.ts"
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
