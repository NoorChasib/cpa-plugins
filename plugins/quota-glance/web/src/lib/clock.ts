// One clock for the whole page.
//
// Every countdown on screen is the same subtraction against the same instant,
// so they share a single interval rather than starting one per row. A pool of
// seven credentials across six windows is forty-odd countdowns; forty timers
// drifting against each other would show neighbouring rows ticking a second
// apart.
//
// It runs only while something reads it and the page can be seen. The menu
// bar app keeps this page loaded all day in a popover that is mostly closed,
// and WebKit still fires a hidden page's one-second interval every second, so
// a clock left running re-renders countdowns nobody can see. Shown again, the
// page reads the wall clock at once, before it paints.
//
// It imports nothing, so node's tests drive it directly; now.ts hands it to
// React.

/** The part of the document the clock reads. */
interface Page {
  readonly visibilityState: string
  addEventListener(type: "visibilitychange", listener: () => void): void
  removeEventListener(type: "visibilitychange", listener: () => void): void
}

// globalThis rather than a bare `document`, which is the same object in a
// browser and lets the tests stand in a page of their own under node.
const page = (): Page => (globalThis as typeof globalThis & { document: Page }).document

let current = Math.floor(Date.now() / 1000)
const listeners = new Set<() => void>()
let timer: ReturnType<typeof setInterval> | undefined

function tick(): void {
  // Read the wall clock each tick rather than counting intervals: a laptop
  // that slept for an hour has to come back showing the hour, not resume
  // where it left off.
  const next = Math.floor(Date.now() / 1000)
  if (next === current) return
  current = next
  for (const listener of listeners) listener()
}

// Runs the interval while anything listens and the page is visible, and stops
// it otherwise. Starting it reads the clock first, so a page shown again or a
// first reader never sees the instant the clock stopped at.
function sync(): void {
  const run = listeners.size > 0 && page().visibilityState === "visible"
  if (run && timer === undefined) {
    tick()
    timer = setInterval(tick, 1000)
  } else if (!run && timer !== undefined) {
    clearInterval(timer)
    timer = undefined
  }
}

/** For useSyncExternalStore: calls `listener` each time the second changes. */
export function subscribe(listener: () => void): () => void {
  if (listeners.size === 0) page().addEventListener("visibilitychange", sync)
  listeners.add(listener)
  sync()
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0) page().removeEventListener("visibilitychange", sync)
    sync()
  }
}

/** The current instant in Unix seconds, as of the clock's last tick. */
export function getSnapshot(): number {
  return current
}
