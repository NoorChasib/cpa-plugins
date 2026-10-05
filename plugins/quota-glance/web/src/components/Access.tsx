import { useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"

import type { ConsoleLatch } from "../lib/access"
import { useNowSeconds } from "../lib/now"
import { type AccessState, consoleRetryBlocked, retryConsole, signOut } from "../lib/session"
import { formatClock } from "../lib/time"

/**
 * The header's key: what this browser signs in with, and the controls for it.
 *
 * The page has two ways in — the dashboard password saved here, and the CPA
 * console's remembered session — and tries them in that order on every
 * request. Neither is visible anywhere else on the page, which is fine while
 * they work and baffling when one stops: a console key CPA refused is held
 * back for good, and the only way to hand it over again is the button here.
 */

/** A key, at 16px, drawn the way every other mark on this page is: inline. */
function KeyIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <circle cx="5.4" cy="10.6" r="3" stroke="currentColor" strokeWidth="1.5" />
      <path
        d="M7.6 8.4 13.6 2.4M11.4 4.6l1.7 1.7M9.6 6.4l1.4 1.4"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  )
}

/** Which way a door is, as the dot beside its name: open, refused, or absent. */
type Tone = "open" | "shut" | "none"

const DOT: Record<Tone, string> = { open: "bg-good", shut: "bg-crit", none: "bg-ink-4" }

function Group({ title, tone, children }: { title: string; tone: Tone; children: React.ReactNode }) {
  return (
    <section className="mt-[14px] rounded-[10px] border border-line bg-band px-[13px] pb-[12px] pt-[10px]">
      <h4 className="flex items-center gap-[7px] text-[12px] font-semibold text-ink">
        <span className={`size-[6px] shrink-0 rounded-full ${DOT[tone]}`} aria-hidden="true" />
        {title}
      </h4>
      {children}
    </section>
  )
}

function Text({ children }: { children: React.ReactNode }) {
  return <p className="mt-[6px] text-[12px] leading-[1.6] text-ink-2">{children}</p>
}

function Actions({ children }: { children: React.ReactNode }) {
  return <div className="mt-[10px] flex flex-wrap gap-[7px]">{children}</div>
}

/**
 * The console session, in the words its state calls for. A latch is shown by
 * its reason, which is what CPA said; the advice differs for each, because a
 * wrong key, a locked-out address and a server that refuses remote sessions
 * are fixed in three different places.
 */
function consoleText(state: AccessState, latch: ConsoleLatch | null): string {
  if (!state.consoleFound) {
    return "None found in this browser. Sign in to the CPA console on this address with Remember password ticked to use it here."
  }
  if (latch === null) {
    return state.reading === "token" ? "Found. Used only when no dashboard password is saved." : "In use."
  }
  switch (latch.reason) {
    case "refused":
      return "CPA refused it, so this page stopped using it. Sign in to the console again to replace it."
    case "banned":
      // A ban always carries its end; an older record without one is read as
      // CPA's default, thirty minutes from the refusal.
      return `CPA is locking this address out of management until about ${formatClock(latch.until ?? latch.at + 1800)}. This page will not try before then.`
    case "remote":
      return "CPA accepts console sessions only on the machine it runs on. Use the dashboard password here."
    case "off":
      return "CPA’s management API is switched off on this server. Use the dashboard password here."
    default:
      return `Something in front of CPA refused it (HTTP ${latch.status}).`
  }
}

/**
 * The dialog, mounted while it is open, in the confirmation's pattern: a
 * native modal for the focus trap, the inert page and Escape, and focus handed
 * back to the key when it unmounts.
 */
function AccessDialog({
  state,
  onClose,
  onSignIn,
}: {
  state: AccessState
  onClose: () => void
  onSignIn: () => void
}) {
  const dialog = useRef<HTMLDialogElement>(null)
  const opener = useRef(document.activeElement)
  const queryClient = useQueryClient()
  const now = useNowSeconds()

  useEffect(() => {
    const element = dialog.current
    if (element && !element.open) element.showModal()
    const target = opener.current
    return () => {
      if (target instanceof HTMLElement && target.isConnected) target.focus()
    }
  }, [])

  const latch = state.consoleLatch
  const tokenTone: Tone = state.tokenRefused ? "shut" : state.tokenSaved ? "open" : "none"
  const consoleTone: Tone = latch ? "shut" : state.consoleFound ? (state.reading === "console" ? "open" : "none") : "none"
  // Ticks with the page's clock, so the button comes back on its own at the
  // minute CPA said the ban lifts.
  const blocked = consoleRetryBlocked(latch, now)

  const enterPassword = () => {
    onClose()
    onSignIn()
  }

  return (
    <dialog
      ref={dialog}
      aria-labelledby="access-title"
      className="qg-dialog"
      // Escape closes it, and so does a click outside it: nothing here is lost
      // by dismissing, and every control inside acts on its own press.
      onCancel={onClose}
      onClick={(event) => {
        // A click on the backdrop is delivered to the dialog itself, at a point
        // outside its box; one on its padding is too, inside it.
        if (event.target !== event.currentTarget) return
        const box = event.currentTarget.getBoundingClientRect()
        const inside =
          event.clientX >= box.left && event.clientX <= box.right && event.clientY >= box.top && event.clientY <= box.bottom
        if (!inside) onClose()
      }}
    >
      <h3 id="access-title" className="text-[14px] font-semibold text-ink">
        Access on this browser
      </h3>

      <Group title="Dashboard password" tone={tokenTone}>
        {!state.tokenSaved ? (
          <>
            <Text>Not saved in this browser.</Text>
            <Actions>
              <button type="button" className="qg-btn" onClick={enterPassword}>
                Enter password
              </button>
            </Actions>
          </>
        ) : state.tokenRefused ? (
          <>
            <Text>Saved, but the plugin refused it. Enter the current dashboard password.</Text>
            <Actions>
              <button type="button" className="qg-btn qg-btn-go" onClick={enterPassword}>
                Enter password
              </button>
              <button type="button" className="qg-btn" onClick={signOut}>
                Sign out
              </button>
            </Actions>
          </>
        ) : (
          <>
            <Text>Saved in this browser. Used to read this page and to use banked resets.</Text>
            <Actions>
              <button type="button" className="qg-btn" onClick={signOut}>
                Sign out
              </button>
            </Actions>
          </>
        )}
      </Group>

      <Group title="CPA console session" tone={consoleTone}>
        <Text>{consoleText(state, latch)}</Text>
        {!state.consoleFound && (
          <Actions>
            {/* _top: inside the CPA sidebar this page is a frame, and the
              * console belongs in the window, not in the frame. */}
            <a className="qg-btn" href={state.consoleHref} target="_top">
              Open the CPA console
            </a>
          </Actions>
        )}
        {latch && (
          <>
            <Actions>
              <button
                type="button"
                className="qg-btn"
                disabled={blocked}
                onClick={() => {
                  // One click, one ask. A second refusal latches the key again
                  // before anything else can send it. The dialog closes on the
                  // click because the answer is the page's own: the figures,
                  // or the banner or sign-in screen saying it was refused again.
                  // Over the sign-in screen it would close anyway, when the
                  // read starts and the screen gives way to the loading one.
                  if (retryConsole()) {
                    onClose()
                    void queryClient.invalidateQueries({ queryKey: ["summary"] })
                  }
                }}
              >
                Try it again
              </button>
            </Actions>
            <p className="mt-[8px] text-[11px] leading-[1.55] text-ink-3">
              Each refused try counts toward CPA’s limit of 5 failed sign-ins, after which it locks this address out of
              management for 30 minutes.
            </p>
          </>
        )}
      </Group>

      <p className="mt-[14px] text-[11px] leading-[1.6] text-ink-3">
        Saved sign-ins stay in this browser’s storage for <span className="text-ink-2">{window.location.host}</span> (in
        the menu bar app, in the app’s own storage). Anyone who can use this browser profile can view this page and use
        banked resets. Signing out here does not sign you out of the CPA console.
      </p>

      <div className="mt-[16px] flex justify-end">
        <button type="button" className="qg-btn" onClick={onClose}>
          Close
        </button>
      </div>
    </dialog>
  )
}

/**
 * The key in the header, and the dialog behind it.
 *
 * It carries a dot when a credential this browser holds has been refused and
 * nothing else is standing in for it: a saved password the plugin turned
 * down, or a console session CPA did while it was the way in. A refused
 * console key behind a working password is left unmarked — nothing on the
 * page depends on it.
 */
export function Access({ state, onSignIn }: { state: AccessState; onSignIn: () => void }) {
  const [open, setOpen] = useState(false)
  const attention = state.tokenRefused || (state.consoleLatch !== null && state.reading !== "token")

  return (
    <>
      <button
        type="button"
        className={`qg-key ${attention ? "is-attention" : ""}`}
        aria-label="Access"
        title="Access on this browser"
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
      >
        <KeyIcon />
      </button>
      {open && <AccessDialog state={state} onClose={() => setOpen(false)} onSignIn={onSignIn} />}
    </>
  )
}
