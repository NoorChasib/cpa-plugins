// The editor's parts, shared by the Monthly API Credit card and the Accounts
// card: both turn their open list into fields in place (option A), and both
// end in the same pinned foot with one Save.

/** The pencil on an Edit button. */
function Pencil() {
  return (
    <svg viewBox="0 0 12 12" fill="none" aria-hidden="true">
      <path d="M8.4 1.6 10.4 3.6 4.2 9.8 1.6 10.4 2.2 7.8Z" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
    </svg>
  )
}

/** The tick before a saved line. */
function Tick() {
  return (
    <svg viewBox="0 0 12 12" fill="none" aria-hidden="true">
      <path d="M2.4 6.4 4.9 8.8 9.6 3.4" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

/**
 * "Set here": a value typed on the dashboard rather than read from Quota
 * Cache's config. A dot in the accent after the value; the hover says what
 * the config has. `lead` is the one in a foot or a caption, before its words.
 */
export function SetHereDot({ title, lead = false }: { title?: string; lead?: boolean }) {
  return lead ? (
    <span className="qg-mk is-lead" aria-hidden="true" />
  ) : (
    <span className="qg-mk" role="img" aria-label="set here" title={title} />
  )
}

/** The button that turns a card's list into fields. */
export function EditButton({ onClick, children }: { onClick: () => void; children: React.ReactNode }) {
  return (
    <button type="button" className="qg-btn qg-fbtn" onClick={onClick}>
      <Pencil />
      {children}
    </button>
  )
}

/** A link-shaped action under a field or in a line: Undo, Use config, Add, Remove. */
export function EditLink({
  onClick,
  children,
  label,
  disabled = false,
}: {
  onClick: () => void
  children: React.ReactNode
  label?: string
  disabled?: boolean
}) {
  return (
    <button type="button" className="qg-elink" aria-label={label} disabled={disabled} onClick={onClick}>
      {children}
    </button>
  )
}

/** The middot between the parts of a caption, which a screen reader skips. */
export function Dot() {
  return (
    <span className="qg-edot" aria-hidden="true">
      ·
    </span>
  )
}

/**
 * An input's frame: the sign-in field's look, tinted once changed, dashed when
 * it has gone back to the config's value, red when it cannot be sent.
 */
export function Box({
  state,
  prefix,
  children,
}: {
  state: { changed: boolean; dropped: boolean; bad: boolean; busy: boolean }
  prefix?: string
  children: React.ReactNode
}) {
  const classes = [
    "qg-ebox",
    state.changed ? "is-dirty" : "",
    state.dropped ? "is-back" : "",
    state.bad ? "is-bad" : "",
    state.busy ? "is-busy" : "",
  ]
  return (
    <span className={classes.filter(Boolean).join(" ")}>
      {prefix && (
        <span className="qg-epre" aria-hidden="true">
          {prefix}
        </span>
      )}
      {children}
    </span>
  )
}

/** What the foot says beside its buttons, and in what ink. */
export type FootStatus = { tone: "ok" | "bad" | "unknown" | "warn"; text: string }

/** A status's notice: an answer that may or may not have saved, and a dropped row, are amber. */
const NOTICE_TONE: Record<FootStatus["tone"], string> = {
  ok: "is-ok",
  bad: "is-bad",
  unknown: "is-unknown",
  warn: "is-unknown",
}

/**
 * The foot of a list being edited: what the fields mean, then the change
 * count, Cancel and Save. Pinned to the bottom of the viewport while it is
 * on screen (must-fix 1), so Save stays in reach in the menu bar's 400×620
 * popover however many rows are open above it.
 *
 * Save is off with nothing to send, with anything to fix, while a save is
 * under way and while no door is open; off, it takes the muted look of an
 * unavailable Use one, not a fade (must-fix 7). After a failure, while
 * there is still something to send, it reads "Try again"; the reason sits
 * above the buttons either way.
 */
export function EditFoot({
  help,
  changes,
  errors,
  saving,
  blocked,
  status,
  failed,
  onCancel,
  onSave,
}: {
  help: React.ReactNode
  changes: number
  errors: number
  saving: boolean
  /** Why Save cannot be pressed whatever the fields say: access lost, editing switched off. Null when it can. */
  blocked: string | null
  status: FootStatus | null
  failed: boolean
  onCancel: () => void
  onSave: () => void
}) {
  const count =
    errors > 0
      ? `${errors} to fix`
      : changes === 0
        ? "no changes"
        : `${changes} ${changes === 1 ? "change" : "changes"}`
  const off = saving || blocked !== null || errors > 0 || changes === 0
  return (
    <div className="qg-efoot">
      {(status || blocked) && (
        <p className={`qg-notice ${NOTICE_TONE[status?.tone ?? "bad"]} qg-efail`} role="alert">
          <span>{blocked ?? status?.text}</span>
        </p>
      )}
      <div className="qg-efoot-row">
        <span className="qg-ehelp">{help}</span>
        <span className="qg-eacts">
          <span className={`qg-ecount ${errors > 0 ? "is-bad" : ""}`} aria-live="polite">
            {count}
          </span>
          <button type="button" className="qg-btn" disabled={saving} onClick={onCancel}>
            Cancel
          </button>
          <button type="button" className="qg-btn qg-btn-go qg-esave" disabled={off} onClick={onSave}>
            {saving ? (
              <>
                <span className="qg-espin" aria-hidden="true" />
                Saving…
              </>
            ) : failed && changes > 0 ? (
              "Try again"
            ) : (
              "Save"
            )}
          </button>
        </span>
      </div>
    </div>
  )
}

/** A card's foot at rest: where its values come from, a saved line when there is one, and the Edit button. */
export function RestFoot({
  children,
  saved,
  action,
}: {
  children: React.ReactNode
  saved: string | null
  action: React.ReactNode
}) {
  return (
    <div className="qg-afoot">
      {saved ? (
        <span className="qg-eok" role="status">
          <Tick />
          <span>{saved}</span>
        </span>
      ) : (
        <span>{children}</span>
      )}
      {action}
    </div>
  )
}
