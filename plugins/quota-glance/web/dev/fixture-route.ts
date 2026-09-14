import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import type { Plugin } from "vite"

// A stand-in for the plugin's summary route, backed by the committed golden
// fixtures. It exists so the app can be developed and reviewed without a
// running CPA, and it lives in the dev server rather than in the bundle: the
// shell is served unauthenticated, so a fixture full of addresses must never
// reach it.
//
// What it copies from the real route, because the client depends on it: the
// path CPA authenticates, ETag revalidation with 304, and no-store caching.
// What it adds, because a fixture cannot: scenarios for the states the design
// has to survive.
//
// Both paths, because the client tries both: CPA's management route first, and
// the plugin's own token-guarded resource route as the fallback.
//
// In production CPA authenticates the management path before the plugin sees
// the request, and the plugin itself checks the token on the resource path.
// Here there is no CPA, so the management path accepts any bearer value and
// refuses only its absence, while the resource path checks against DEV_TOKEN —
// which is what lets the sign-in screen and the password field be exercised.

const MANAGEMENT_PATH = "/v0/management/plugins/quota-glance/summary"
const RESOURCE_PATH = "/v0/resource/plugins/quota-glance/summary"
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

type Outcome = Doc | "unauthorized" | "down"

/** The states §5 of the handoff requires the app to render legibly. */
function scenarios(): Record<string, () => Outcome> {
  const golden = () => JSON.parse(readFileSync(fixture("summary"), "utf8")) as Doc
  const degraded = () => JSON.parse(readFileSync(fixture("summary-degraded"), "utf8")) as Doc

  return {
    golden,
    degraded,

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

    // The plugin has been updated past what this bundle knows how to read.
    "future-schema": () => ({ ...golden(), schemaVersion: 2 }),

    unauthorized: () => "unauthorized",
    // Serves the document, but only down the fallback path: the refusal below
    // is keyed on the scenario name. Exercises the fall from a rejected console
    // session to the saved password, which is the point of having two ways in.
    "cpa-expired": golden,
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

export function goldenFixtureRoute(): Plugin {
  // Fixed for the life of the dev server. See rebase().
  let offset: number | null = null

  return {
    name: "quota-glance:golden-fixture-route",
    apply: "serve",
    configureServer(server) {
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
        const result = pick()

        // CPA answers an absent or rejected management key with a bare 401, and
        // so does the plugin for a wrong token.
        const presented = (req.headers.authorization ?? "").replace(/^Bearer /i, "").trim()
        const refused =
          (name === "cpa-expired" && viaCPA) || (viaCPA ? presented === "" : presented !== DEV_TOKEN)
        if (result === "unauthorized" || refused) {
          res.statusCode = 401
          res.setHeader("Cache-Control", "no-store")
          res.end()
          return
        }
        if (result === "down") {
          res.statusCode = 503
          res.setHeader("Cache-Control", "no-store")
          res.end()
          return
        }

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
