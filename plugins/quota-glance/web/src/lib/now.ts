import { useSyncExternalStore } from "react"

import { getSnapshot, subscribe } from "./clock"

// The page's one clock, as React reads it. When it runs and what it reads
// live in clock.ts, where the node tests reach them.
//
// Each reader subscribes for itself, so a tick re-renders the rows that show a
// time and nothing above them. A store rather than state: shown again, the
// page commits the caught-up countdowns before its first frame, where a state
// update could paint the old ones first.

/** The current instant in Unix seconds, re-rendering its consumers each tick. */
export function useNowSeconds(): number {
  return useSyncExternalStore(subscribe, getSnapshot)
}
