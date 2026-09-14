import { createContext, useContext, useEffect, useState } from "react"

// One clock for the whole page.
//
// Every countdown on screen is the same subtraction against the same instant,
// so they share a single interval rather than starting one per row. A pool of
// seven credentials across six windows is forty-odd countdowns; forty timers
// drifting against each other would show neighbouring rows ticking a second
// apart.

const NowContext = createContext<number>(Math.floor(Date.now() / 1000))

export const NowProvider = NowContext.Provider

/** The current instant in Unix seconds, re-rendering its consumers each tick. */
export function useNowSeconds(): number {
  return useContext(NowContext)
}

export function useClock(): number {
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000))
  useEffect(() => {
    // Read the wall clock each tick rather than counting intervals: a laptop
    // that slept for an hour has to come back showing the hour, not resume
    // where it left off.
    const id = window.setInterval(() => setNow(Math.floor(Date.now() / 1000)), 1000)
    return () => window.clearInterval(id)
  }, [])
  return now
}
