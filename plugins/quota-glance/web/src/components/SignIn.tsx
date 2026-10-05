import { useState } from "react"

import { pagePrefix, type Refusal } from "../lib/access"

/**
 * The two ways in, the password first.
 *
 * Opened from the CPA console sidebar this screen usually never appears: the
 * page finds the console's remembered session and uses it. It appears when
 * there is no such session — a phone, a bookmark, the menu bar app — and when
 * what this browser held was refused, in which case it says which and why.
 *
 * The password is the plugin's `web-token`. It goes first because it works
 * everywhere this page loads and never costs a failed CPA management sign-in.
 * Saved here, it is what gets sent on every later visit from this browser, so
 * the screen is seen once.
 */

/**
 * Why the screen is up, by what refused what. The console's reasons are CPA's
 * own words, read off its refusal; a proxy's is the same advice as a wrong key.
 */
const REFUSED: Record<Refusal, string> = {
  token: "The saved dashboard password was refused. Enter the current one.",
  refused:
    "CPA refused the console session saved in this browser, so this page has stopped using it. Enter the dashboard password, or sign in to the CPA console again.",
  other:
    "CPA refused the console session saved in this browser, so this page has stopped using it. Enter the dashboard password, or sign in to the CPA console again.",
  remote: "CPA accepts console sessions only on the machine it runs on. Enter the dashboard password instead.",
  off: "CPA’s management API is switched off on this server. Enter the dashboard password instead.",
  banned:
    "CPA is refusing management sign-ins from this address for up to 30 minutes after repeated failures. This page will not retry. The dashboard password works meanwhile.",
}

export function SignIn({
  reason,
  rejected,
  onPassword,
  onCancel,
  actions,
}: {
  /** What was refused, when something was; null on a first visit. */
  reason: Refusal | null
  /** Something this browser held was refused, as opposed to nothing held. */
  rejected: boolean
  onPassword: (value: string) => void
  /** Back to the figures; only when there are figures to go back to. */
  onCancel?: () => void
  /** The Access control, so the console session can be retried from here too. */
  actions?: React.ReactNode
}) {
  const [value, setValue] = useState("")
  const trimmed = value.trim()
  // The console's own page, on this CPA. `/` is CPA's JSON root, not the
  // console, which is what this link used to land on.
  const consoleHref = `${pagePrefix(window.location.pathname)}/management.html`

  return (
    <main className="mx-auto flex min-h-dvh max-w-[420px] flex-col justify-center px-[22px] py-8">
      <div className="mb-[10px] flex items-center justify-between gap-3">
        <h1 className="text-[19px] font-[650] tracking-[-0.015em]">{rejected ? "That didn’t work" : "Sign in"}</h1>
        {actions}
      </div>
      <p className="mb-[22px] text-[12.5px] leading-[1.6] text-ink-2">
        {(reason && REFUSED[reason]) ??
          "Enter the dashboard password to see your quotas and use banked resets in this browser."}
      </p>

      <form
        onSubmit={(event) => {
          event.preventDefault()
          if (trimmed) onPassword(trimmed)
        }}
      >
        {/* type=password so it is not left legible on a screen someone else can
          * see; autoComplete off so browsers do not keep a second copy. Focused
          * when the reader came here on purpose from the dashboard, and not on
          * a first visit, where a phone would raise its keyboard over the
          * explanation. */}
        <input
          type="password"
          value={value}
          onChange={(event) => setValue(event.target.value)}
          placeholder="Dashboard password"
          aria-label="Dashboard password"
          autoComplete="off"
          autoCorrect="off"
          autoCapitalize="off"
          spellCheck={false}
          autoFocus={onCancel !== undefined}
          className="num w-full rounded-[8px] border border-line bg-card px-[11px] py-[9px] text-[12.5px]
            text-ink outline-none placeholder:text-ink-3 placeholder:font-sans focus:border-accent"
        />
        <button
          type="submit"
          disabled={!trimmed}
          className="mt-[8px] w-full rounded-[8px] border border-line bg-card-2 px-[11px] py-[9px]
            text-[12.5px] font-medium text-ink transition-colors
            hover:border-accent disabled:cursor-not-allowed disabled:opacity-45 disabled:hover:border-line"
        >
          Save and continue
        </button>
      </form>
      <p className="mt-[7px] text-[11.5px] leading-[1.55] text-ink-3">
        This is <code className="num">web-token</code> from the plugin’s configuration (printed once in the CPA log if
        you left it empty). It is saved in this browser. Anyone who has it can view this dashboard and use banked
        resets.
      </p>

      <div className="my-[18px] flex items-center gap-[10px]">
        <span className="h-px flex-1 bg-line" />
        <span className="text-[11px] text-ink-3">or</span>
        <span className="h-px flex-1 bg-line" />
      </div>

      {/* _top: in the CPA sidebar this page is a frame, and the console has to
        * load in the window rather than inside it. Drawn quieter than the
        * password's button: it is the second way in, and the one whose
        * refusals count toward CPA's lockout, so it should not read as the
        * page's main action while the password field is still empty. */}
      <a
        href={consoleHref}
        target="_top"
        className="rounded-[8px] border border-line px-[13px] py-[10px] text-center text-[12.5px]
          font-medium text-ink-2 transition-colors hover:border-accent hover:text-ink"
      >
        Sign in to the CPA console
      </a>
      <p className="mt-[7px] text-[11.5px] leading-[1.55] text-ink-3">Tick Remember password, then reopen this page.</p>

      {onCancel && (
        <button
          type="button"
          onClick={onCancel}
          className="mt-[22px] self-center text-[12px] text-ink-3 underline-offset-[3px] transition-colors
            hover:text-ink hover:underline"
        >
          Back to the dashboard
        </button>
      )}
    </main>
  )
}
