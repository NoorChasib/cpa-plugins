// The browser's side of access.ts: where this page is, its storage, the one
// console queue a document has, and what the Access control shows.
//
// Everything that decides whether a credential may be presented lives in
// access.ts, where the node tests reach it. This file only supplies the
// browser's answers to its questions and tells React when they change.

import { useSyncExternalStore } from "react"

import {
  type Access,
  ACCESS_CHANGED,
  type ConsoleLatch,
  consoleLatch,
  createPressBook,
  createSerial,
  type Door,
  doors,
  type Env,
  forgiveConsole,
  LATCH_KEY,
  pagePrefix,
  readConsoleKey,
  rereadAfter,
  retryBlocked,
  type Serial,
  type StorageLike,
  WATCHED_KEYS,
} from "./access"
import * as token from "./token"

/**
 * localStorage, except that a write the browser refuses is kept in memory.
 *
 * The console latch is the value that matters: a browser with a full quota,
 * or one of the private modes that refuses writes, would otherwise drop it and
 * present the refused key again on the next poll. Held in memory it lasts as
 * long as this document, which is all a document can promise without storage.
 * Reads prefer storage, so a change made in another tab still wins.
 */
const memory = new Map<string, string>()

/** Announces a latch change to this document, after the current render. */
function changed(key: string): void {
  if (key === LATCH_KEY) queueMicrotask(() => window.dispatchEvent(new Event(ACCESS_CHANGED)))
}

const storage: StorageLike = {
  getItem(key) {
    let value: string | null = null
    try {
      value = window.localStorage.getItem(key)
    } catch {
      /* storage refused; memory is all there is */
    }
    return value ?? memory.get(key) ?? null
  },
  setItem(key, value) {
    try {
      window.localStorage.setItem(key, value)
      memory.delete(key)
    } catch {
      memory.set(key, value)
    }
    changed(key)
  },
  removeItem(key) {
    memory.delete(key)
    try {
      window.localStorage.removeItem(key)
    } catch {
      /* nothing stored to remove */
    }
    changed(key)
  },
}

export function browserEnv(): Env {
  return {
    storage,
    origin: window.location.origin,
    host: window.location.host,
    pathname: window.location.pathname,
    userAgent: window.navigator.userAgent,
    now: () => Math.floor(Date.now() / 1000),
  }
}

/**
 * The origin's console queue: one Web Lock, shared by every document on this
 * origin, held from re-checking the door to latching a refusal. Absent where
 * the browser keeps Web Locks from the page, which it does on an insecure
 * origin — a plain-HTTP LAN address among them — and there each document
 * queues on its own.
 */
function originQueue(): Serial | undefined {
  const locks = (window.navigator as { locks?: LockManager }).locks
  if (!locks) return undefined
  return <T>(task: () => Promise<T>) => locks.request("quota-glance:console", () => task()) as Promise<T>
}

/**
 * Every console request this document makes, reads and spends, one at a time,
 * and one at a time with every other document on the origin where it can be.
 */
export const consoleQueue = createSerial(originQueue())

/** Pending press ids, per credential, for this document. See PressBook. */
const presses = createPressBook((bytes) => window.crypto.getRandomValues(bytes))

/** Console keys held back from reads after a request that got no answer. */
const unanswered = new Set<string>()

/** What access.ts's two requests need, built from this browser. */
export function accessContext(): Access {
  return {
    env: browserEnv(),
    fetch: (url, init) => window.fetch(url, init),
    serial: consoleQueue,
    token: { read: token.read, refused: token.refused, refuse: token.refuse },
    presses,
    unanswered,
  }
}

/** The doors open now, best first. */
export function openDoors(): Door[] {
  return doors(browserEnv(), token.read(), token.refused())
}

/** What the Access control shows about this browser's two ways in. */
export interface AccessState {
  /** A dashboard password is saved in this browser. */
  tokenSaved: boolean
  /** It is saved, and the plugin refused it in this document. */
  tokenRefused: boolean
  /** The console holds a key this page could use: remembered, for this CPA. */
  consoleFound: boolean
  /** That key's latch, when CPA refused it. */
  consoleLatch: ConsoleLatch | null
  /** The door the next request goes through; null when neither is open. */
  reading: "token" | "console" | null
  /** The console, on this page's own CPA. */
  consoleHref: string
}

export function accessState(): AccessState {
  const env = browserEnv()
  const saved = token.read()
  const key = readConsoleKey(env)
  const open = doors(env, saved, token.refused())
  return {
    tokenSaved: saved !== null,
    tokenRefused: saved !== null && !open.some((door) => door.kind === "token"),
    consoleFound: key !== null,
    consoleLatch: key === null ? null : consoleLatch(env, key),
    reading: open[0]?.kind ?? null,
    consoleHref: `${pagePrefix(env.pathname)}/management.html`,
  }
}

const sameLatch = (a: ConsoleLatch | null, b: ConsoleLatch | null): boolean =>
  a === b ||
  (a !== null &&
    b !== null &&
    a.fp === b.fp &&
    a.reason === b.reason &&
    a.status === b.status &&
    a.at === b.at &&
    a.until === b.until)

const sameState = (a: AccessState, b: AccessState): boolean =>
  a.tokenSaved === b.tokenSaved &&
  a.tokenRefused === b.tokenRefused &&
  a.consoleFound === b.consoleFound &&
  a.reading === b.reading &&
  a.consoleHref === b.consoleHref &&
  sameLatch(a.consoleLatch, b.consoleLatch)

/**
 * Calls `fn` when another document on this origin changes a storage key that
 * can open or close a door: the console signing in or out, a password saved
 * or cleared elsewhere, a latch written or lifted in another tab. A `storage`
 * event with no key is a whole-storage clear, which changes all of them.
 *
 * It only notifies. It never lifts a latch, so a key refused anywhere stays
 * held back here; the caller re-reads, and the doors are recomputed.
 */
export function subscribe(fn: () => void): () => void {
  const watched = new Set(WATCHED_KEYS)
  const onStorage = (event: StorageEvent) => {
    if (event.key === null || watched.has(event.key)) fn()
  }
  window.addEventListener("storage", onStorage)
  return () => window.removeEventListener("storage", onStorage)
}

/**
 * subscribe, for the document's reads: only the changes worth asking for the
 * document again at once. See rereadAfter.
 */
export function subscribeReads(fn: () => void): () => void {
  const onStorage = (event: StorageEvent) => {
    if (rereadAfter(event.key, event.newValue)) fn()
  }
  window.addEventListener("storage", onStorage)
  return () => window.removeEventListener("storage", onStorage)
}

/** subscribe, plus this document's own changes, for the Access control. */
function watch(fn: () => void): () => void {
  const stop = subscribe(fn)
  window.addEventListener(ACCESS_CHANGED, fn)
  return () => {
    stop()
    window.removeEventListener(ACCESS_CHANGED, fn)
  }
}

// useSyncExternalStore compares snapshots by identity and re-renders on any
// difference, so an unchanged state has to come back as the same object.
let snapshot: AccessState | null = null

function current(): AccessState {
  const next = accessState()
  if (snapshot !== null && sameState(snapshot, next)) return snapshot
  snapshot = next
  return next
}

export function useAccess(): AccessState {
  return useSyncExternalStore(watch, current)
}

/**
 * Forgets the dashboard password in this browser and reloads.
 *
 * The reload is part of it: it also drops the menu bar app's readout template,
 * which replays this page's last successful summary request and lives exactly
 * as long as the document. It does not sign anyone out of the CPA console,
 * which is the console's own session.
 */
export function signOut(): void {
  token.clear()
  window.location.reload()
}

/**
 * Lifts the console latch, for the Access control's "Try it again" and nothing
 * else. Refused, returning false, while CPA's ban has not lifted. The caller
 * then asks for the document once; one click presents the key at most once,
 * because a second refusal latches it again before anything else can send it.
 */
export function retryConsole(): boolean {
  return forgiveConsole(browserEnv())
}

/**
 * Lets reads present a console key again after a request that got no answer,
 * for the banner's "Try again" and nothing else. The caller then asks for the
 * document once, so one click presents the key at most once.
 */
export function resumeConsole(): void {
  unanswered.clear()
}

/** Whether "Try it again" is pointless right now: CPA's ban has not lifted. */
export function consoleRetryBlocked(latch: ConsoleLatch | null, now: number): boolean {
  return retryBlocked(latch, now)
}
