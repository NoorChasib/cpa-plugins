import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import type { IncomingMessage, ServerResponse } from "node:http"
import { fileURLToPath } from "node:url"
import type { Plugin } from "vite"

// A stand-in for the plugin's routes, backed by the committed golden fixtures.
// It exists so the app can be developed and reviewed without a running CPA,
// and it lives in the dev server rather than in the bundle: the shell is served
// unauthenticated, so a fixture full of addresses must never reach it.
//
// What it copies from the real routes, because the page depends on it: the
// paths, ETag revalidation with 304, no-store caching, and the exact answers a
// refusal comes back with. What it adds, because a fixture cannot: scenarios
// for the states the design has to survive, and endings for every way a press
// can turn out.
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
export const DEV_TOKEN = "dev-token"

const fixture = (name: string): string =>
  fileURLToPath(new URL(`../../testdata/golden/${name}.json`, import.meta.url))

/** Every epoch field in the document, so rebasing touches those and nothing else. */
const EPOCH_FIELDS = new Set([
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

/** The spend header, decoded as the plugin decodes it, or the error code to answer with. */
function pressFromHeader(value: string | string[] | undefined): Press | string {
  if (typeof value !== "string" || value.length > MAX_PRESS_BYTES) return "invalid_request"
  // Unpadded base64url and nothing else. Node's decoder skips what it does not
  // understand, and the plugin's refuses it.
  if (!/^[A-Za-z0-9_-]*$/.test(value) || value.length % 4 === 1) return "invalid_request"
  let raw: string
  try {
    raw = new TextDecoder("utf-8", { fatal: true }).decode(Buffer.from(value, "base64url"))
  } catch {
    return "invalid_request"
  }
  if (!raw.trimStart().startsWith("{")) return "invalid_request"
  return parsePress(raw, true)
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

        offset ??= nowSeconds() - (result.generatedAtEpoch as number)
        let doc = url.searchParams.get("rebase") === "0" ? result : (rebase(result, offset) as Doc)
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
