import { CHECKING_LABEL } from "../lib/checks"

/**
 * "Checking the new limits…" beside an account whose banked reset was just
 * spent, while Quota Cache polls it: the figures beside it are from before the
 * reset. The words are in its title as well, for a row too narrow to show
 * them whole. When it shows and when it goes is checks.ts.
 */
export function CheckingPill() {
  return (
    <span className="qg-chip qg-chip-checking" title={CHECKING_LABEL}>
      <span>{CHECKING_LABEL}</span>
    </span>
  )
}
