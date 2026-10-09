import type { FoldFlag } from "../lib/pool"

/**
 * The fold marker: pointing down when the card is open, right when it is
 * shut. `aria-hidden` because the button around it announces its state
 * through `aria-expanded`.
 */
export function Chevron() {
  return (
    <svg viewBox="0 0 10 10" aria-hidden="true" className="qg-chev">
      <path
        d="M1.5 3.5 L5 7 L8.5 3.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

const FLAG_TONE: Record<string, string> = { low: "qg-chip-low", critical: "qg-chip-critical" }

/**
 * The accounts a shut fold names, at the right end of its line: the name,
 * the figure in the figures' face, and the word for it. Nothing when every
 * account is fine, so a calm card has a calm fold line.
 */
export function Flags({ flags }: { flags: Pick<FoldFlag, "id" | "name" | "figure" | "word" | "tone">[] }) {
  if (flags.length === 0) return null
  return (
    <span className="qg-flags">
      {flags.map((flag) => (
        <span key={flag.id} className={`qg-chip ${FLAG_TONE[flag.tone] ?? ""}`}>
          {/* Each its own item, so the chip's gap spaces them: adjacent text
            * would run together as one. */}
          <span>{flag.name}</span>
          {flag.figure && <b className="num">{flag.figure}</b>}
          {flag.word && <span>{flag.word}</span>}
        </span>
      ))}
    </span>
  )
}

/**
 * The line that opens and shuts a card's list: a full-width button, because
 * at 390px a chevron alone is not a target. What it holds is the card's
 * business — a count, a note, the accounts worth a look — and all of it is
 * read with the card shut, so it is the button's name as well as its label.
 */
export function FoldButton({
  open,
  controls,
  labelledBy,
  onToggle,
  className = "",
  children,
}: {
  open: boolean
  controls: string
  /** The card's title, then the fold's own content, so each fold is told apart from the next. */
  labelledBy: string
  onToggle: () => void
  className?: string
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      className={`qg-fold ${className}`}
      aria-expanded={open}
      aria-controls={controls}
      aria-labelledby={labelledBy}
      onClick={onToggle}
    >
      <Chevron />
      {children}
    </button>
  )
}
