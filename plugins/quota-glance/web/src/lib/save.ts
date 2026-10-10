// Saving what the editor changed: an organization's monthly credit, refill
// date and Console reading, or a Claude subscription's renewal date.
//
// A Save goes through one door, chosen before it is sent (access.ts
// saveThrough), exactly as a press does. With the dashboard password it is a
// GET to the save-settings route, carrying the batch in a header, because CPA
// dispatches nothing but GET to a resource route; with a console session it is
// a POST to the management settings route. Neither is retried and neither
// falls through to the other door. Unlike a press, a save needs no id to be
// safe to repeat: the plugin answers a batch it already holds as unchanged,
// and one another save overtook as a conflict.
//
// The menu bar's readout never sees one. It intercepts only GETs to the two
// summary paths; a POST, and a GET to save-settings, pass it by. After a
// save the page re-reads the summary through its normal request.

import { saveThrough, type SettingsBatch } from "./access"
import { managementPath, resourcePath } from "./cpa-auth"
import { accessContext, openDoors } from "./session"
import { type SaveOutcome, saveOutcome } from "./settings"

/**
 * Whether this browser can save at all: some door is open. The same check
 * that hides Use one: a control certain to be refused is worse than none, and
 * a console key CPA has refused is never offered again.
 */
export function canSaveHere(): boolean {
  return openDoors().length > 0
}

/**
 * Carries ?scenario= and ?save= through to the dev fixture route, which is
 * how each answer a save can get is reviewed. Stripped from the production
 * bundle, where the only thing on the other end is the plugin.
 */
function devTarget(url: string): string {
  if (!import.meta.env.DEV) return url
  const dev = new URLSearchParams(window.location.search)
  const forwarded = new URLSearchParams()
  for (const key of ["scenario", "save", "rebase"]) {
    const value = dev.get(key)
    if (value !== null) forwarded.set(key, value)
  }
  return [...forwarded].length > 0 ? `${url}?${forwarded}` : url
}

/** Sends one batch, once, and says what came of it. */
export async function saveSettings(batch: SettingsBatch): Promise<SaveOutcome> {
  const answer = await saveThrough(accessContext(), batch, {
    tokenURL: devTarget(resourcePath("/save-settings")),
    consoleURL: devTarget(managementPath("/settings")),
  })
  return saveOutcome(answer)
}
