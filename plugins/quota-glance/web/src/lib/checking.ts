import { useSyncExternalStore } from "react"

import { type Check, getChecks, subscribe } from "./checks"

// The accounts being checked after a banked reset, as React reads them. What
// a check is and when it ends live in checks.ts, where the node tests reach
// them.

/** Every check started, re-rendering its consumers when another starts. */
export function useChecks(): readonly Check[] {
  return useSyncExternalStore(subscribe, getChecks)
}
