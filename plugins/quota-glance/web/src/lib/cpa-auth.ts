// Authentication, borrowed from the console that is already open.
//
// This page is served from a CPA resource route, which CPA does not
// authenticate — so it holds no credential of its own and asks for none. What
// it does instead is what every other plugin page in this repository does
// (see plugins/quota-cache/internal/plugin/browser_auth_script.go): recover the
// management key that the official management console
// (Cli-Proxy-API-Management-Center) persists in localStorage, and present it to
// this plugin's own management routes, which CPA *does* authenticate.
//
// The console stores its state under "cli-proxy-auth" — legacy installs used
// "managementKey" — behind a documented reversible XOR obfuscation marked with
// an "enc::v1::" prefix and keyed by a fixed salt, location.host and
// navigator.userAgent. Reading it is the documented trust model for plugin
// resource pages served from the console's own origin; a console on a different
// origin simply has no entry here, and this returns null.
//
// The recovered key goes to same-origin CPA management routes and nowhere else.
// It is never stored, copied, or logged by this page.

const SALT = "cli-proxy-api-webui::secure-storage"
const OBFUSCATION_PREFIX = "enc::v1::"
const PLUGIN_ID = "quota-glance"

function keyBytes(): Uint8Array {
  let text = SALT
  try {
    text = `${SALT}|${window.location.host}|${window.navigator.userAgent}`
  } catch {
    /* fall back to the salt alone */
  }
  return new TextEncoder().encode(text)
}

function deobfuscate(raw: string | null): string | null {
  if (typeof raw !== "string" || raw === "") return null
  if (!raw.startsWith(OBFUSCATION_PREFIX)) return raw
  try {
    const binary = atob(raw.slice(OBFUSCATION_PREFIX.length))
    const key = keyBytes()
    const plain = new Uint8Array(binary.length)
    for (let i = 0; i < binary.length; i++) {
      plain[i] = binary.charCodeAt(i) ^ key[i % key.length]!
    }
    return new TextDecoder().decode(plain)
  } catch {
    return null
  }
}

function parseLoose(text: string | null): unknown {
  if (typeof text !== "string") return null
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

/** The console's management key, or null when this browser has no console session. */
export function managementKey(): string | null {
  try {
    const persisted = parseLoose(deobfuscate(window.localStorage.getItem("cli-proxy-auth")))
    if (persisted && typeof persisted === "object") {
      const state = (persisted as { state?: { managementKey?: unknown } }).state
      if (state && typeof state.managementKey === "string" && state.managementKey !== "") {
        return state.managementKey
      }
    }
    const legacy = parseLoose(deobfuscate(window.localStorage.getItem("managementKey")))
    if (typeof legacy === "string" && legacy !== "") return legacy
  } catch {
    /* storage unavailable; treated the same as no session */
  }
  return null
}

/**
 * An absolute management path, built from where this page is.
 *
 * Derived rather than hardcoded so a reverse proxy that mounts CPA under a
 * prefix keeps working, and so the page behaves the same whether it was opened
 * from the resource tree or the management one.
 */
export function managementPath(suffix: string): string {
  const path = window.location.pathname.replace(/\/+$/, "")
  let prefix = ""
  for (const marker of [`/v0/management/plugins/${PLUGIN_ID}`, `/v0/resource/plugins/${PLUGIN_ID}`]) {
    const at = path.indexOf(marker)
    if (at >= 0) {
      prefix = path.slice(0, at)
      break
    }
  }
  return `${prefix}/v0/management/plugins/${PLUGIN_ID}${suffix}`
}

/**
 * `extra`, plus the Authorization header when a key was recovered.
 *
 * The header value is assembled from parts so the built page never contains a
 * literal bearer-credential string — the same precaution the other plugin pages
 * take, and what lets the build assert it carries no token.
 */
export function authHeaders(extra?: Record<string, string>): Record<string, string> {
  const headers: Record<string, string> = { ...extra }
  const key = managementKey()
  if (key) headers.Authorization = ["Bearer", key].join(" ")
  return headers
}
