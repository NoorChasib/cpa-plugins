import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import type { IncomingMessage, ServerResponse } from "node:http"
import { fileURLToPath } from "node:url"
import type { Plugin } from "vite"

import { cycleOn, dateEpoch, readDate, READING_MAX_AGE, READING_MAX_AHEAD } from "../src/lib/settings.ts"

// A stand-in for the plugin's routes, backed by the committed golden fixtures.
// It exists so the app can be developed and reviewed without a running CPA,
// and it lives in the dev server rather than in the bundle: the shell is served
// unauthenticated, so a fixture full of addresses must never reach it.
//
// What it copies from the real routes, because the page depends on it: the
// paths, ETag revalidation with 304, no-store caching, and the exact answers a
// refusal comes back with. What it adds, because a fixture cannot: scenarios
// for the states the design has to survive, endings for every way a press
// can turn out, and a settings store held in memory so the editor saves end
// to end.
//
// Two doors, as in production. The resource tree is the plugin's own: CPA
// authenticates nothing there and dispatches only GET, and the plugin checks
// the dashboard password itself — here, DEV_TOKEN. The management tree is
// CPA's: its middleware checks the management key before the plugin sees
// anything, answers a refusal with JSON in CPA's own words, and counts every
// missing or wrong key toward a thirty-minute ban of the address. This copies
// that too, ban included, so a page that presents a refused key again shows up
// here as a ban rather than in production. There being no real key, any
// non-empty one is accepted unless a scenario or ending says otherwise.

const MANAGEMENT_PATH = "/v0/management/plugins/quota-glance/summary"
const RESOURCE_PATH = "/v0/resource/plugins/quota-glance/summary"
/** The token door's press: GET only, on the resource tree only. */
const SPEND_PATH = "/v0/resource/plugins/quota-glance/spend"
/** The console door's press: POST, behind CPA's management sign-in. */
const MANAGEMENT_REDEEM_PATH = "/v0/management/plugins/quota-glance/redeem"
/**
 * Registered by the plugin, never dispatched by CPA, which sends nothing but
 * GET to a resource route. The page no longer uses it; it is answered here as
 * CPA answers it, so that it is not proxied either.
 */
const RESOURCE_REDEEM_PATH = "/v0/resource/plugins/quota-glance/redeem"
/**
 * Every path that spends, or could. Each is answered here whatever the method
 * and whatever QUOTA_GLANCE_PROXY says, so no press made against the dev server
 * ever reaches a real CPA.
 */
export const PRESS_PATHS: ReadonlySet<string> = new Set([SPEND_PATH, MANAGEMENT_REDEEM_PATH, RESOURCE_REDEEM_PATH])
/** The token door's save: GET only, the batch in a header. */
const SAVE_SETTINGS_PATH = "/v0/resource/plugins/quota-glance/save-settings"
/** The console door's save: POST, behind CPA's management sign-in. */
const MANAGEMENT_SETTINGS_PATH = "/v0/management/plugins/quota-glance/settings"
/**
 * Both paths that write settings. Each is answered here whatever the method and
 * whatever QUOTA_GLANCE_PROXY says, into the store this fixture keeps in
 * memory, so no save made against the dev server ever reaches a real CPA.
 */
export const SETTINGS_PATHS: ReadonlySet<string> = new Set([SAVE_SETTINGS_PATH, MANAGEMENT_SETTINGS_PATH])
export const DEV_TOKEN = "dev-token"

const fixture = (name: string): string =>
  fileURLToPath(new URL(`../../testdata/golden/${name}.json`, import.meta.url))

/**
 * Every epoch field in the document, so rebasing touches those and nothing else.
 * A test walks both golden documents and fails on any `*Epoch` key missing here.
 */
export const EPOCH_FIELDS: ReadonlySet<string> = new Set([
  "generatedAtEpoch",
  "observedAtEpoch",
  "nextAttemptEpoch",
  "lastObservedEpoch",
  "soonestResetAtEpoch",
  "resetAtEpoch",
  // Activity is dated against the build clock, so a fixture that rebased
  // everything else would show a request from twenty minutes ago as days old —
  // the one number on the page that would look broken rather than stale.
  "lastRequestAtEpoch",
  // The banked-reset deadline, so the countdown beside a credit reads the same
  // distance from now as it would against a live snapshot.
  "expiresAtEpoch",
  // When a Claude reset cooldown lifts, and when a subscription renews: both
  // tick on the page, and both would read as long past without this. A Claude
  // renewal is an estimate the golden build already placed ahead of its own
  // clock, so moving it with the rest keeps it ahead here too.
  "holdUntilEpoch",
  "renewalAtEpoch",
  // When a card's pool is full again, which the card counts down to beside
  // its legend.
  "fullAtEpoch",
  // Claude API credits: each organization's cycle, its next refill and the
  // pool's. These are 00:00 UTC in the document, and the rebase offset is an
  // arbitrary number of seconds, so here they are not: a date printed from
  // one can read a day off, and the YYYY-MM-DD strings beside them in each
  // account's settings are not moved at all. Use ?rebase=0 to review dates
  // exactly. Rounding the offset to whole days instead would move every
  // countdown on the page by up to 23 hours.
  "cycleStartEpoch",
  "renewsAtEpoch",
  "refillAtEpoch",
  // The usage meter: when it began, last saved and stopped, when it last
  // dropped, could not match or could not read a record, and each gap.
  "sinceEpoch",
  "updatedAtEpoch",
  "stoppedAtEpoch",
  "lastDroppedEpoch",
  "lastUnattributedEpoch",
  "lastRejectedEpoch",
  "fromEpoch",
  "toEpoch",
  // Each organization: what its spend counts from, its Console reading, when
  // it was entered, when counting began and traffic was last seen, and its
  // refusals; and each unlinked organization's first and last traffic.
  "spentSinceEpoch",
  "atEpoch",
  "enteredAtEpoch",
  "meterSinceEpoch",
  "lastSeenEpoch",
  "lastAtEpoch",
  "claudeCodeLastAtEpoch",
  "firstSeenEpoch",
])

type Doc = Record<string, unknown>

const nowSeconds = () => Math.floor(Date.now() / 1000)

/**
 * Shifts every instant so the document reads as if it had been built when this
 * dev server started.
 *
 * The fixtures are frozen at a fixed epoch, so without this every countdown is
 * long expired and the page under review looks nothing like the page in
 * service. The offset is fixed at startup rather than recomputed per request,
 * which is what lets countdowns actually run down between polls and lets the
 * ETag stay stable long enough to exercise the 304 path.
 *
 * Durations (`*InSeconds`) are already relative and are left alone; a zero or
 * null epoch means "no such instant" and is left alone too.
 */
function rebase(value: unknown, delta: number): unknown {
  if (Array.isArray(value)) return value.map((item) => rebase(item, delta))
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value as Doc).map(([key, item]) => [
        key,
        EPOCH_FIELDS.has(key) && typeof item === "number" && item > 0 ? item + delta : rebase(item, delta),
      ]),
    )
  }
  return value
}

function stale(doc: Doc, reason: string): Doc {
  return { ...doc, stale: true, staleReason: reason }
}

/** Overrides every balance in a document, for the card states no fixture holds. */
function withBalance(doc: Doc, patch: Doc): Doc {
  return { ...doc, balances: ((doc.balances as Doc[] | undefined) ?? []).map((balance) => ({ ...balance, ...patch })) }
}

const HOUR = 3600
const DAY = 86400

/** A banked-reset inventory, with its instants relative to the document's build. */
type Resets = { count: number; expiresIn: number | null; redeemable: boolean; hold: string; holdIn: number | null }

/** What one fixture credential is given when the golden documents do not say. */
type Extras = { renewalIn?: number; credits?: Doc; resets?: Resets }

/**
 * The account facts the page renders that the golden documents may not carry:
 * Claude banked resets with the provider's hold beside them, and a Codex or
 * Grok account's renewal date and credit balance.
 *
 * Chosen to put every shape on screen at once. In the ordinary fixture: a Codex
 * account with both a renewal and a balance, a Grok account with prepaid
 * credit, and three Claude accounts holding resets — one at a limit and
 * spendable, one not at a limit, one cooling down. In the degraded fixture: a
 * Claude account CPA has parked in cooldown that still gets its button (the
 * case this dashboard used to hide), a disabled one that shows its count and
 * no button, an unlimited Codex balance and a renewal date already passed.
 *
 * Keyed by fixture id, and each matters only where the fixture itself has
 * nothing to say: an absent field, or a null on a credential picked here. Once
 * the golden documents carry real values those win and this steps aside.
 */
const EXTRAS: Record<string, Extras> = {
  // summary.json
  "codex-noor@example.com.json": {
    renewalIn: 24 * DAY + 3 * HOUR,
    credits: { display: "57,706.15", unlimited: false, amount: "57706.1465605", unit: "credits" },
  },
  "xai-noor@example.com.json": {
    credits: { display: "$12.40", unlimited: false, amount: "12.40", unit: "usd" },
  },
  "claude-chasibnoor@example.com.json": {
    resets: { count: 1, expiresIn: 5 * DAY + 2 * HOUR, redeemable: true, hold: "", holdIn: null },
  },
  "claude-siphorchannel@example.com.json": {
    resets: { count: 1, expiresIn: 12 * DAY + 5 * HOUR, redeemable: true, hold: "notLimited", holdIn: null },
  },
  "claude-agency@example.com.json": {
    resets: { count: 2, expiresIn: 26 * DAY, redeemable: true, hold: "cooldown", holdIn: 2 * HOUR + 14 * 60 },
  },
  // summary-degraded.json
  "claude-unavailable@example.com.json": {
    resets: { count: 1, expiresIn: 9 * DAY, redeemable: true, hold: "cooldown", holdIn: 47 * 60 },
  },
  "claude-disabled@example.com.json": {
    resets: { count: 1, expiresIn: null, redeemable: false, hold: "", holdIn: null },
  },
  "codex-model@example.com.json": {
    renewalIn: -2 * HOUR,
    credits: { display: "Unlimited", unlimited: true, amount: "", unit: "credits" },
  },
  "xai-raw@example.com.json": {
    credits: { display: "$0.85", unlimited: false, amount: "0.85", unit: "usd" },
  },
}

function resetsAt(built: number, resets: Resets): Doc {
  return {
    availableCount: resets.count,
    expiresAtEpoch: resets.expiresIn === null ? null : built + resets.expiresIn,
    expiresInSeconds: resets.expiresIn,
    redeemable: resets.redeemable,
    hold: resets.hold,
    holdUntilEpoch: resets.holdIn === null ? null : built + resets.holdIn,
  }
}

/** Fills in EXTRAS. Runs before rebasing, so its instants move with the rest. */
function withAccountExtras(doc: Doc): Doc {
  const built = doc.generatedAtEpoch as number
  const credentials = ((doc.credentials as Doc[] | undefined) ?? []).map((credential) => {
    const extra = EXTRAS[credential.id as string] ?? {}
    const next: Doc = { ...credential }
    if (next.renewalAtEpoch == null) next.renewalAtEpoch = extra.renewalIn === undefined ? null : built + extra.renewalIn
    if (next.credits == null) next.credits = extra.credits ?? null
    if (next.resetCredits == null) next.resetCredits = extra.resets ? resetsAt(built, extra.resets) : null
    else next.resetCredits = { hold: "", holdUntilEpoch: null, ...(next.resetCredits as Doc) }
    return next
  })
  return { ...doc, credentials }
}

/**
 * Every Claude account holding a reset, each under a different hold, so all
 * four hints can be read side by side. Accounts beyond the fourth are left
 * spendable.
 */
function withEveryHold(doc: Doc): Doc {
  const built = doc.generatedAtEpoch as number
  const holds: [string, number | null][] = [
    ["notLimited", null],
    ["cooldown", 3 * HOUR + 20 * 60],
    ["paused", null],
    ["ineligible", null],
  ]
  let next = 0
  const credentials = (doc.credentials as Doc[]).map((credential) => {
    if (credential.provider !== "claude") return credential
    const [hold, holdIn] = holds[next++] ?? ["", null]
    return {
      ...credential,
      resetCredits: resetsAt(built, { count: 1, expiresIn: 14 * DAY, redeemable: true, hold, holdIn }),
    }
  })
  return { ...doc, credentials }
}

/**
 * Every Claude account with its weekly limit spent, which is the session
 * card's face when the pool holds every reporting account out. Neither golden
 * document reaches it, so it is patched from the ordinary one as the server
 * would build it: the counts and shares still add up, as checkPoolAddsUp in
 * pool_test.go demands of a real document.
 */
function withEveryWeeklySpent(doc: Doc): Doc {
  const spent = { remainingFraction: 0, remainingPercent: 0, level: "critical" }
  const providers = (doc.providers as Doc[]).map((provider) => {
    if (provider.id !== "claude") return provider
    const rows = (provider.rows as Doc[]).map((row) => {
      const aggregate = row.aggregate as Doc
      const entries = row.entries as Doc[]
      const patch = (fields: (entry: Doc) => Doc) =>
        entries.map((entry) => (entry.hasReading ? { ...entry, ...fields(entry) } : entry))

      if (row.rowId === "weekly") {
        // Each account that resets next now goes from nothing to full, so
        // the recovery is its whole share of the pool.
        const share = 1 / (aggregate.memberCount as number)
        const next = patch((entry) => ({
          ...spent,
          pooledFraction: 0,
          pooledPercent: 0,
          poolShare: 0,
          recoveryShare: entry.resetsNext ? share : 0,
        }))
        const gain = next.reduce((sum, entry) => sum + ((entry.recoveryShare as number | undefined) ?? 0), 0)
        const gainPercent = Math.round(gain * 100)
        return {
          ...row,
          entries: next,
          aggregate: {
            ...aggregate,
            ...spent,
            projectedGainFraction: gain,
            projectedGainPercent: gainPercent,
            subtext: (aggregate.subtext as string).replace(/^\+\d+%/, `+${gainPercent}%`),
          },
        }
      }

      if (row.rowId === "session") {
        // Every reporting account is held out at its own reading, and a mean
        // over nobody has nothing ahead to announce.
        const next = patch(() => ({ heldOut: true, poolShare: 0, recoveryShare: 0, resetsNext: false }))
        return {
          ...row,
          entries: next,
          aggregate: {
            ...aggregate,
            ...spent,
            memberCount: 0,
            excludedCount: provider.credentialCount,
            heldOutCount: next.filter((entry) => entry.heldOut === true).length,
            trend: "unknown",
            soonestResetAtEpoch: null,
            soonestResetInSeconds: null,
            fullAtEpoch: null,
            fullInSeconds: null,
            projectedGainFraction: 0,
            projectedGainPercent: 0,
            subtext: "",
          },
        }
      }

      if (row.rowId === "weekly_fable") {
        // Each account keeps its own Fable figure but counts as the nothing
        // its weekly leaves. The golden's gain, subtext, soonest reset and
        // full-at all stand: siphorchannel's Fable and weekly reset within
        // the minute, so its recovery is still from nothing to full.
        const next = patch(() => ({ pooledFraction: 0, pooledPercent: 0, poolShare: 0 }))
        return { ...row, entries: next, aggregate: { ...aggregate, ...spent } }
      }

      return row
    })
    return { ...provider, rows }
  })
  return { ...doc, providers }
}

// ---------------------------------------------------------------------------
// Monthly API Credit states the golden documents do not reach

/** Money as the server prints it: "$1,250.00". */
const usd = (amount: number) =>
  `$${amount.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`

const levelOf = (fraction: number) => (fraction <= 0.1 ? "critical" : fraction <= 0.4 ? "low" : "ok")

/**
 * The pool, summed again from its accounts as the server sums it (D.3 rule
 * 11), for a scenario or a save that changed an account's figures. Floats
 * are fine here: this is a stand-in for review, never a contract.
 */
function recountPool(credits: Doc, now: number): Doc {
  const accounts = credits.accounts as Doc[]
  const counted = accounts.filter((account) => account.counted === true)
  const sum = (key: string) => counted.reduce((total, account) => total + ((account[key] as number) ?? 0), 0)
  const credit = sum("monthlyCredit")
  const left = sum("left")
  const used = sum("used")
  const overage = sum("overage")
  const hasEstimate = counted.some((account) => account.hasEstimate === true)
  const fraction = credit > 0 ? Math.max(0, Math.min(1, left / credit)) : 0
  const refilling = counted.filter((account) => typeof account.renewsAtEpoch === "number" && (account.used as number) > 0)
  const soonest = refilling.length > 0 ? Math.min(...refilling.map((account) => account.renewsAtEpoch as number)) : null
  const latest = refilling.length > 0 ? Math.max(...refilling.map((account) => account.renewsAtEpoch as number)) : null
  const gainers = refilling.filter((account) => account.renewsAtEpoch === soonest)
  const gain = gainers.reduce((total, account) => total + (account.used as number), 0)
  const text = (amount: number) => (hasEstimate ? usd(amount) : "")
  return {
    ...credits,
    pool: {
      hasEstimate,
      lowerBound: counted.some((account) => account.lowerBound === true),
      monthlyCredit: hasEstimate ? credit : 0,
      monthlyCreditText: text(credit),
      used: hasEstimate ? used : 0,
      usedText: text(used),
      left: hasEstimate ? left : 0,
      leftText: text(left),
      overage: hasEstimate ? overage : 0,
      overageText: text(overage),
      remainingFraction: hasEstimate ? fraction : 0,
      remainingPercent: hasEstimate ? Math.round(fraction * 100) : 0,
      level: hasEstimate && credit > 0 ? levelOf(fraction) : "",
      nextRefill:
        soonest === null || credit <= 0
          ? null
          : {
              accountIds: gainers.map((account) => account.id),
              refillAtEpoch: soonest,
              refillInSeconds: soonest - now,
              gain,
              gainText: usd(gain),
              gainFraction: gain / credit,
              gainPercent: Math.round((gain / credit) * 100),
            },
      fullAtEpoch: latest,
      fullInSeconds: latest === null ? null : latest - now,
      accountCount: accounts.length,
      countedCount: counted.length,
      missingCount: accounts.length - counted.length,
    },
  }
}

/** Replaces one account of a document's API credits, by label. */
function patchAccounts(doc: Doc, patches: Record<string, (account: Doc) => Doc>): Doc {
  const credits = doc.apiCredits as Doc
  const accounts = (credits.accounts as Doc[]).map((account) => {
    const patch = patches[account.label as string]
    return patch ? patch(account) : account
  })
  return { ...doc, apiCredits: { ...credits, accounts } }
}

/** A fake Console organization's id, as the D.7 fixtures write them. */
const fakeOrg = (n: number) => `00000000-0000-4000-8000-${n.toString(16).padStart(12, "0")}`

/**
 * Every row state of F.2 the golden document does not hold, side by side:
 * spend past the credit, a reading above the credit, a Claude Code-based
 * refusal that is not out, a credit of $0.00, counting that began mid-cycle
 * on an organization with no traffic, a reading taken on the refill day, a
 * reading from before the last refill, and more unlinked organizations than
 * the pool names. The sentences are the server's (D.4).
 */
function withAPIStates(doc: Doc): Doc {
  const built = doc.generatedAtEpoch as number
  let next = patchAccounts(doc, {
    alpha: (account) => ({
      ...account,
      spent: 240.1,
      spentText: "$240.10",
      used: 200,
      usedText: "$200.00",
      left: 0,
      leftText: "$0.00",
      overage: 40.1,
      overageText: "$40.10",
      remainingFraction: 0,
      remainingPercent: 0,
      level: "critical",
      dataIssues: ["overCredit"],
      issue:
        "Spend is $40.10 past the monthly credit. Anthropic bills purchased credit after the monthly credit; if there is none, enter a Console reading.",
    }),
    bravo: (account) => ({
      ...account,
      left: 252,
      leftText: "$252.00",
      used: 0,
      usedText: "$0.00",
      remainingFraction: 1,
      remainingPercent: 100,
      level: "ok",
      reading: { ...(account.reading as Doc), remaining: 260, remainingText: "$260.00" },
      settings: { ...(account.settings as Doc), reading: { ...((account.settings as Doc).reading as Doc), remainingUsd: "260.00" } },
      dataIssues: ["readingAboveCredit"],
      issue: "Your Console reading is more than the monthly credit; check the monthly credit.",
    }),
    charlie: (account) => ({
      ...account,
      refusals: { total: 0, lastAtEpoch: null, claudeCodeTotal: 4, claudeCodeLastAtEpoch: built - 40 * 60 },
      dataIssues: ["claudeCodeRefused"],
      issue:
        "Anthropic refused requests from a Claude Code-based client (Claude Code or the Agent SDK) for low credit. If they were Agent SDK requests, this credit may be spent; enter a Console reading.",
    }),
    delta: (account) => ({
      ...account,
      monthlyCredit: 0,
      monthlyCreditText: "$0.00",
      monthlyCreditSource: "dashboard",
      used: 0,
      usedText: "$0.00",
      left: 0,
      leftText: "$0.00",
      overage: 30,
      overageText: "$30.00",
      remainingFraction: 0,
      remainingPercent: 0,
      level: "",
      cacheWriteExtra: 0,
      cacheWriteExtraText: "",
      settings: { ...(account.settings as Doc), monthlyUsd: "0", revision: "4", updatedAtEpoch: built - 3 * HOUR },
      dataIssues: ["zeroCredit", "overCredit"],
      issue: "This organization's monthly credit is set to $0.00.",
    }),
    echo: (account) => ({
      ...account,
      lowerBound: true,
      spent: 0,
      spentText: "$0.00",
      used: 0,
      usedText: "$0.00",
      left: 500,
      leftText: "$500.00",
      remainingFraction: 1,
      remainingPercent: 100,
      meterSinceEpoch: built - 2 * HOUR,
      lastSeenEpoch: null,
      dataIssues: ["meterStartedLate", "noTraffic"],
      issue: "Counting began after this cycle started, so earlier spend is missing. Enter a Console reading to correct it.",
    }),
  })
  const credits = next.apiCredits as Doc
  const echo = (credits.accounts as Doc[]).find((account) => account.label === "echo")!
  const extra = (n: number, label: string, patch: Doc): Doc => ({
    ...echo,
    id: `org-${String(n).repeat(12).slice(0, 12)}`,
    label,
    order: 4 + n,
    organizationId: fakeOrg(10 + n),
    lowerBound: false,
    meterSinceEpoch: echo.meterSinceEpoch,
    lastSeenEpoch: built - 20 * 60,
    settings: { ...(echo.settings as Doc), configMonthlyUsd: "100", configRenews: "2026-09-10" },
    ...patch,
  })
  const cycleStart = echo.cycleStartEpoch as number
  const accounts = [
    ...(credits.accounts as Doc[]),
    extra(1, "foxtrot", {
      basis: "reading",
      monthlyCredit: 100,
      monthlyCreditText: "$100.00",
      spent: 0.5,
      spentText: "$0.50",
      used: 0.5,
      usedText: "$0.50",
      left: 99.5,
      leftText: "$99.50",
      remainingFraction: 0.995,
      remainingPercent: 100,
      level: "ok",
      reading: {
        remaining: 100,
        remainingText: "$100.00",
        atEpoch: cycleStart + HOUR,
        enteredAtEpoch: cycleStart + HOUR + 300,
        spentSince: 0.5,
        spentSinceText: "$0.50",
      },
      settings: {
        ...(echo.settings as Doc),
        configMonthlyUsd: "100",
        configRenews: "2026-09-10",
        revision: "3",
        reading: { remainingUsd: "100.00", atEpoch: cycleStart + HOUR, enteredAtEpoch: cycleStart + HOUR + 300 },
        updatedAtEpoch: cycleStart + HOUR + 300,
      },
      dataIssues: ["readingOnRefillDay"],
      issue:
        "This Console reading was taken on the refill day. If Console did not show the new credit yet, enter a new reading once it does.",
    }),
    extra(2, "golf", {
      monthlyCredit: 200,
      monthlyCreditText: "$200.00",
      spent: 12,
      spentText: "$12.00",
      used: 12,
      usedText: "$12.00",
      left: 188,
      leftText: "$188.00",
      remainingFraction: 0.94,
      remainingPercent: 94,
      level: "ok",
      settings: {
        ...(echo.settings as Doc),
        configMonthlyUsd: "200",
        configRenews: "2026-09-10",
        revision: "2",
        reading: { remainingUsd: "143.20", atEpoch: cycleStart - 10 * DAY, enteredAtEpoch: cycleStart - 10 * DAY + 600 },
        readingUnusedReason: "beforeRefill",
        updatedAtEpoch: cycleStart - 10 * DAY + 600,
      },
      dataIssues: ["readingUnused"],
      issue: `Your Console reading of $143.20 on ${new Date((cycleStart - 10 * DAY) * 1000).toLocaleDateString("en-US", {
        month: "short",
        day: "numeric",
        timeZone: "UTC",
      })} was before the last refill, so it is not used.`,
    }),
    extra(3, "hotel", {
      monthlyCredit: 100, monthlyCreditText: "$100.00", spent: 93, spentText: "$93.00",
      used: 93, usedText: "$93.00", left: 7, leftText: "$7.00", overage: 0, overageText: "$0.00",
      remainingFraction: 0.07, remainingPercent: 7, level: "critical", dataIssues: [], issue: "",
    }),
    extra(4, "india", {
      monthlyCredit: 100, monthlyCreditText: "$100.00", spent: 0, spentText: "$0.00",
      used: 0, usedText: "$0.00", left: 100, leftText: "$100.00", overage: 0, overageText: "$0.00",
      remainingFraction: 1, remainingPercent: 100, level: "ok", lastSeenEpoch: null,
      dataIssues: ["noTraffic"], issue: "No API traffic for this organization has reached CPA since counting began.",
    }),
  ]
  const unlinked = [
    ...(credits.unlinked as Doc[]),
    { organizationId: fakeOrg(0x20), firstSeenEpoch: built - 3 * DAY, lastSeenEpoch: built - 50 * 60, requests: 1, reason: "notConfigured" },
    { organizationId: fakeOrg(0x21), firstSeenEpoch: built - 6 * DAY, lastSeenEpoch: built - 5 * HOUR, requests: 12, reason: "overLimit" },
    { organizationId: fakeOrg(0x22), firstSeenEpoch: built - 9 * DAY, lastSeenEpoch: built - 2 * DAY, requests: 4, reason: "notConfigured" },
  ]
  next = { ...next, apiCredits: recountPool({ ...credits, accounts, unlinked }, built) }
  return next
}

/**
 * Quota Cache switched off two hours ago: the meter's open gap makes every
 * counted organization a lower bound, and the card says counting stopped.
 */
function withMeterStopped(doc: Doc): Doc {
  const built = doc.generatedAtEpoch as number
  const credits = doc.apiCredits as Doc
  const gap =
    "Quota Cache was not counting for part of this period, for example while it was off or reloading, so some spend may be missing. Enter a Console reading to correct it."
  const accounts = (credits.accounts as Doc[]).map((account) =>
    account.counted
      ? {
          ...account,
          lowerBound: true,
          dataIssues: ["meterGap", ...(account.dataIssues as string[])],
          issue: (account.dataIssues as string[]).length === 0 ? gap : account.issue,
        }
      : account,
  )
  const meter = { ...(credits.meter as Doc), stoppedAtEpoch: built - 2 * HOUR, stopReason: "disabled", updatedAtEpoch: built - 2 * HOUR }
  return { ...doc, apiCredits: recountPool({ ...credits, meter, accounts }, built) }
}

/**
 * No meter file at all: Quota Cache older than 0.1.14, or not loaded since.
 * Every organization waits for one, and nothing is counted.
 */
function withoutMeter(doc: Doc): Doc {
  const built = doc.generatedAtEpoch as number
  const credits = doc.apiCredits as Doc
  const blank = {
    state: "pending",
    counted: false,
    hasEstimate: false,
    basis: "",
    lowerBound: false,
    spent: 0,
    spentText: "",
    spentSinceEpoch: null,
    used: 0,
    usedText: "",
    left: 0,
    leftText: "",
    overage: 0,
    overageText: "",
    remainingFraction: 0,
    remainingPercent: 0,
    level: "",
    reading: null,
    cacheWriteExtra: 0,
    cacheWriteExtraText: "",
    meterSinceEpoch: null,
    lastSeenEpoch: null,
    dataIssues: ["meterMissing"],
    issue: "Quota Cache has not saved an API meter yet. Update it to 0.1.14 or newer; counting starts when it next loads.",
  }
  const accounts = (credits.accounts as Doc[]).map((account) => ({ ...account, ...blank }))
  return { ...doc, apiCredits: recountPool({ ...credits, meter: null, accounts, unlinked: [] }, built) }
}

/** settings.json could not be read: nothing set here applies, and nothing can be edited. */
function withSettingsUnreadable(doc: Doc): Doc {
  // None of a corrupt settings file applies. Drop the seeded overrides and
  // let the config's values stand before closing the editor.
  const store = seedStore(doc)
  for (const id of [...store.credits.keys(), ...store.renewals.keys()]) store.touched.add(id)
  store.credits.clear()
  store.renewals.clear()
  doc = applyStore(doc, store, doc.generatedAtEpoch as number)
  const credits = doc.apiCredits as Doc
  const accounts = (credits.accounts as Doc[]).map((account) => {
    const settings = account.settings as Doc
    const own = ["noOrganization", "duplicateOrganization", "overLimit", "cacheTooOld"].includes(settings.notEditableReason as string)
    return { ...account, settings: { ...settings, editable: false, notEditableReason: own ? settings.notEditableReason : "settingsUnreadable" } }
  })
  const credentials = (doc.credentials as Doc[]).map((credential) => ({ ...credential, renewalEditable: false }))
  return {
    ...doc,
    credentials,
    apiCredits: { ...credits, accounts, editing: { available: false, reason: "settingsUnreadable" } },
  }
}

/**
 * The degraded document with allow-edit on, so the editor can be reviewed on
 * the rows that cannot take it: an item with no organization-id, a duplicate,
 * one past the sixteenth, one Quota Cache 0.1.13 still reads, and an orphan.
 */
function withEditing(doc: Doc): Doc {
  const credits = doc.apiCredits as Doc
  const accounts = (credits.accounts as Doc[]).map((account) => {
    const settings = account.settings as Doc
    return settings.notEditableReason === "disabled"
      ? { ...account, settings: { ...settings, editable: true, notEditableReason: "" } }
      : account
  })
  return { ...doc, apiCredits: { ...credits, accounts, editing: { available: true, reason: "" } } }
}

/** The goldens retain historical filename ids, which cannot key saved dates.
 * Give Claude fake auth indexes to review every renewal field's source note. */
function withEditableRenewals(doc: Doc): Doc {
  const ids = new Map<string, string>()
  const claude = (doc.credentials as Doc[]).filter((one) => one.provider === "claude")
  claude.forEach((one, i) => {
    ids.set(one.id as string, (i + 1).toString(16).padStart(16, "0"))
  })
  const rename = (value: unknown): unknown => {
    if (typeof value === "string") return ids.get(value) ?? value
    if (Array.isArray(value)) return value.map(rename)
    if (value && typeof value === "object") return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, rename(item)]))
    return value
  }
  const next = rename(doc) as Doc
  return { ...next, credentials: (next.credentials as Doc[]).map((one) => ({ ...one, renewalEditable: one.provider === "claude" })) }
}

type Outcome = Doc | "unauthorized" | "down"

/** The states §5 of the handoff requires the app to render legibly. */
function scenarios(): Record<string, () => Outcome> {
  const golden = () => withAccountExtras(JSON.parse(readFileSync(fixture("summary"), "utf8")) as Doc)
  const degraded = () => withAccountExtras(JSON.parse(readFileSync(fixture("summary-degraded"), "utf8")) as Doc)

  return {
    golden,
    degraded,

    // Every hint a Claude reset can carry beside its button, at once.
    holds: () => withEveryHold(golden()),

    // Every Claude account has burned its weekly: the session card's face
    // when every reporting account is held out of its mean.
    "weekly-spent": () => withEveryWeeklySpent(golden()),

    // Data still renders; a banner names the reason above it.
    "stale-cache": () => stale(golden(), "cacheStale"),
    "stale-missing": () => stale(golden(), "cacheMissing"),
    "stale-roster": () => stale(golden(), "rosterUnavailable"),
    "stale-schema": () => stale(golden(), "snapshotSchemaUnsupported"),

    // Nothing has ever been polled: stale, and genuinely empty underneath.
    "never-observed": () => ({
      ...stale(golden(), "neverObserved"),
      observedAtEpoch: null,
      nextAttemptEpoch: null,
      counters: { credentials: 3, observedOK: 0, observeError: 0 },
      credentials: (golden().credentials as Doc[]).slice(0, 3).map((c) => ({
        ...c,
        status: "pending",
        lastObservedEpoch: 0,
      })),
      providers: [],
    }),

    // Not an error: a fresh install with no quota provider among its credentials.
    empty: () => ({
      ...golden(),
      counters: { credentials: 0, observedOK: 0, observeError: 0 },
      credentials: [],
      providers: [],
    }),

    // The OpenRouter card's other two faces. The degraded fixture already
    // carries a low balance whose last poll failed and has gone stale; these
    // are the two states no fixture holds. The wording is the server's.
    "balance-out": () =>
      withBalance(golden(), {
        remaining: -1.2,
        remainingText: "-$1.20",
        used: 101.7,
        level: "critical",
        subtext: "Out of credit · $101.70 spent of $100.50 purchased",
      }),
    "balance-unread": () =>
      withBalance(golden(), {
        hasReading: false,
        remaining: 0,
        remainingText: "",
        purchased: 0,
        used: 0,
        level: "",
        observedAtEpoch: 0,
        state: "pending",
        dataIssues: ["refreshPending"],
        subtext: "Waiting for Quota Cache to read the balance.",
      }),

    // Small installs still need the renewal editor, including an install
    // whose last Claude account disappeared but left a saved renewal date.
    "single-claude": () => {
      const doc = golden()
      const credential = (doc.credentials as Doc[]).find((one) => one.provider === "claude" && one.renewalEditable)!
      return { ...doc, credentials: [credential], providers: [], apiCredits: null }
    },
    "renewal-orphans": () => ({
      ...golden(), credentials: [], providers: [], apiCredits: null,
      renewalOrphans: [{ id: "0123456789abcdef", date: "2026-10-29", revision: "1", updatedAtEpoch: 1791547200 }],
    }),

    // The Monthly API Credit card's states the golden documents do not hold.
    "api-states": () => withAPIStates(golden()),
    "meter-stopped": () => withMeterStopped(golden()),
    "no-meter": () => withoutMeter(golden()),
    "settings-unreadable": () => withSettingsUnreadable(golden()),
    "degraded-editable": () => withEditing(degraded()),
    "renewals-editable": () => withEditableRenewals(golden()),

    // The plugin has been updated past what this bundle knows how to read.
    "future-schema": () => ({ ...golden(), schemaVersion: 2 }),

    unauthorized: () => "unauthorized",
    // Serve the document, but only through the dashboard password: CPA
    // refuses the console session first, each in its own words (see
    // CPA_REFUSALS). Exercise the console latch, the fall from a refused
    // console session to the password, and the sign-in screen's copy for each.
    "cpa-expired": golden,
    "cpa-banned": golden,
    "cpa-remote-off": golden,
    // The other way round: the plugin refuses the dashboard password, and only
    // a console session gets in.
    "token-refused": golden,
    down: () => "down",
  }
}

/**
 * Pushes the first countdown on the page a few seconds out, anchored to real
 * time rather than the rebase offset, so it runs out while the page is open and
 * has to say "resetting…" instead of counting past zero.
 */
function expiring(doc: Doc, inSeconds: number): Doc {
  const row = ((doc.providers as Doc[])[0]?.rows as Doc[] | undefined)?.[0]
  const entry = (row?.entries as Doc[] | undefined)?.[0]
  if (!row || !entry) return doc
  const at = nowSeconds() + inSeconds
  entry.resetAtEpoch = at
  entry.resetInSeconds = inSeconds
  entry.resetDisplayHint = "countdown"
  row.aggregate = { ...(row.aggregate as Doc), soonestResetAtEpoch: at, soonestResetInSeconds: inSeconds }
  return doc
}

/** A reply, kept whole so the press ledger can hand back the same bytes. */
type Reply = { status: number; headers: Record<string, string>; body: string }

/** The plugin's own JSON, as jsonResponse in internal/api/api.go writes it. */
function pluginJSON(status: number, body: unknown): Reply {
  return {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" },
    body: JSON.stringify(body),
  }
}

function send(res: ServerResponse, reply: Reply, extra: Record<string, string> = {}): void {
  res.statusCode = reply.status
  for (const [name, value] of Object.entries({ ...reply.headers, ...extra })) res.setHeader(name, value)
  res.end(reply.body)
}

function json(res: ServerResponse, status: number, body: unknown): void {
  send(res, pluginJSON(status, body))
}

/**
 * A reply with no body. CPA's own 404, for a route it does not dispatch, is
 * exactly this; the plugin's refusal of a wrong password adds no-store.
 */
function bare(res: ServerResponse, status: number, noStore: boolean): void {
  res.statusCode = status
  if (noStore) res.setHeader("Cache-Control", "no-store")
  res.end()
}

/** The dashboard password, as the plugin reads it off a resource request. */
function presentedToken(req: IncomingMessage): string {
  return (req.headers.authorization ?? "").replace(/^Bearer /i, "").trim()
}

/** The management key, as CPA's middleware reads it: bearer, else the whole header, else X-Management-Key. */
function presentedKey(req: IncomingMessage): string {
  const header = req.headers.authorization ?? ""
  const space = header.indexOf(" ")
  const provided = space >= 0 && header.slice(0, space).toLowerCase() === "bearer" ? header.slice(space + 1) : header
  if (provided !== "") return provided
  const fallback = req.headers["x-management-key"]
  return typeof fallback === "string" ? fallback : ""
}

/**
 * CPA's refusals, in its own words — AuthenticateManagementKey in CPA's
 * internal/api/handlers/management/handler.go — which the page's classifier
 * matches exactly. `counted` is whether CPA adds it to the address's failures.
 */
const CPA_REFUSALS = {
  banned: { status: 403, error: "IP banned due to too many failed attempts. Try again in 29m40s", counted: false },
  remote: { status: 403, error: "remote management disabled", counted: false },
  missing: { status: 401, error: "missing management key", counted: true },
  invalid: { status: 401, error: "invalid management key", counted: true },
} as const

type CPARefusal = keyof typeof CPA_REFUSALS

/** The summary scenarios in which CPA refuses the console session. */
const CPA_SCENARIOS: Record<string, CPARefusal> = {
  unauthorized: "invalid",
  "cpa-expired": "invalid",
  "cpa-banned": "banned",
  "cpa-remote-off": "remote",
}

/** The press endings in which CPA refuses the console session before the plugin runs. */
const CPA_ENDINGS: Record<string, CPARefusal> = {
  "key-refused": "invalid",
  "ip-banned": "banned",
  "remote-off": "remote",
}

/** The press endings the plugin's gate answers on the token door, before its ledger. */
const GATE_ENDINGS = new Set(["cross-site", "too-early"])

/** A Go duration as CPA prints a ban's remainder: rounded to the second. */
function goDuration(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000))
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return `${h}h${m}m${s}s`
  if (m > 0) return `${m}m${s}s`
  return `${s}s`
}

/**
 * Whether the browser says this request did not come from a script on this
 * page, as crossSite in internal/api/api.go reads it: each header may be
 * absent, but a present one must say same-origin, a script fetch, and no
 * destination, and nothing may say it is a prefetch.
 */
function crossSite(headers: IncomingMessage["headers"]): boolean {
  const only = (name: string, ...allowed: string[]) => {
    const value = headers[name]
    if (value === undefined) return true
    return (Array.isArray(value) ? value : value.split(",")).every((one) => allowed.includes(one.trim().toLowerCase()))
  }
  if (!only("sec-fetch-site", "same-origin") || !only("sec-fetch-mode", "cors", "same-origin") || !only("sec-fetch-dest", "empty")) {
    return true
  }
  return headers["sec-purpose"] !== undefined || headers.purpose !== undefined
}

const PRESS_ID = /^[A-Za-z0-9_-]{16,64}$/
const MAX_PRESS_BYTES = 4096

type Press = { credentialId: string; pressId: string | null }

/**
 * A press, from the POST body or the decoded spend header — the same object
 * either way — or the error code the plugin answers with when it is not one.
 */
function parsePress(raw: string, pressRequired: boolean): Press | string {
  let body: unknown
  try {
    body = JSON.parse(raw)
  } catch {
    return "invalid_request"
  }
  if (body === null || typeof body !== "object" || Array.isArray(body)) return "invalid_request"
  const { credentialId, confirmed, pressId } = body as Record<string, unknown>
  if (credentialId !== undefined && typeof credentialId !== "string") return "invalid_request"
  if (confirmed !== undefined && typeof confirmed !== "boolean") return "invalid_request"
  if (confirmed !== true) return "confirmation_required"
  if (pressId === undefined || pressId === null) {
    return pressRequired ? "invalid_request" : { credentialId: credentialId ?? "", pressId: null }
  }
  if (typeof pressId !== "string" || !PRESS_ID.test(pressId)) return "invalid_request"
  return { credentialId: credentialId ?? "", pressId }
}

/**
 * A header carrying JSON as unpadded base64url, decoded as the plugin decodes
 * it, or null when it is not one: absent, repeated, too long, or anything but
 * base64url. Node's decoder skips what it does not understand, and the
 * plugin's refuses it.
 */
function headerJSON(value: string | string[] | undefined, maxBytes: number): string | null {
  if (typeof value !== "string" || value.length > Math.ceil((maxBytes * 4) / 3)) return null
  if (!/^[A-Za-z0-9_-]*$/.test(value) || value.length % 4 === 1) return null
  let raw: string
  try {
    raw = new TextDecoder("utf-8", { fatal: true }).decode(Buffer.from(value, "base64url"))
  } catch {
    return null
  }
  return raw.trimStart().startsWith("{") ? raw : null
}

/** The spend header, decoded as the plugin decodes it, or the error code to answer with. */
function pressFromHeader(value: string | string[] | undefined): Press | string {
  if (typeof value === "string" && value.length > MAX_PRESS_BYTES) return "invalid_request"
  const raw = headerJSON(value, MAX_PRESS_BYTES)
  return raw === null ? "invalid_request" : parsePress(raw, true)
}

/**
 * What the plugin answers a fresh press with, per ?redeem=. `gateway` and
 * `dropped` are the plugin spending the reset and the answer being lost on the
 * way back, which is why their reply is a reset: a second press with the same
 * press id is answered with it, from the ledger.
 */
function pressReply(ending: string, provider: string): Reply {
  const outcome = (name: string, windowsReset: number, remainingCount: number) =>
    pluginJSON(200, { provider, outcome: name, windowsReset, remainingCount, snapshotPending: true })
  switch (ending) {
    case "nothing-to-reset":
      return outcome("nothingToReset", 0, 0)
    case "no-credit":
      return outcome("noCredit", 0, 0)
    case "failed":
      return outcome("failed", 0, 1)
    // Claude's refusals: nothing spent, the count unchanged.
    case "not-limited":
      return outcome("notLimited", 0, 1)
    case "cooldown":
      return outcome("cooldown", 0, 1)
    case "paused":
      return outcome("paused", 0, 1)
    case "ineligible":
      return outcome("ineligible", 0, 1)
    // The answer to a second press after an unknown outcome, when the first
    // had gone through.
    case "already-used":
      return outcome("alreadyUsed", 0, 1)
    case "refused":
      return pluginJSON(502, { error: "provider_refused" })
    case "unavailable":
      return pluginJSON(502, { error: "provider_unavailable" })
    case "rate-limited":
      return pluginJSON(502, { error: "provider_rate_limited" })
    // A first unknown answer: the claim's window closes ten minutes on.
    case "outcome-unknown":
      return pluginJSON(502, { error: "outcome_unknown", retryUntilEpoch: nowSeconds() + 600 })
    // An unknown claim whose window closed before it could be settled.
    case "window-closed":
      return pluginJSON(502, { error: "retry_window_closed" })
    case "unusable":
      return pluginJSON(409, { error: "credential_unusable" })
    case "in-flight":
      return pluginJSON(409, { error: "already_in_flight" })
    case "not-redeemable":
      return pluginJSON(409, { error: "not_redeemable" })
    default:
      return outcome("reset", 2, 1)
  }
}

/** Every ending, for the warning that names them when ?redeem= is misspelled. */
const ENDINGS = [
  "reset",
  "nothing-to-reset",
  "no-credit",
  "failed",
  "not-limited",
  "cooldown",
  "paused",
  "ineligible",
  "already-used",
  "refused",
  "unavailable",
  "rate-limited",
  "outcome-unknown",
  "window-closed",
  "unusable",
  "in-flight",
  "not-redeemable",
  "switched-off",
  // The transport, after the plugin answered.
  "gateway",
  "dropped",
  // Console door only: CPA, before the plugin.
  ...Object.keys(CPA_ENDINGS),
  // Token door only: the plugin's gate, before its ledger.
  ...GATE_ENDINGS,
]

/** How long the press ledger remembers a finished press, as the plugin's does. */
const LEDGER_TTL_MS = 10 * 60 * 1000

// ---------------------------------------------------------------------------
// Settings: what the editor saves, kept in memory

type StoredReading = { remainingUsd: string; at: number; enteredAt: number; spentSince?: number }
type StoredCredit = { monthlyUsd: string | null; renews: string | null; reading: StoredReading | null; rev: number; updatedAt: number }
type StoredRenewal = { date: string; rev: number; updatedAt: number }

/**
 * One scenario's settings.json, as internal/overrides holds it: the file's
 * revision, and each entry with the revision that last wrote it. Seeded from
 * the document's own settings blocks, so a first save meets the revisions
 * the page was shown. `touched` is every id a save here has written: only
 * those are laid over the document served, so an untouched row keeps the
 * fixture's own figures, rebased.
 */
interface SettingsStore {
  revision: number
  credits: Map<string, StoredCredit>
  renewals: Map<string, StoredRenewal>
  touched: Set<string>
}

function seedStore(doc: Doc): SettingsStore {
  const store: SettingsStore = { revision: 0, credits: new Map(), renewals: new Map(), touched: new Set() }
  const credits = doc.apiCredits as Doc | null | undefined
  for (const account of (credits?.accounts as Doc[] | undefined) ?? []) {
    const settings = account.settings as Doc
    if (!settings.revision) continue
    const reading = settings.reading as Doc | null
    store.credits.set(account.id as string, {
      monthlyUsd: (settings.monthlyUsd as string) || null,
      renews: (settings.renews as string) || null,
      reading: reading
        ? { remainingUsd: reading.remainingUsd as string, at: reading.atEpoch as number, enteredAt: reading.enteredAtEpoch as number,
            spentSince: ((account.reading as Doc | null)?.spentSince as number | undefined) ?? 0 }
        : null,
      rev: Number(settings.revision),
      updatedAt: (settings.updatedAtEpoch as number | null) ?? 0,
    })
  }
  for (const orphan of (credits?.orphans as Doc[] | undefined) ?? []) {
    store.credits.set(orphan.id as string, {
      monthlyUsd: (orphan.monthlyUsd as string) || null,
      renews: (orphan.renews as string) || null,
      reading: null,
      rev: Number(orphan.revision),
      updatedAt: orphan.updatedAtEpoch as number,
    })
  }
  const renewals: Doc[] = [
    ...((doc.credentials as Doc[] | undefined) ?? []).flatMap((credential): Doc[] =>
      credential.renewalSetting ? [{ id: credential.id, ...(credential.renewalSetting as Doc) }] : [],
    ),
    ...((doc.renewalOrphans as Doc[] | undefined) ?? []),
  ]
  for (const renewal of renewals) {
    store.renewals.set(renewal.id as string, {
      date: renewal.date as string,
      rev: Number(renewal.revision),
      updatedAt: renewal.updatedAtEpoch as number,
    })
  }
  const revs = [...store.credits.values(), ...store.renewals.values()].map((entry) => entry.rev)
  store.revision = Math.max(0, ...revs)
  return store
}

/** When a renewal date set here next comes round: the date while it is ahead, else the next monthly one. */
function nextRenewal(date: string, now: number): number {
  const day = dateEpoch(date)
  if (day > now) return day
  return cycleOn(date, now)?.end ?? day
}

/**
 * The served document with every row a save here wrote laid over it: each
 * account's settings block and the sources it gives, and — for a counted
 * account — its credit, cycle and estimate figured again the simple way,
 * then the pool. A stand-in so a save can be seen to land; it does not price
 * the meter, and an account's state is the fixture's.
 */
function applyStore(doc: Doc, store: SettingsStore, now: number): Doc {
  if (store.touched.size === 0) return doc
  const credits = doc.apiCredits as Doc | null | undefined
  let next = doc
  if (credits) {
    const accounts = (credits.accounts as Doc[]).map((account) => {
      const id = account.id as string
      if (!store.touched.has(id)) return account
      const entry = store.credits.get(id)
      const settings = account.settings as Doc
      const monthlyUsd = entry?.monthlyUsd ?? ""
      const renews = entry?.renews ?? ""
      const configCredit = settings.configMonthlyUsdInvalid ? "" : (settings.configMonthlyUsd as string)
      const configRenews = settings.configRenewsInvalid ? "" : (settings.configRenews as string)
      const reading = entry?.reading ?? null
      const patched: Doc = {
        ...account,
        monthlyCreditSource: monthlyUsd ? "dashboard" : configCredit ? "config" : "none",
        renewsSource: renews ? "dashboard" : configRenews ? "config" : "none",
        settings: {
          ...settings,
          revision: entry ? String(entry.rev) : "",
          monthlyUsd,
          renews,
          reading: reading ? { remainingUsd: reading.remainingUsd, atEpoch: reading.at, enteredAtEpoch: reading.enteredAt } : null,
          readingUnusedReason: "",
          updatedAtEpoch: entry?.updatedAt ?? null,
        },
      }
      if (!account.counted || account.state === "out") return patched
      const grantText = monthlyUsd || configCredit
      const refill = renews || configRenews
      const cycle = refill ? cycleOn(refill, now) : null
      if (!grantText || !cycle) return patched
      const grant = Number(grantText)
      const spent = account.spent as number
      const sinceReading = reading?.spentSince ?? 0
      const left = reading ? Math.max(0, Number(reading.remainingUsd) - sinceReading) : Math.max(0, grant - spent)
      const used = Math.max(0, Math.min(grant, grant - left))
      const overage = reading ? 0 : Math.max(0, spent - grant)
      const fraction = grant > 0 ? Math.max(0, Math.min(1, left / grant)) : 0
      return {
        ...patched,
        basis: reading ? "reading" : "credit",
        monthlyCredit: grant,
        monthlyCreditText: usd(grant),
        cycleStartEpoch: cycle.start,
        renewsAtEpoch: cycle.end,
        renewsInSeconds: cycle.end - now,
        left,
        leftText: usd(left),
        used,
        usedText: usd(used),
        overage,
        overageText: usd(overage),
        remainingFraction: fraction,
        remainingPercent: Math.round(fraction * 100),
        level: grant > 0 ? levelOf(fraction) : "",
        reading: reading
          ? {
              remaining: Number(reading.remainingUsd),
              remainingText: usd(Number(reading.remainingUsd)),
              atEpoch: reading.at,
              enteredAtEpoch: reading.enteredAt,
              spentSince: sinceReading,
              spentSinceText: usd(sinceReading),
            }
          : null,
      }
    })
    const orphans = (credits.orphans as Doc[]).filter((orphan) => store.credits.has(orphan.id as string))
    next = { ...next, apiCredits: recountPool({ ...credits, accounts, orphans }, now) }
  }
  const credentials = ((next.credentials as Doc[] | undefined) ?? []).map((credential) => {
    const id = credential.id as string
    if (!store.touched.has(id)) return credential
    const entry = store.renewals.get(id)
    const setting = entry ? { date: entry.date, revision: String(entry.rev), updatedAtEpoch: entry.updatedAt } : null
    if (credential.renewalSource === "reported") return { ...credential, renewalSetting: setting }
    return entry
      ? { ...credential, renewalSetting: setting, renewalSource: "dashboard", renewalEstimated: false, renewalAtEpoch: nextRenewal(entry.date, now) }
      : { ...credential, renewalSetting: null, renewalSource: null, renewalEstimated: false, renewalAtEpoch: null }
  })
  const renewalOrphans = ((next.renewalOrphans as Doc[] | undefined) ?? []).filter((orphan) =>
    store.renewals.has(orphan.id as string),
  )
  return { ...next, credentials, renewalOrphans }
}

const MONTHLY_USD = /^[0-9]{1,7}(\.[0-9]{1,2})?$/
const STAMP = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/
const MAX_SETTINGS_BYTES = 4096

type CreditItem = {
  id: string
  baseRevision: string
  monthlyUsd: string | null
  renews: string | null
  reading: { remainingUsd: string; at: string } | null
}
type RenewalItem = { id: string; baseRevision: string; date: string | null }
type Batch = { kind: "apiCredits"; items: CreditItem[] } | { kind: "renewals"; items: RenewalItem[] }

const exactKeys = (value: unknown, keys: string[]): value is Record<string, unknown> =>
  value !== null &&
  typeof value === "object" &&
  !Array.isArray(value) &&
  Object.keys(value).length === keys.length &&
  keys.every((key) => key in value)
const nullableString = (value: unknown) => value === null || typeof value === "string"

/** A batch's shape, as ParseBatch in internal/overrides judges it; null for invalid_request. */
function parseBatch(raw: string): Batch | null {
  let body: unknown
  try {
    body = JSON.parse(raw)
  } catch {
    return null
  }
  if (!exactKeys(body, ["kind", "items"]) || !Array.isArray(body.items)) return null
  const seen = new Set<string>()
  const fresh = (item: Record<string, unknown>) => {
    if (typeof item.id !== "string" || item.id === "" || seen.has(item.id)) return false
    if (typeof item.baseRevision !== "string" || !/^(|0|[1-9][0-9]{0,19})$/.test(item.baseRevision)) return false
    seen.add(item.id)
    return true
  }
  if (body.kind === "apiCredits") {
    if (body.items.length === 0 || body.items.length > 16) return null
    for (const item of body.items) {
      if (!exactKeys(item, ["id", "baseRevision", "monthlyUsd", "renews", "reading"]) || !fresh(item)) return null
      if (!nullableString(item.monthlyUsd) || !nullableString(item.renews)) return null
      const reading = item.reading
      if (reading !== null && (!exactKeys(reading, ["remainingUsd", "at"]) || typeof reading.remainingUsd !== "string" || typeof reading.at !== "string")) {
        return null
      }
    }
    return body as Batch
  }
  if (body.kind === "renewals") {
    if (body.items.length === 0 || body.items.length > 32) return null
    for (const item of body.items) {
      if (!exactKeys(item, ["id", "baseRevision", "date"]) || !fresh(item) || !nullableString(item.date)) return null
    }
    return body as Batch
  }
  return null
}

/** The first value the save-time rules refuse, as {error, id, field}; null when every one passes. */
function checkBatch(batch: Batch, store: SettingsStore, doc: Doc, now: number): Doc | null {
  if (batch.kind === "renewals") {
    for (const item of batch.items) {
      if (item.date !== null && readDate(item.date).kind !== "ok") return { error: "invalid_date", id: item.id, field: "date" }
    }
    return null
  }
  const accounts = (((doc.apiCredits as Doc | null)?.accounts as Doc[] | undefined) ?? [])
  for (const item of batch.items) {
    const fail = (error: string, field: string) => ({ error, id: item.id, field })
    if (item.monthlyUsd !== null && !MONTHLY_USD.test(item.monthlyUsd)) return fail("invalid_monthly_usd", "monthlyUsd")
    if (item.renews !== null && readDate(item.renews).kind !== "ok") return fail("invalid_renews", "renews")
    const reading = item.reading
    if (reading === null) continue
    if (!MONTHLY_USD.test(reading.remainingUsd)) return fail("invalid_reading_amount", "reading.remainingUsd")
    const stored = store.credits.get(item.id)?.reading
    const at = STAMP.test(reading.at) ? Date.parse(reading.at) / 1000 : Number.NaN
    // A reading resent as stored is not judged against the clock again.
    if (stored && stored.remainingUsd === reading.remainingUsd && stored.at === at) continue
    if (!Number.isFinite(at) || at < now - READING_MAX_AGE || at > now + READING_MAX_AHEAD) return fail("invalid_reading_time", "reading.at")
    const account = accounts.find((one) => one.id === item.id)
    const renews = item.renews ?? ((account?.settings as Doc | undefined)?.configRenews as string | undefined) ?? ""
    const cycle = renews ? cycleOn(renews, now) : null
    if (cycle && at < cycle.start) return fail("reading_before_refill", "reading.at")
  }
  return null
}

/** Ids the served document does not offer for editing; an orphan may only be cleared. */
function notEditable(batch: Batch, doc: Doc): string[] {
  const credits = doc.apiCredits as Doc | null | undefined
  if (batch.kind === "apiCredits") {
    const editable = new Set(
      ((credits?.accounts as Doc[] | undefined) ?? []).filter((account) => (account.settings as Doc).editable).map((account) => account.id),
    )
    const orphans = new Set(((credits?.orphans as Doc[] | undefined) ?? []).map((orphan) => orphan.id))
    return batch.items
      .filter((item) => !editable.has(item.id) && !(orphans.has(item.id) && item.monthlyUsd === null && item.renews === null && item.reading === null))
      .map((item) => item.id)
  }
  const editable = new Set(((doc.credentials as Doc[] | undefined) ?? []).filter((credential) => credential.renewalEditable).map((credential) => credential.id))
  const orphans = new Set(((doc.renewalOrphans as Doc[] | undefined) ?? []).map((orphan) => orphan.id))
  return batch.items.filter((item) => !editable.has(item.id) && !(orphans.has(item.id) && item.date === null)).map((item) => item.id)
}

/** Whether an item asks for exactly what is stored now. */
function holds(store: SettingsStore, batch: Batch, item: CreditItem | RenewalItem): boolean {
  if (batch.kind === "renewals") {
    const entry = store.renewals.get(item.id)
    return (entry?.date ?? null) === (item as RenewalItem).date
  }
  const want = item as CreditItem
  const entry = store.credits.get(item.id)
  const reading = entry?.reading ?? null
  const sameReading =
    reading === null || want.reading === null
      ? reading === want.reading
      : reading.remainingUsd === want.reading.remainingUsd && reading.at === Date.parse(want.reading.at) / 1000
  return (entry?.monthlyUsd ?? null) === want.monthlyUsd && (entry?.renews ?? null) === want.renews && sameReading
}

/** Every answer a save can be given, for the warning that names them when ?save= is misspelled. */
const SAVE_ENDINGS = [
  "saved",
  "conflict",
  "unwritable",
  "full",
  "throttled",
  "not-editable",
  "unavailable",
  "switched-off",
  "invalid-monthly-usd",
  "invalid-reading-time",
  "dropped",
  "gateway",
]

/**
 * One save, after the door's own checks, in the plugin's order from step 6
 * (E.4): shape, values, editable, conflict, rate, commit. ?save= forces an
 * ending; "conflict" first writes another device's value to every row, and
 * "dropped" and "gateway" commit before losing the answer, which is why the
 * page must treat them as unknown.
 */
function saveSettings(raw: string, ending: string, store: SettingsStore, doc: Doc, now: number): Reply | "dropped" | "gateway" {
  if (raw.length > MAX_SETTINGS_BYTES) return pluginJSON(400, { error: "invalid_request" })
  const batch = parseBatch(raw)
  if (batch === null) return pluginJSON(400, { error: "invalid_request" })
  if (ending === "unavailable") return pluginJSON(503, { error: "settings_unavailable" })
  const first = batch.items[0]!
  if (ending === "invalid-monthly-usd") return pluginJSON(400, { error: "invalid_monthly_usd", id: first.id, field: batch.kind === "renewals" ? "date" : "monthlyUsd" })
  if (ending === "invalid-reading-time") return pluginJSON(400, { error: "invalid_reading_time", id: first.id, field: "reading.at" })
  const refused = checkBatch(batch, store, doc, now)
  if (refused) return pluginJSON(400, refused)
  const locked = ending === "not-editable" ? batch.items.map((item) => item.id) : notEditable(batch, doc)
  if (locked.length > 0) return pluginJSON(409, { error: "not_editable", ids: locked })

  if (ending === "conflict") {
    // Another device saves first.
    store.revision++
    for (const item of batch.items) {
      store.touched.add(item.id)
      if (batch.kind === "renewals") store.renewals.set(item.id, { date: "2026-12-01", rev: store.revision, updatedAt: now })
      else store.credits.set(item.id, { ...(store.credits.get(item.id) ?? { renews: null, reading: null }), monthlyUsd: "275", rev: store.revision, updatedAt: now })
    }
  }
  const revOf = (id: string) => {
    const entry = batch.kind === "renewals" ? store.renewals.get(id) : store.credits.get(id)
    return entry ? String(entry.rev) : ""
  }
  const conflicts = batch.items.filter((item) => item.baseRevision !== revOf(item.id) && !holds(store, batch, item)).map((item) => item.id)
  if (conflicts.length > 0) {
    const served = applyStore(doc, store, now)
    const current: Doc = {}
    for (const id of conflicts) {
      if (batch.kind === "renewals") {
        current[id] = ((served.credentials as Doc[]).find((credential) => credential.id === id)?.renewalSetting as Doc | undefined) ?? null
      } else {
        current[id] = (((served.apiCredits as Doc).accounts as Doc[]).find((account) => account.id === id)?.settings as Doc | undefined) ?? null
      }
    }
    return pluginJSON(409, { error: "conflict", revision: String(store.revision), conflicts, current })
  }
  if (batch.items.every((item) => holds(store, batch, item))) {
    return pluginJSON(200, { ok: true, unchanged: true, revision: String(store.revision) })
  }
  if (ending === "throttled") return { ...pluginJSON(429, { error: "too_many_writes" }), headers: { ...pluginJSON(429, {}).headers, "Retry-After": "60" } }
  if (ending === "full") return pluginJSON(409, { error: "settings_full" })
  if (ending === "unwritable") return pluginJSON(503, { error: "settings_unwritable" })

  store.revision++
  for (const item of batch.items) {
    store.touched.add(item.id)
    if (batch.kind === "renewals") {
      const date = (item as RenewalItem).date
      if (date === null) store.renewals.delete(item.id)
      else store.renewals.set(item.id, { date, rev: store.revision, updatedAt: now })
      continue
    }
    const want = item as CreditItem
    if (want.monthlyUsd === null && want.renews === null && want.reading === null) {
      store.credits.delete(item.id)
      continue
    }
    const kept = store.credits.get(item.id)?.reading ?? null
    const at = want.reading ? Date.parse(want.reading.at) / 1000 : 0
    const reading = want.reading
      ? kept && kept.remainingUsd === want.reading.remainingUsd && kept.at === at
        ? kept
        : { remainingUsd: want.reading.remainingUsd, at, enteredAt: now }
      : null
    store.credits.set(item.id, { monthlyUsd: want.monthlyUsd, renews: want.renews, reading, rev: store.revision, updatedAt: now })
  }
  if (ending === "dropped" || ending === "gateway") return ending
  const served = applyStore(doc, store, now)
  const settings: Doc = {}
  for (const item of batch.items) {
    settings[item.id] =
      batch.kind === "renewals"
        ? (((served.credentials as Doc[]).find((credential) => credential.id === item.id)?.renewalSetting as Doc | undefined) ?? null)
        : ((((served.apiCredits as Doc).accounts as Doc[]).find((account) => account.id === item.id)?.settings as Doc | undefined) ?? null)
  }
  return pluginJSON(200, { ok: true, unchanged: false, revision: String(store.revision), settings })
}

export interface FixtureOptions {
  /**
   * How long a press takes to answer. Slow enough by default to see the button
   * settle into its pending state, which is the half of this interaction a
   * static review cannot check; the tests set it to zero.
   */
  pressDelayMs?: number
}

export function goldenFixtureRoute(options: FixtureOptions = {}): Plugin {
  const pressDelayMs = options.pressDelayMs ?? 700
  // Fixed for the life of the dev server. See rebase().
  let offset: number | null = null
  // Each scenario's settings, for the life of the dev server, as the plugin
  // keeps settings.json across reloads. Restarting the dev server forgets them.
  const stores = new Map<string, SettingsStore>()

  /**
   * A scenario's document as the summary route serves it: rebased unless
   * ?rebase=0, with what saves here have written laid over it.
   */
  const serve = (name: string, result: Doc, rebased: boolean): Doc => {
    offset ??= nowSeconds() - (result.generatedAtEpoch as number)
    const doc = rebased ? (rebase(result, offset) as Doc) : result
    let store = stores.get(name)
    if (!store) {
      store = seedStore(doc)
      stores.set(name, store)
    }
    return applyStore(doc, store, nowSeconds())
  }

  return {
    name: "quota-glance:golden-fixture-route",
    apply: "serve",
    configureServer(server) {
      const warn = (message: string) => server.config.logger.warn(`[quota-glance fixture] ${message}`, { timestamp: true })

      // CPA's management sign-in, for this one address: the dev server's
      // whole audience. Five counted failures ban it for thirty minutes, a
      // success clears the count, and a ban outlasts every scenario. Restarting
      // the dev server lifts it, which is the one thing CPA's own ban does not
      // allow.
      let failures = 0
      let bannedUntil = 0

      /** CPA's middleware: true when the request may go on to the plugin. */
      const admit = (req: IncomingMessage, res: ServerResponse, forced: CPARefusal | null): boolean => {
        res.setHeader("X-CPA-VERSION", "dev")
        const refuse = (status: number, error: string) => {
          res.statusCode = status
          res.setHeader("Content-Type", "application/json; charset=utf-8")
          res.end(JSON.stringify({ error }))
          return false
        }
        const now = Date.now()
        if (bannedUntil > now) {
          return refuse(403, `IP banned due to too many failed attempts. Try again in ${goDuration(bannedUntil - now)}`)
        }
        if (bannedUntil !== 0) {
          bannedUntil = 0
          failures = 0
        }
        const key = presentedKey(req)
        const refusal: CPARefusal | null =
          forced === "banned" || forced === "remote" ? forced : key === "" ? "missing" : forced
        if (refusal === null) {
          failures = 0
          return true
        }
        const { status, error, counted } = CPA_REFUSALS[refusal]
        if (counted) {
          failures++
          warn(`CPA would count this as a failed management sign-in (${error}): ${failures} of 5`)
          if (failures >= 5) {
            bannedUntil = now + 30 * 60 * 1000
            failures = 0
            warn("CPA would now ban this address from management for 30 minutes; restart the dev server to lift it")
          }
        }
        return refuse(status, error)
      }

      // The press ledger: each press id's answer, so a second copy of a press
      // is answered with the first copy's bytes and X-Quota-Glance-Replayed,
      // and spends nothing — as internal/api/ledger.go does.
      const ledger = new Map<string, { credentialId: string; reply: Promise<Reply>; finishedAt: number | null }>()

      /**
       * Answers a press that got past every gate. Spends nothing: replies with
       * whichever ending ?redeem= names, after a delay, so the dialog, the
       * outcome line and every failure message can be reviewed without a
       * Codex or Claude account.
       */
      const press = (res: ServerResponse, { credentialId, pressId }: Press, ending: string): void => {
        // The plugin names the provider it spoke to, and the outcome line is
        // worded from it, so the answer has to match the credential pressed.
        const provider = credentialId.startsWith("claude-") ? "claude" : "codex"
        const now = Date.now()
        for (const [id, entry] of ledger) {
          if (entry.finishedAt !== null && now - entry.finishedAt >= LEDGER_TTL_MS) ledger.delete(id)
        }
        let settle: (reply: Reply) => void = () => undefined
        if (pressId !== null) {
          const held = ledger.get(pressId)
          if (held) {
            // The same press id naming another credential is a broken client
            // or a forgery, and either way not a spend.
            if (held.credentialId !== credentialId) return json(res, 400, { error: "invalid_request" })
            void held.reply.then((reply) => send(res, reply, { "X-Quota-Glance-Replayed": "1" }))
            return
          }
          const entry = {
            credentialId,
            reply: new Promise<Reply>((resolve) => (settle = resolve)),
            finishedAt: null as number | null,
          }
          ledger.set(pressId, entry)
          const resolve = settle
          settle = (reply) => {
            entry.finishedAt = Date.now()
            resolve(reply)
          }
        }
        if (!ENDINGS.includes(ending)) warn(`unknown ending ${ending}, answering reset; try one of: ${ENDINGS.join(", ")}`)
        setTimeout(() => {
          const reply = pressReply(ending, provider)
          settle(reply)
          if (ending === "dropped") {
            // The connection drops with no answer at all, which the page has
            // to report as unknown rather than as nothing spent.
            res.destroy()
          } else if (ending === "gateway") {
            // A reverse proxy giving up on a slow press: its own page, no code.
            res.statusCode = 504
            res.setHeader("Content-Type", "text/html")
            res.end("<html><body>504 Gateway Time-out</body></html>")
          } else {
            send(res, reply)
          }
        }, pressDelayMs)
      }

      // Every press path, every method. Registered before Vite's own
      // middleware, the proxy among them, so nothing here is ever forwarded.
      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://localhost")
        if (!PRESS_PATHS.has(url.pathname)) return next()
        const method = (req.method ?? "GET").toUpperCase()
        const ending = url.searchParams.get("redeem") ?? "reset"

        if (url.pathname === RESOURCE_REDEEM_PATH) {
          // CPA dispatches only GET to a resource route, and the plugin has no
          // GET /redeem: the page's old resource POST ends at CPA's bare 404.
          return method === "GET" ? json(res, 404, { error: "not_found" }) : bare(res, 404, false)
        }

        if (url.pathname === SPEND_PATH) {
          if (method !== "GET") return bare(res, 404, false)
          const doorEnding = ending in CPA_ENDINGS ? "reset" : ending
          if (doorEnding !== ending) warn(`ending ${ending} is CPA refusing a console session; the token door never meets it`)
          // In the plugin's order: the fetch-metadata gate, early data, the
          // password, the switch, the header. None of them costs anything.
          if (crossSite(req.headers) || ending === "cross-site") return json(res, 403, { error: "cross_site" })
          if (req.headers["early-data"] !== undefined || ending === "too-early") return json(res, 425, { error: "too_early" })
          // The plugin's refusal of a wrong password is a bare 401.
          if (presentedToken(req) !== DEV_TOKEN) return bare(res, 401, true)
          if (ending === "switched-off") return json(res, 404, { error: "not_found" })
          const parsed = pressFromHeader(req.headers["x-quota-glance-spend"])
          if (typeof parsed === "string") return json(res, 400, { error: parsed })
          return press(res, parsed, doorEnding)
        }

        // The console door: CPA's sign-in first, then the plugin.
        if (!admit(req, res, CPA_ENDINGS[ending] ?? null)) return
        if (method !== "POST") return bare(res, 404, false)
        const doorEnding = GATE_ENDINGS.has(ending) ? "reset" : ending
        if (doorEnding !== ending) warn(`ending ${ending} is the token door's gate; the console door never meets it`)
        if (ending === "switched-off") return json(res, 404, { error: "not_found" })
        const media = (req.headers["content-type"] ?? "").split(";")[0]?.trim().toLowerCase() ?? ""
        if (media !== "" && media !== "application/json") return json(res, 415, { error: "unsupported_media_type" })

        let raw = ""
        req.setEncoding("utf8")
        req.on("data", (chunk: string) => (raw += chunk))
        req.on("end", () => {
          if (raw.length > MAX_PRESS_BYTES) return json(res, 400, { error: "invalid_request" })
          const parsed = parsePress(raw, false)
          if (typeof parsed === "string") return json(res, 400, { error: parsed })
          press(res, parsed, doorEnding)
        })
      })

      // Both settings paths, every method. Registered before Vite's own
      // middleware, the proxy among them, so no save is ever forwarded.
      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://localhost")
        if (!SETTINGS_PATHS.has(url.pathname)) return next()
        const method = (req.method ?? "GET").toUpperCase()
        const ending = url.searchParams.get("save") ?? "saved"
        if (!SAVE_ENDINGS.includes(ending)) warn(`unknown save ending ${ending}; try one of: ${SAVE_ENDINGS.join(", ")}`)
        const name = url.searchParams.get("scenario") ?? "golden"
        const pick = scenarios()[name]
        const result = pick ? pick() : null
        const doc = result && typeof result === "object" ? serve(name, result, url.searchParams.get("rebase") !== "0") : null

        // The plugin's step 4: editing switched off is a 404 that says
        // nothing more, and settings.json unreadable a 503.
        const gate = (): boolean => {
          const editing = (doc?.apiCredits as Doc | null | undefined)?.editing as Doc | undefined
          if (doc === null || ending === "switched-off" || editing?.reason === "disabled") {
            json(res, 404, { error: "not_found" })
            return false
          }
          if (editing?.reason === "settingsUnreadable") {
            json(res, 503, { error: "settings_unavailable" })
            return false
          }
          return true
        }
        const answer = (raw: string) => {
          const reply = saveSettings(raw, ending, stores.get(name)!, doc!, nowSeconds())
          setTimeout(() => {
            if (reply === "dropped") {
              // The save landed and its answer was lost: the page must say
              // it may or may not have saved.
              res.destroy()
            } else if (reply === "gateway") {
              res.statusCode = 504
              res.setHeader("Content-Type", "text/html")
              res.end("<html><body>504 Gateway Time-out</body></html>")
            } else {
              send(res, reply)
            }
          }, pressDelayMs)
        }

        if (url.pathname === SAVE_SETTINGS_PATH) {
          if (method !== "GET") return bare(res, 404, false)
          // In the plugin's order: the fetch-metadata gate, early data, the
          // password, the switch, the header.
          if (crossSite(req.headers)) return json(res, 403, { error: "cross_site" })
          if (req.headers["early-data"] !== undefined) return json(res, 425, { error: "too_early" })
          if (presentedToken(req) !== DEV_TOKEN) return bare(res, 401, true)
          if (!gate()) return
          const raw = headerJSON(req.headers["x-quota-glance-settings"], MAX_SETTINGS_BYTES)
          if (raw === null) return json(res, 400, { error: "invalid_request" })
          return answer(raw)
        }

        // The console door: CPA's sign-in first, then the plugin.
        if (!admit(req, res, null)) return
        if (method !== "POST") return bare(res, 404, false)
        if (!gate()) return
        const media = (req.headers["content-type"] ?? "").split(";")[0]?.trim().toLowerCase() ?? ""
        if (media !== "" && media !== "application/json") return json(res, 415, { error: "unsupported_media_type" })
        let raw = ""
        req.setEncoding("utf8")
        req.on("data", (chunk: string) => (raw += chunk))
        req.on("end", () => answer(raw))
      })

      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://localhost")
        const viaCPA = url.pathname === MANAGEMENT_PATH
        if (!viaCPA && url.pathname !== RESOURCE_PATH) return next()

        const all = scenarios()
        const name = url.searchParams.get("scenario") ?? "golden"
        const pick = all[name]
        if (!pick) {
          res.statusCode = 404
          res.end(`unknown scenario ${name}; try one of: ${Object.keys(all).join(", ")}, expiring`)
          return
        }

        if (viaCPA) {
          // CPA's middleware answers a missing, wrong or banned key in JSON,
          // in its own words, before the plugin runs.
          if (!admit(req, res, CPA_SCENARIOS[name] ?? null)) return
        } else if (name === "unauthorized" || name === "token-refused" || presentedToken(req) !== DEV_TOKEN) {
          // The plugin's own refusal of the dashboard password: a bare 401,
          // as tokenRefusal in internal/api/api.go answers it.
          return bare(res, 401, true)
        }

        const result = pick()
        if (result === "unauthorized") return bare(res, 401, true)
        if (result === "down") return bare(res, 503, true)

        let doc = serve(name, result, url.searchParams.get("rebase") !== "0")
        const runsOutIn = Number(url.searchParams.get("expiring"))
        if (Number.isFinite(runsOutIn) && runsOutIn > 0) doc = expiring(doc, runsOutIn)

        const body = `${JSON.stringify(doc, null, 2)}\n`
        const etag = `"${createHash("sha256").update(body).digest("hex")}"`

        res.setHeader("Content-Type", "application/json; charset=utf-8")
        res.setHeader("Cache-Control", "no-store")
        res.setHeader("ETag", etag)
        if (req.headers["if-none-match"] === etag) {
          res.statusCode = 304
          res.end()
          return
        }
        res.statusCode = 200
        res.end(body)
      })
    },
  }
}
