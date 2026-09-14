import type { Counters } from "../lib/types"

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

/**
 * The pool in one line.
 *
 * Straight from `counters`, which reports exactly three numbers: how many
 * credentials there are, how many polled cleanly, and how many failed. The
 * remainder — pending, unsupported, disabled, unavailable — is deliberately not
 * reconstructed here by subtraction. Four different situations share that gap
 * and a single invented label for all of them would be wrong for at least
 * three.
 */
export function Footer({ counters }: { counters: Counters }) {
  const parts = [plural(counters.credentials, "credential"), `${counters.observedOK} observed`]
  if (counters.observeError > 0) parts.push(`${counters.observeError} failing`)

  return <footer className="mt-[18px] pl-[2px] text-[11.5px] text-ink-3">{parts.join(" · ")}</footer>
}
