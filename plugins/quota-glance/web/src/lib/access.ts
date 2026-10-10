// Which credential this page may present, and the three requests that present
// one: reading the document, spending a banked reset, and saving settings.
//
// There are two ways in. The dashboard password (`web-token`, the token door)
// goes to this plugin's own resource routes, which CPA does not authenticate;
// a wrong one costs nothing but the plugin's own limiter. The CPA console's
// management key (the console door) goes to the management routes, and there
// every refusal counts toward CPA's ban: five failed management sign-ins lock
// the address out of the whole Management API for thirty minutes, a missing key
// counts, and localhost is not exempt. Everything below is shaped by that
// asymmetry. The token is tried first, a console key is presented only when it
// passes a strict reading of the console's own storage, and the first refusal
// of a key latches it out of use, in storage, before anything else can run.
//
// This file is a leaf on purpose: no imports, no DOM types, and nothing a type
// stripper cannot erase. The browser glue lives in session.ts; this is what the
// node tests under web/test load and drive, so the rules that keep CPA's
// counter at zero are exercised as written rather than restated in a test.

export const PLUGIN_ID = "quota-glance"

/** The dashboard password, saved by token.ts. */
export const TOKEN_KEY = "quota-glance.token"
/** The console latch: a refused key, fingerprinted. See latchConsole. */
export const LATCH_KEY = "quota-glance.console-refused"
/** Where the console keeps its session: the modern entry and the legacy ones. */
export const CONSOLE_KEYS = ["cli-proxy-auth", "managementKey", "isLoggedIn", "apiBase", "apiUrl"] as const
/** Every storage key whose change can open or close a door. */
export const WATCHED_KEYS: readonly string[] = [...CONSOLE_KEYS, TOKEN_KEY, LATCH_KEY]
/**
 * The same-document change signal. A `storage` event reaches only the other
 * documents on the origin, so a change made here is announced with this.
 */
export const ACCESS_CHANGED = "quota-glance:access"

/** Carries a press on the token door. Mirrors the POST body. */
export const SPEND_HEADER = "X-Quota-Glance-Spend"
/** Marks an answer the plugin's press ledger handed back for an earlier copy. */
export const REPLAYED_HEADER = "X-Quota-Glance-Replayed"
/** Carries a settings batch on the token door. Mirrors the POST body. */
export const SETTINGS_HEADER = "X-Quota-Glance-Settings"

/** A read gives up after this, so a hung plugin surfaces as a failed poll. */
export const READ_TIMEOUT_MS = 20_000
/**
 * A spend gives up after this: longer than the plugin's own 55-second bound on
 * the whole exchange, because an answer the plugin was about to give —
 * "outcome unknown" included — must not arrive at a closed connection.
 */
export const SPEND_TIMEOUT_MS = 65_000
/**
 * A save gives up after this. The plugin writes one small file and rebuilds
 * the document before it answers, so anything longer is a hung plugin.
 */
export const SAVE_TIMEOUT_MS = 30_000
/** How long a press id is reused for the same credential after a lost answer. */
export const PRESS_REUSE_SECONDS = 600
/** CPA's ban, for a refusal that says it is banned but not for how long. */
export const DEFAULT_BAN_SECONDS = 1800
/** How often the document is polled while some door is open. */
export const POLL_MS = 60_000

const STORAGE_CAP = 32_768
const KEY_CAP = 4096
const BASE_CAP = 2048

/** The few storage calls this file makes. */
export interface StorageLike {
  getItem(key: string): string | null
  /**
   * Must not lose a latch: a browser that refuses the write (a full quota,
   * some private modes) has to keep the value in memory rather than throw,
   * or a refused key would be presented again on the next poll.
   */
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

/** Everything about where this page is that the rules depend on. */
export interface Env {
  storage: StorageLike
  /** `location.origin`. */
  origin: string
  /** `location.host`, one half of the console's obfuscation key. */
  host: string
  /** `location.pathname`, which carries any reverse-proxy prefix. */
  pathname: string
  /** `navigator.userAgent`, the other half of that key. */
  userAgent: string
  /** Epoch seconds. */
  now(): number
}

export type DoorKind = "token" | "console"
export type Door = { kind: "token"; token: string } | { kind: "console"; key: string }

/** Why the console latch holds a key, in the words the copy is keyed on. */
export type ConsoleReason = "refused" | "banned" | "remote" | "off" | "other"
/** Why a door is shut: the token was refused, or the console key was. */
export type Refusal = "token" | ConsoleReason

export interface ConsoleLatch {
  v: 1
  /** fingerprint() of the refused key, so only that key is held back. */
  fp: string
  reason: ConsoleReason
  /** The HTTP status that refused it; 0 when the record was unreadable. */
  status: number
  /** When it was refused, epoch seconds. */
  at: number
  /** For a ban, when CPA said it would lift; otherwise null. */
  until: number | null
}

/** A refusal, by the plugin's or CPA's own words. See classifyRefusal. */
export type RefusalCode =
  | "console_refused"
  | "ip_banned"
  | "remote_disabled"
  | "management_off"
  | "refused_other"
  | "token_refused"
  | "cross_site"
  | "too_early"
  | "not_found"
  | "route_missing"

export interface Classified {
  code: RefusalCode
  /** Which latch this refusal sets, if any. */
  latch: Refusal | null
  /** For a ban: how long CPA said it lasts, else null. */
  banSeconds: number | null
}

const CONSOLE_REASONS: readonly ConsoleReason[] = ["refused", "banned", "remote", "off", "other"]

/** The code a console latch reports, for a press refused before it was sent. */
const CODE_OF_REASON: Record<ConsoleReason, RefusalCode> = {
  refused: "console_refused",
  banned: "ip_banned",
  remote: "remote_disabled",
  off: "management_off",
  other: "refused_other",
}

/**
 * Every `error` the plugin answers a press or a save with, on either door:
 * Handle, redeemResponse, spendResponse, spend and redeemError in
 * internal/api/api.go, parsePress's confirmation_required, and the settings
 * doors in internal/api/settings.go. A body that carries one of these is the
 * plugin's own answer. A body that carries anything else came from something
 * in front of the plugin — a gateway that writes its errors as JSON among
 * them — and says nothing about whether the plugin acted.
 */
export const PLUGIN_ERRORS: ReadonlySet<string> = new Set([
  "disabled",
  "not_found",
  "unsupported_media_type",
  "invalid_request",
  "confirmation_required",
  "cross_site",
  "too_early",
  "already_in_flight",
  "not_redeemable",
  "credential_unusable",
  "retry_window_closed",
  "outcome_unknown",
  "provider_rate_limited",
  "provider_refused",
  "provider_unavailable",
  // Saving settings.
  "settings_unavailable",
  "settings_unwritable",
  "settings_full",
  "conflict",
  "not_editable",
  "too_many_writes",
  "invalid_monthly_usd",
  "invalid_renews",
  "invalid_date",
  "invalid_reading_amount",
  "invalid_reading_time",
  "reading_before_refill",
])

/**
 * Neither way in worked, or there was none to try.
 *
 * `hadCredential` distinguishes "nothing to try" — no console session and no
 * saved password — from "what we had was refused", which is the difference
 * between asking the reader to sign in and telling them their credential
 * stopped working. `reason` says which credential and why, so the sign-in
 * screen can say what to do about it; null when there was nothing.
 */
export class NoSessionError extends Error {
  readonly hadCredential: boolean
  readonly reason: Refusal | null
  constructor(hadCredential: boolean, reason: Refusal | null = null) {
    super(hadCredential ? "credentials rejected" : "no credentials")
    this.name = "NoSessionError"
    this.hadCredential = hadCredential
    this.reason = reason
  }
}

/**
 * A console request got no answer at all: the connection dropped, the
 * deadline passed, or something in front of CPA answered with a redirect,
 * which this page never follows.
 *
 * CPA may still have read the key and counted it, so the key is held back
 * from every later read in this document until the reader asks again (see
 * Access.unanswered). Not a NoSessionError: nothing says the key was refused,
 * so the reader is not sent to sign in.
 */
export class ConsoleUnansweredError extends Error {
  constructor() {
    super("the console session's last request got no answer")
    this.name = "ConsoleUnansweredError"
  }
}

const isObject = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === "object" && !Array.isArray(value)

/**
 * The header value, assembled from parts so the built page never contains a
 * literal bearer-credential string — the precaution web_test.go relies on to
 * assert the bundle carries no token.
 */
export function bearer(value: string): string {
  return ["Bearer", value].join(" ")
}

/**
 * Whatever sits in front of this page's path: a reverse proxy's mount point,
 * or nothing. The page is served from its own plugin's route on either tree;
 * on the dev server it is served from `/`, and the prefix is empty.
 */
export function pagePrefix(pathname: string): string {
  const path = pathname.replace(/\/+$/, "")
  for (const tree of ["resource", "management"]) {
    const at = path.indexOf(`/v0/${tree}/plugins/${PLUGIN_ID}`)
    if (at >= 0) return path.slice(0, at)
  }
  return ""
}

/** The origin and prefix every path this page builds starts from. */
export function rootOf(env: Env): string {
  return `${env.origin}${pagePrefix(env.pathname)}`
}

/**
 * Whether the console's API base is this page's own CPA.
 *
 * The console can be pointed at any server, and its storage is per origin, not
 * per server: a key it remembers for another CPA must never be presented to
 * this one, where it is simply a wrong key and a counted failure. The console
 * normalises its base by trimming slashes and a `/v8/management` suffix; this
 * accepts that and the `/v0/management` form, and nothing looser.
 */
export function matchesRoot(value: unknown, env: Env): boolean {
  if (typeof value !== "string" || value.length === 0 || value.length > BASE_CAP) return false
  if (!/^https?:\/\//i.test(env.origin)) return false
  const trimmed = value.trim()
  if (!/^https?:\/\//i.test(trimmed) || /[?#]/.test(trimmed) || /^https?:\/\/[^/]*@/i.test(trimmed)) return false
  try {
    const url = new URL(trimmed)
    if (url.username || url.password || url.search || url.hash) return false
    const path = url.pathname
      .replace(/\/+$/, "")
      .replace(/\/v[08]\/management$/i, "")
      .replace(/\/+$/, "")
    return url.origin === env.origin && path === pagePrefix(env.pathname)
  } catch {
    return false
  }
}

/**
 * Whether a value can be sent as a bearer credential exactly as stored.
 *
 * Anything the Headers class would trim, fold or refuse is refused here
 * instead, before it can become a request that fails in transit — or worse, a
 * request carrying a different value than the one that was checked.
 */
export function validKey(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > KEY_CAP) return false
  // Control characters, and anything outside Latin-1, which a header cannot
  // carry. Engines differ on which of these Headers itself rejects.
  if (value.trim() !== value || /[\x00-\x1f\x7f]/.test(value) || /[^\x00-\xff]/.test(value)) return false
  try {
    const header = bearer(value)
    return new Headers({ Authorization: header }).get("Authorization") === header
  } catch {
    return false
  }
}

function stored(env: Env, name: string): string | null {
  const value = env.storage.getItem(name)
  if (value !== null && value.length > STORAGE_CAP) throw new RangeError(`${name} is too large to be a session`)
  return value
}

/**
 * The console's storage codec: an `enc::v1::` value is base64 of the JSON
 * XORed with a key built from a fixed salt, this host and this browser's user
 * agent. Reversible obfuscation, not encryption, and not a security boundary.
 *
 * Strict by default: bytes that are not UTF-8 — what a blob written under
 * another user agent decodes to — or text that is not JSON throw, and the
 * caller reads that as no session. `loose` is for the legacy entries, which
 * were sometimes plain strings.
 */
function decode(raw: string | null, env: Env, loose: boolean): unknown {
  if (raw === null) return null
  let value = raw
  if (value.startsWith("enc::v1::")) {
    const binary = atob(value.slice("enc::v1::".length))
    const salt = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${env.host}|${env.userAgent}`)
    const bytes = new Uint8Array(binary.length)
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i) ^ (salt[i % salt.length] ?? 0)
    value = new TextDecoder("utf-8", { fatal: true }).decode(bytes)
  }
  try {
    return JSON.parse(value)
  } catch (error) {
    if (loose) return value
    throw error
  }
}

/**
 * The console's management key, if this browser holds one this page may use.
 *
 * Ported from codex-catalog-filter's reader, which audited the console's
 * storage format. The modern `cli-proxy-auth` entry is authoritative whenever
 * it exists — after a logout, with Remember password off, or unreadable — and
 * the legacy entries are never consulted beside it: the legacy key surviving a
 * modern logout is exactly the stale credential that used to be presented on
 * every poll. The key is accepted only when the console remembered it on
 * purpose, still counts itself signed in, and was talking to this CPA.
 */
export function readConsoleKey(env: Env): string | null {
  try {
    const modern = stored(env, "cli-proxy-auth")
    if (modern !== null) {
      const record = decode(modern, env, false)
      if (!isObject(record) || record.version !== 0) return null
      const state = record.state
      if (!isObject(state)) return null
      if (state.rememberPassword !== true || state.isAuthenticated === false) return null
      if (!matchesRoot(state.apiBase, env) || !validKey(state.managementKey)) return null
      return state.managementKey
    }
    if (stored(env, "isLoggedIn") !== "true") return null
    const base = decode(stored(env, "apiBase"), env, true) || decode(stored(env, "apiUrl"), env, true)
    const key = decode(stored(env, "managementKey"), env, true)
    return matchesRoot(base, env) && validKey(key) ? key : null
  } catch {
    return null
  }
}

/**
 * FNV-1a, 64-bit, as hex.
 *
 * Not a secrecy primitive: it only has to tell one key from another. It is used
 * rather than a SHA because crypto.subtle does not exist on an insecure origin,
 * and the menu bar app commonly loads this page over plain HTTP on a LAN or
 * tailnet address.
 */
export function fnv1a64(text: string): string {
  let hash = 0xcbf29ce484222325n
  for (const byte of new TextEncoder().encode(text)) {
    hash ^= BigInt(byte)
    hash = (hash * 0x100000001b3n) & 0xffffffffffffffffn
  }
  return hash.toString(16).padStart(16, "0")
}

/** Which key a latch holds back: the key, bound to this page's own CPA. */
export function fingerprint(env: Env, key: string): string {
  return fnv1a64(`${rootOf(env)}\n${key}`)
}

function parseLatch(raw: string): ConsoleLatch | null {
  if (raw.length > STORAGE_CAP) return null
  try {
    const value: unknown = JSON.parse(raw)
    if (!isObject(value) || value.v !== 1 || typeof value.fp !== "string") return null
    const reason = CONSOLE_REASONS.find((one) => one === value.reason)
    if (reason === undefined || typeof value.status !== "number" || typeof value.at !== "number") return null
    if (value.until !== null && typeof value.until !== "number") return null
    return { v: 1, fp: value.fp, reason, status: value.status, at: value.at, until: value.until }
  } catch {
    return null
  }
}

/**
 * The latch on the console key this browser holds now, or null.
 *
 * Pruned as it is read: a latch for a key the console no longer holds — signed
 * out, or signed in again with another — is deleted, so it lives exactly as
 * long as the refused key it shadows and stores nothing the console's own
 * entry does not. A record this page cannot read holds back whatever key is
 * there, because the safe reading of an unknown latch is that it is one.
 */
export function consoleLatch(env: Env, key: string | null = readConsoleKey(env)): ConsoleLatch | null {
  let raw: string | null
  try {
    raw = env.storage.getItem(LATCH_KEY)
  } catch {
    return null
  }
  if (raw === null) return null
  const prune = () => {
    try {
      env.storage.removeItem(LATCH_KEY)
    } catch {
      /* the next read prunes it */
    }
  }
  if (key === null) {
    prune()
    return null
  }
  const fp = fingerprint(env, key)
  const latch = parseLatch(raw)
  if (latch === null) return { v: 1, fp, reason: "other", status: 0, at: 0, until: null }
  if (latch.fp !== fp) {
    prune()
    return null
  }
  return latch
}

/** The console key, when it passes the strict reader and is not latched. */
export function usableConsoleKey(env: Env): string | null {
  const key = readConsoleKey(env)
  return key !== null && consoleLatch(env, key) === null ? key : null
}

/**
 * Holds a refused key back, in storage, so every document on this origin —
 * this one after a reload, other tabs, the sidebar — stops presenting it.
 *
 * Synchronous on purpose. It runs the moment the refusal is seen and before
 * whoever asked is handed anything, so no caller can resume into a state where
 * the key still looks usable.
 */
export function latchConsole(
  env: Env,
  key: string,
  reason: ConsoleReason,
  status: number,
  banSeconds: number | null = null,
): ConsoleLatch {
  const at = env.now()
  const latch: ConsoleLatch = {
    v: 1,
    fp: fingerprint(env, key),
    reason,
    status,
    at,
    until: reason === "banned" ? at + (banSeconds ?? DEFAULT_BAN_SECONDS) : null,
  }
  try {
    env.storage.setItem(LATCH_KEY, JSON.stringify(latch))
  } catch {
    /* StorageLike promises not to throw here; see setItem */
  }
  return latch
}

/** Whether the latch is a ban CPA has not yet lifted, when a retry is pointless. */
export function retryBlocked(latch: ConsoleLatch | null, now: number): boolean {
  return latch !== null && latch.reason === "banned" && latch.until !== null && now < latch.until
}

/**
 * Lifts the latch, for an explicit "Try it again" and nothing else. Refused
 * while CPA's ban has not lifted: a key presented then can only extend it.
 * Returns whether the latch is gone.
 */
export function forgiveConsole(env: Env): boolean {
  if (retryBlocked(consoleLatch(env), env.now())) return false
  try {
    env.storage.removeItem(LATCH_KEY)
  } catch {
    return false
  }
  return true
}

/**
 * Seconds until CPA lifts a ban, from its own message — "IP banned due to too
 * many failed attempts. Try again in 29m40s", a Go duration rounded to the
 * second — or null when the message does not say.
 */
export function parseBanSeconds(text: string): number | null {
  const match = /Try again in ((?:\d+h)?(?:\d+m)?(?:\d+(?:\.\d+)?s)?)\s*$/.exec(text)
  if (!match || match[1] === "" || match[1] === undefined) return null
  const parts = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(match[1])
  if (!parts) return null
  const seconds = Number(parts[1] ?? 0) * 3600 + Number(parts[2] ?? 0) * 60 + Number(parts[3] ?? 0)
  // A day at most. CPA's ban is thirty minutes; a figure past a day is not
  // CPA's, and would leave the retry button disabled for no reason.
  return Number.isFinite(seconds) ? Math.min(Math.ceil(seconds), 86_400) : null
}

/**
 * What a refusal means, by door, status and the JSON `error` (null when the
 * body was not JSON — a proxy's page, or nothing at all). Null when the status
 * is not a refusal.
 *
 * CPA's strings are matched exactly, from AuthenticateManagementKey in CPA's
 * internal/api/handlers/management/handler.go. A 401 or 403 that does not carry
 * one was issued by something in front of CPA. Either way it came before the
 * plugin ran — the plugin itself never answers 401 or 403 on the console door,
 * and on the token door answers them before its press ledger — so every row
 * here is a request that spent nothing.
 */
export function classifyRefusal(door: DoorKind, status: number, errorText: string | null): Classified | null {
  const plain = (code: RefusalCode, latch: Refusal | null): Classified => ({ code, latch, banSeconds: null })
  if (status === 404) return plain(errorText === "not_found" ? "not_found" : "route_missing", null)
  if (status === 425) return plain("too_early", null)
  if (door === "console") {
    if (status === 401) {
      return errorText === "missing management key" || errorText === "invalid management key"
        ? plain("console_refused", "refused")
        : plain("refused_other", "other")
    }
    if (status === 403) {
      if (errorText !== null && errorText.startsWith("IP banned")) {
        return { code: "ip_banned", latch: "banned", banSeconds: parseBanSeconds(errorText) ?? DEFAULT_BAN_SECONDS }
      }
      if (errorText === "remote management disabled") return plain("remote_disabled", "remote")
      if (errorText === "remote management key not set") return plain("management_off", "off")
      return plain("refused_other", "other")
    }
    return null
  }
  // The plugin's limiter answers 429 only to failed attempts and never
  // throttles a correct token, so a 429 means this token is wrong too.
  if (status === 401 || status === 429) return plain("token_refused", "token")
  if (status === 403) return errorText === "cross_site" ? plain("cross_site", null) : plain("refused_other", "token")
  return null
}

/**
 * The doors open right now, best first: the token, then the console.
 *
 * The token goes first because a wrong one costs nothing at CPA; the console
 * key is the fallback, for a sidebar browser with no saved password. Recomputed
 * on every attempt, so a door closed by a refusal a moment ago — in this
 * document or, through storage, in another — is already skipped.
 */
export function doors(env: Env, token: string | null, refusedToken: string | null): Door[] {
  const open: Door[] = []
  if (token !== null && token !== refusedToken && validKey(token)) open.push({ kind: "token", token })
  const key = usableConsoleKey(env)
  if (key !== null) open.push({ kind: "console", key })
  return open
}

/** The poll, while some door is open; none while there is nothing to present. */
export function refetchInterval(open: readonly Door[]): number | false {
  return open.length > 0 ? POLL_MS : false
}

/**
 * Whether a storage change made by another document is a reason for this one
 * to ask for the document again straight away.
 *
 * Most are: the console signing in or out, a password saved, a latch written.
 * Two are not, because the only door they can open is the console's, and the
 * document that made them is about to present that key itself: a latch lifted
 * by Try it again, and a saved password removed by Sign out, which leaves a
 * console key the password kept unused as the way in. Every other tab asking
 * at once would turn one click into a request per tab, all sent before the
 * one refusal that could latch the key again. Those tabs ask at their own next
 * poll or focus instead, by when that refusal is in storage.
 */
export function rereadAfter(key: string | null, newValue: string | null): boolean {
  // No key: the whole of storage was cleared.
  if (key === null) return true
  if (!WATCHED_KEYS.includes(key)) return false
  return newValue !== null || (key !== LATCH_KEY && key !== TOKEN_KEY)
}

export type Serial = <T>(task: () => Promise<T>) => Promise<T>

/**
 * One request at a time, in order.
 *
 * Every console request in a document goes through one of these, reads and
 * spends alike, and each re-checks its door when its turn comes. However many
 * triggers arrive together with a stale key — a poll, a focus, a retry, a press
 * — the first is refused and latches, and the rest find the door shut and send
 * nothing.
 *
 * `across` stretches that over every document on the origin. Given the
 * origin's own queue — Web Locks, where the browser has them — each turn also
 * waits for that, so tabs a browser restores together, or tabs woken by a
 * click in another, take their turns one by one, and each finds the latch the
 * one before it wrote.
 */
export function createSerial(across?: Serial): Serial {
  let tail: Promise<unknown> = Promise.resolve()
  return <T>(task: () => Promise<T>): Promise<T> => {
    const run = tail.then(across ? () => across(task) : task)
    tail = run.catch(() => undefined)
    return run
  }
}

const BASE64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

/** Unpadded base64url. Spelled out because btoa takes a binary string, not bytes. */
export function base64url(bytes: Uint8Array): string {
  let out = ""
  for (let i = 0; i < bytes.length; i += 3) {
    const a = bytes[i] ?? 0
    const b = bytes[i + 1]
    const c = bytes[i + 2]
    const n = (a << 16) | ((b ?? 0) << 8) | (c ?? 0)
    out += BASE64URL[(n >> 18) & 63]! + BASE64URL[(n >> 12) & 63]!
    if (b !== undefined) out += BASE64URL[(n >> 6) & 63]!
    if (c !== undefined) out += BASE64URL[n & 63]!
  }
  return out
}

/**
 * A fresh press id: 16 random bytes as 22 base64url characters.
 *
 * `fill` is crypto.getRandomValues, which unlike crypto.randomUUID works on an
 * insecure origin — the menu bar app's plain-HTTP address among them.
 */
export function newPressId(fill: (bytes: Uint8Array<ArrayBuffer>) => unknown): string {
  const bytes = new Uint8Array(16)
  fill(bytes)
  return base64url(bytes)
}

/** One confirmed press, as both doors carry it. */
export interface SpendBody {
  credentialId: string
  confirmed: true
  pressId: string
}

/** The token door's spend header: the POST body's JSON, as unpadded base64url. */
export function encodeSpendHeader(body: SpendBody): string {
  return base64url(new TextEncoder().encode(JSON.stringify(body)))
}

/**
 * Press ids awaiting an answer, per credential.
 *
 * A press whose answer was lost keeps its id, so pressing again within ten
 * minutes sends the same id and the plugin's ledger answers it with whatever
 * the first copy came to rather than spending again. The plugin's own answer
 * settles the id, and so does a new press turned away before the plugin ran;
 * the next press is then a new one. A repeat turned away keeps the id, because
 * it says nothing about the press it repeated. Memory only: a reload forgets,
 * which is no worse than the plugin restarting.
 */
export interface PressBook {
  /**
   * The id to send now — the pending one, if it is recent, else a new one —
   * and whether it is the pending one, sent again.
   */
  take(credentialId: string, now: number): { id: string; reused: boolean }
  /** The answer was read; the next press is a new one. */
  settle(credentialId: string): void
}

export function createPressBook(fill: (bytes: Uint8Array<ArrayBuffer>) => unknown): PressBook {
  const pending = new Map<string, { id: string; at: number }>()
  return {
    take(credentialId, now) {
      const held = pending.get(credentialId)
      if (held && now - held.at < PRESS_REUSE_SECONDS) return { id: held.id, reused: true }
      const id = newPressId(fill)
      pending.set(credentialId, { id, at: now })
      return { id, reused: false }
    },
    settle(credentialId) {
      pending.delete(credentialId)
    },
  }
}

/** The parts of a fetch Response this file reads. */
export interface ResponseLike {
  readonly status: number
  readonly ok: boolean
  readonly headers: { get(name: string): string | null }
  text(): Promise<string>
  json(): Promise<unknown>
}

/** The fetch options this file sets; a subset of RequestInit. */
export interface RequestLike {
  method: "GET" | "POST"
  headers: Record<string, string>
  body?: string
  signal: AbortSignal
  mode: "same-origin"
  credentials: "same-origin"
  cache: "no-store"
  redirect: "error"
}

export type FetchLike = (url: string, init: RequestLike) => Promise<ResponseLike>

/** The saved password, and the one this document has seen refused. */
export interface TokenSlot {
  read(): string | null
  refused(): string | null
  refuse(value: string): void
}

/** What the two requests below need from the page. */
export interface Access {
  env: Env
  fetch: FetchLike
  /** One per document, shared by reads and spends. See createSerial. */
  serial: Serial
  token: TokenSlot
  presses: PressBook
  /**
   * Fingerprints of console keys whose last request got no answer, held back
   * from every read in this document until the reader asks again. Memory
   * only: the key may be fine, and a reload is the reader asking again.
   */
  unanswered: Set<string>
}

function request(
  method: RequestLike["method"],
  headers: Record<string, string>,
  signal: AbortSignal,
  body?: string,
): RequestLike {
  // Same-origin, and no redirects: a credential is never carried anywhere but
  // the route it was built for.
  const init: RequestLike = { method, headers, signal, mode: "same-origin", credentials: "same-origin", cache: "no-store", redirect: "error" }
  if (body !== undefined) init.body = body
  return init
}

async function errorOf(response: ResponseLike): Promise<{ json: boolean; body: unknown; error: string | null }> {
  let text: string
  try {
    text = await response.text()
  } catch {
    return { json: false, body: undefined, error: null }
  }
  try {
    const body: unknown = JSON.parse(text)
    const error = isObject(body) && typeof body.error === "string" ? body.error : null
    return { json: true, body, error }
  } catch {
    return { json: false, body: undefined, error: null }
  }
}

/**
 * Latches a key the moment a console request comes back 401 or 403 — first
 * provisionally, on the status alone, then exactly once the body says why. The
 * body read is the one await in between, and nothing may present the key while
 * it runs.
 */
async function refuseConsole(env: Env, key: string, response: ResponseLike): Promise<Classified> {
  latchConsole(env, key, response.status === 401 ? "refused" : "other", response.status)
  const { error } = await errorOf(response)
  const classified = classifyRefusal("console", response.status, error) ?? { code: "refused_other", latch: "other", banSeconds: null }
  const reason = CONSOLE_REASONS.find((one) => one === classified.latch) ?? "other"
  latchConsole(env, key, reason, response.status, classified.banSeconds)
  return classified
}

/** The NoSessionError for the state the doors are in now. */
export function noSession(access: Pick<Access, "env" | "token">): NoSessionError {
  const saved = access.token.read()
  const key = readConsoleKey(access.env)
  const latch = key === null ? null : consoleLatch(access.env, key)
  // A dead token is named first: it is the door this page prefers, and the
  // password is what the sign-in screen asks for.
  const tokenDead = saved !== null && saved !== "" && (saved === access.token.refused() || !validKey(saved))
  return new NoSessionError((saved !== null && saved !== "") || key !== null, tokenDead ? "token" : (latch?.reason ?? null))
}

const openDoors = (access: Pick<Access, "env" | "token">): Door[] =>
  doors(access.env, access.token.read(), access.token.refused())

/** Where a read goes through one door, and with which headers besides the credential. */
export interface ReadTarget {
  url: string
  headers: Record<string, string>
}

/**
 * Reads through the first open door and returns its response, which the caller
 * interprets — except a refusal, which is handled here.
 *
 * With no open door it throws NoSessionError without touching the network. A
 * refused token is latched in memory and the console door tried in its place;
 * a refused console key is latched in storage and NoSessionError thrown; a
 * console request that gets no answer holds its key back in this document and
 * throws ConsoleUnansweredError.
 *
 * The console request runs in the document's serial queue and without the
 * caller's signal. TanStack aborts a query it no longer wants — on refetch, on
 * unmount — and an aborted request is one whose refusal this page never sees,
 * which would leave a refused key unlatched and presented again on the next
 * poll. It keeps its own deadline, so a hung CPA still surfaces.
 */
export async function readThrough(
  access: Access,
  target: (door: Door) => ReadTarget,
  signal?: AbortSignal,
): Promise<{ door: Door; target: ReadTarget; response: ResponseLike }> {
  const first = openDoors(access)[0]
  if (first === undefined) throw noSession(access)

  if (first.kind === "token") {
    const one = target(first)
    const deadline = AbortSignal.timeout(READ_TIMEOUT_MS)
    const response = await access.fetch(
      one.url,
      request("GET", { ...one.headers, Authorization: bearer(first.token) }, signal ? AbortSignal.any([deadline, signal]) : deadline),
    )
    // A refusal by the plugin's own check, or its limiter on failed attempts.
    // None of these reach CPA's counter: CPA authenticates nothing on a
    // resource route.
    if (response.status !== 401 && response.status !== 403 && response.status !== 429) {
      return { door: first, target: one, response }
    }
    access.token.refuse(first.token)
    // Checked before queueing as well as after, so a refused token with no
    // console behind it reports at once rather than waiting out a press.
    if (!openDoors(access).some((one) => one.kind === "console")) throw noSession(access)
  }

  return access.serial(async () => {
    const door = openDoors(access).find((one) => one.kind === "console")
    if (door === undefined || door.kind !== "console") throw noSession(access)
    // A key whose last request went unanswered is not read with again until
    // the reader asks. CPA may have counted that request, and whatever lost
    // its answer — a dropped connection, or a proxy that turns CPA's refusal
    // into a redirect — would lose every later refusal the same way, so
    // nothing would ever latch the key while the poll went on presenting it.
    const fp = fingerprint(access.env, door.key)
    if (access.unanswered.has(fp)) throw new ConsoleUnansweredError()
    const one = target(door)
    let response: ResponseLike
    try {
      response = await access.fetch(
        one.url,
        request("GET", { ...one.headers, Authorization: bearer(door.key) }, AbortSignal.timeout(READ_TIMEOUT_MS)),
      )
    } catch {
      access.unanswered.add(fp)
      throw new ConsoleUnansweredError()
    }
    if (response.status === 401 || response.status === 403) {
      await refuseConsole(access.env, door.key, response)
      throw noSession(access)
    }
    return { door, target: one, response }
  })
}

/** A press or a save that never left the page, and why. */
type Unsent = { sent: false; code: "no_session" | RefusalCode; status: number }

/**
 * A press with no door to go through, reported by the refusal that shut the
 * door — in the same order NoSessionError names them — so the reader is told
 * why nothing was sent rather than only that it was not. The status is the
 * refusal's own, or a 4xx standing in for it: nothing was sent, and every
 * reader of the code treats a 4xx as proof of that.
 */
function unsent(access: Pick<Access, "env" | "token">): Unsent {
  const reason = noSession(access).reason
  if (reason === "token") return { sent: false, code: "token_refused", status: 401 }
  if (reason === null) return { sent: false, code: "no_session", status: 0 }
  const status = consoleLatch(access.env)?.status ?? 0
  return { sent: false, code: CODE_OF_REASON[reason], status: status >= 400 && status < 500 ? status : 403 }
}

/** Where a press goes, by door. */
export interface SpendTargets {
  /** GET, with the press in SPEND_HEADER. */
  tokenURL: string
  /** POST, with the press as the body. */
  consoleURL: string
}

/** What came of one press, before it is put into words. */
export type SpendAnswer =
  /** Nothing left this page. */
  | { sent: false; code: "no_session" | RefusalCode; status: number }
  /** The request went out and nothing came back: the fetch itself failed. */
  | { sent: true; door: DoorKind; answered: false }
  | {
      sent: true
      door: DoorKind
      answered: true
      status: number
      ok: boolean
      /** Whether the body parsed as JSON; `body` is undefined when it did not. */
      json: boolean
      body: unknown
      replayed: boolean
      refusal: Classified | null
      /**
       * The press id is kept for the next press: this answer was lost, or
       * this request repeated a press whose answer was lost and was turned
       * away before the plugin could say what that press came to.
       */
      kept: boolean
    }

/**
 * Sends one confirmed press, through one door, once.
 *
 * The door is chosen before anything is sent and never changes: a refusal is
 * not retried and does not fall through to the other door, because a request
 * that was refused may still be one the reader would not have wanted repeated,
 * and finding out by trying the other door could spend a second reset.
 *
 * The press id is kept only while the page does not know what the plugin did
 * with it: after an answer that was not the plugin's — the fetch failed, a
 * gateway's page or a gateway's own JSON stood in for it, a success came back
 * unreadable — and after a repeat of such a press that was turned away before
 * the plugin ran, which says nothing about the press it repeated. The
 * plugin's own answer settles it, and so does a new press turned away.
 */
export async function spendThrough(access: Access, credentialId: string, targets: SpendTargets): Promise<SpendAnswer> {
  const door = openDoors(access)[0]
  if (door === undefined) return unsent(access)

  const answer = async (
    response: ResponseLike,
    refusal: Classified | null,
    read: Awaited<ReturnType<typeof errorOf>>,
    reused: boolean,
  ): Promise<SpendAnswer> => {
    // The plugin's own answer to this press: an outcome, or one of its codes.
    const fromPlugin = refusal === null && (response.ok ? read.json : PLUGIN_ERRORS.has(read.error ?? ""))
    // Turned away before the plugin acted: a refusal, or a 4xx it did not
    // write. Anything else — a 5xx it did not write above all, which is what a
    // gateway sends when the plugin was slow rather than idle — is an answer
    // lost, and the plugin may have finished the spend after it.
    const turnedAway = !fromPlugin && (refusal !== null || (response.status >= 400 && response.status < 500))
    const kept = !fromPlugin && (!turnedAway || reused)
    if (!kept) access.presses.settle(credentialId)
    return {
      sent: true,
      door: door.kind,
      answered: true,
      status: response.status,
      ok: response.ok,
      json: read.json,
      body: read.body,
      replayed: response.headers.get(REPLAYED_HEADER) === "1",
      refusal,
      kept,
    }
  }

  if (door.kind === "token") {
    const { id: pressId, reused } = access.presses.take(credentialId, access.env.now())
    const headers = {
      Accept: "application/json",
      Authorization: bearer(door.token),
      [SPEND_HEADER]: encodeSpendHeader({ credentialId, confirmed: true, pressId }),
    }
    let response: ResponseLike
    try {
      response = await access.fetch(targets.tokenURL, request("GET", headers, AbortSignal.timeout(SPEND_TIMEOUT_MS)))
    } catch {
      return { sent: true, door: "token", answered: false }
    }
    // A refused token is latched in memory, after the body is read because a
    // 403 is a refusal of the token only when it is not the plugin's own
    // cross_site. Nothing here reaches CPA's counter, so the await costs
    // nothing a console refusal would.
    const read = await errorOf(response)
    const refusal = response.ok ? null : classifyRefusal("token", response.status, read.error)
    if (refusal?.latch === "token") access.token.refuse(door.token)
    return answer(response, refusal, read, reused)
  }

  return access.serial(async (): Promise<SpendAnswer> => {
    // The key may have been refused while this press waited its turn. If so it
    // is not sent again; the press reports the refusal that closed the door.
    const key = usableConsoleKey(access.env)
    if (key === null) return unsent(access)
    const fp = fingerprint(access.env, key)
    const { id: pressId, reused } = access.presses.take(credentialId, access.env.now())
    const headers = { "Content-Type": "application/json", Accept: "application/json", Authorization: bearer(key) }
    let response: ResponseLike
    try {
      response = await access.fetch(
        targets.consoleURL,
        request("POST", headers, AbortSignal.timeout(SPEND_TIMEOUT_MS), JSON.stringify({ credentialId, confirmed: true, pressId })),
      )
    } catch {
      // As for a read: CPA may have counted it, so reads leave the key alone
      // until the reader asks again. A press is the reader asking, so presses
      // are not held back.
      access.unanswered.add(fp)
      return { sent: true, door: "console", answered: false }
    }
    // CPA accepted the key, which is everything a held-back read was unsure of.
    if (response.ok) access.unanswered.delete(fp)
    if (response.status === 401 || response.status === 403) {
      const refusal = await refuseConsole(access.env, key, response)
      // Turned away before the plugin ran: a new press is settled, and a
      // repeat keeps its id, as in answer() above.
      if (!reused) access.presses.settle(credentialId)
      return {
        sent: true,
        door: "console",
        answered: true,
        status: response.status,
        ok: false,
        json: false,
        body: undefined,
        replayed: false,
        refusal,
        kept: reused,
      }
    }
    const read = await errorOf(response)
    return answer(response, response.ok ? null : classifyRefusal("console", response.status, read.error), read, reused)
  })
}

/**
 * One settings batch: every changed row of one card, in the shape both doors
 * carry (internal/overrides ParseBatch). settings.ts builds it; this only
 * sends it.
 */
export interface SettingsBatch {
  kind: string
  items: readonly object[]
}

/** The token door's settings header: the POST body's JSON, as unpadded base64url. */
export function encodeSettingsHeader(batch: SettingsBatch): string {
  return base64url(new TextEncoder().encode(JSON.stringify(batch)))
}

/** Where a save goes, by door. */
export interface SaveTargets {
  /** GET, with the batch in SETTINGS_HEADER. */
  tokenURL: string
  /** POST, with the batch as the body. */
  consoleURL: string
}

/** What came of one save, before it is put into words. */
export type SaveAnswer =
  /** Nothing left this page. */
  | Unsent
  /** The request went out and nothing came back: the fetch itself failed. */
  | { sent: true; door: DoorKind; answered: false }
  | {
      sent: true
      door: DoorKind
      answered: true
      status: number
      ok: boolean
      /** Whether the body parsed as JSON; `body` is undefined when it did not. */
      json: boolean
      body: unknown
      refusal: Classified | null
      /**
       * The plugin's own answer: a JSON outcome, or one of its codes. Anything
       * else came from something in front of it and says nothing about
       * whether it saved.
       */
      plugin: boolean
    }

/**
 * Sends one settings batch, through one door, once: spendThrough's door
 * logic, without a press id.
 *
 * None is needed. The plugin answers a batch it has already applied as
 * unchanged, and one that another save overtook as a conflict, so a resent
 * save is never a second change. It is still never retried and never passed
 * to the other door: the door is fixed before anything is sent, a refusal
 * latches as a read's or a press's does, and the reader decides whether to
 * press Save again.
 */
export async function saveThrough(access: Access, batch: SettingsBatch, targets: SaveTargets): Promise<SaveAnswer> {
  const door = openDoors(access)[0]
  if (door === undefined) return unsent(access)

  if (door.kind === "token") {
    const headers = {
      Accept: "application/json",
      Authorization: bearer(door.token),
      [SETTINGS_HEADER]: encodeSettingsHeader(batch),
    }
    let response: ResponseLike
    try {
      response = await access.fetch(targets.tokenURL, request("GET", headers, AbortSignal.timeout(SAVE_TIMEOUT_MS)))
    } catch {
      return { sent: true, door: "token", answered: false }
    }
    // As for a press: the body is read before the token is latched, because
    // a 403 refuses the token only when it is not the plugin's own cross_site.
    const read = await errorOf(response)
    const refusal = response.ok ? null : classifyRefusal("token", response.status, read.error)
    if (refusal?.latch === "token") access.token.refuse(door.token)
    return saved("token", response, read, refusal)
  }

  return access.serial(async (): Promise<SaveAnswer> => {
    // The key may have been refused while this save waited its turn.
    const key = usableConsoleKey(access.env)
    if (key === null) return unsent(access)
    const fp = fingerprint(access.env, key)
    const headers = { "Content-Type": "application/json", Accept: "application/json", Authorization: bearer(key) }
    let response: ResponseLike
    try {
      response = await access.fetch(
        targets.consoleURL,
        request("POST", headers, AbortSignal.timeout(SAVE_TIMEOUT_MS), JSON.stringify(batch)),
      )
    } catch {
      // CPA may have counted it, so reads leave the key alone until the
      // reader asks again, as after a press.
      access.unanswered.add(fp)
      return { sent: true, door: "console", answered: false }
    }
    if (response.ok) access.unanswered.delete(fp)
    if (response.status === 401 || response.status === 403) {
      const refusal = await refuseConsole(access.env, key, response)
      return saved("console", response, { json: false, body: undefined, error: null }, refusal)
    }
    const read = await errorOf(response)
    return saved("console", response, read, response.ok ? null : classifyRefusal("console", response.status, read.error))
  })
}

/** A save's answer, as saveThrough reports it. */
function saved(
  door: DoorKind,
  response: ResponseLike,
  read: Awaited<ReturnType<typeof errorOf>>,
  refusal: Classified | null,
): SaveAnswer {
  return {
    sent: true,
    door,
    answered: true,
    status: response.status,
    ok: response.ok,
    json: read.json,
    body: read.body,
    refusal,
    plugin: refusal === null && (response.ok ? read.json : PLUGIN_ERRORS.has(read.error ?? "")),
  }
}
