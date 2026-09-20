import { useEffect, useRef, useState } from "react"

import { useNowSeconds } from "../lib/now"
import { canRedeemHere, describeError, describeOutcome, redeemReset } from "../lib/redeem"
import { formatDuration } from "../lib/time"
import type { Credential } from "../lib/types"

/**
 * Banked rate-limit resets, one block per provider that has any.
 *
 * It sits above the window cards rather than inside them because a banked reset
 * is a fact about the account, not about a window: spending one clears the
 * session and weekly windows together. Repeating it on every card would put
 * three buttons on screen for one irreversible action.
 *
 * The whole block is absent when no credential in the provider holds one, which
 * is the ordinary case and leaves the page exactly as it was.
 */

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

/**
 * How close the credit is to lapsing.
 *
 * A banked reset expires thirty days after it is granted and the count alone
 * cannot say that one is about to, so a deadline inside a week is called out in
 * warning ink. An undated credit says so rather than implying it is safe.
 */
function Expiry({ expiresAtEpoch, now }: { expiresAtEpoch: number | null; now: number }) {
  if (expiresAtEpoch === null) return <span className="text-ink-3">no expiry reported</span>
  const remaining = expiresAtEpoch - now
  if (remaining <= 0) return <span className="text-crit">expired</span>
  const soon = remaining <= 7 * 86400
  return (
    <span className={soon ? "text-warn" : "text-ink-3"}>expires in {formatDuration(remaining)}</span>
  )
}

/**
 * The confirmation, in a native modal dialog.
 *
 * `showModal` is used rather than a hand-built overlay because it brings the
 * focus trap, the inert background and the Escape key with it — three things
 * that a confirmation for an irreversible action should not be re-implementing
 * slightly wrong. The default Escape close is allowed: dismissing is the safe
 * direction, and the only way out that spends anything is the button.
 */
function ConfirmDialog({
  credential,
  count,
  busy,
  onConfirm,
  onCancel,
}: {
  credential: Credential
  count: number
  busy: boolean
  onConfirm: () => void
  onCancel: () => void
}) {
  const dialog = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    const element = dialog.current
    if (element && !element.open) element.showModal()
  }, [])

  return (
    <dialog
      ref={dialog}
      aria-labelledby="redeem-title"
      className="qg-dialog"
      onCancel={(event) => {
        // A request is in flight and the credit may already be spent; closing
        // the dialog now would hide the only place the outcome is reported.
        if (busy) event.preventDefault()
        else onCancel()
      }}
    >
      <h3 id="redeem-title" className="text-[14px] font-semibold text-ink">
        Use a banked reset?
      </h3>
      <p className="mt-[10px] text-[12.5px] leading-[1.65] text-ink-2">
        This spends one of {credential.email || credential.id}&rsquo;s {plural(count, "banked reset")} straight away. It
        clears that account&rsquo;s session and weekly Codex windows and moves its weekly reset date.
      </p>
      {/* The sentence that matters. A banked reset is an entitlement the
        * account already holds, so spending it is not free capacity and there
        * is no way to take it back. */}
      <p className="mt-[9px] text-[12.5px] leading-[1.65] text-ink-2">
        It cannot be undone, and it does not add allowance — it brings your existing allowance forward.
      </p>

      <div className="mt-[18px] flex justify-end gap-[9px]">
        <button type="button" className="qg-btn" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
        <button type="button" className="qg-btn qg-btn-go" disabled={busy} onClick={onConfirm}>
          {busy ? "Redeeming…" : "Use one reset"}
        </button>
      </div>
    </dialog>
  )
}

type Notice = { tone: "ok" | "bad"; text: string }

function CreditRow({ credential, onDone }: { credential: Credential; onDone: () => void }) {
  const now = useNowSeconds()
  const credits = credential.resetCredits
  const [asking, setAsking] = useState(false)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<Notice | null>(null)
  if (!credits) return null

  async function confirm() {
    setBusy(true)
    try {
      const result = await redeemReset(credential.id)
      setNotice({ tone: result.outcome === "noCredit" ? "bad" : "ok", text: describeOutcome(result) })
      // Ask for a fresh document. It will not show a smaller count until
      // quota-cache polls again — the message says so — but everything else on
      // the page stays current.
      onDone()
    } catch (error) {
      setNotice({ tone: "bad", text: describeError(error) })
    } finally {
      setBusy(false)
      setAsking(false)
    }
  }

  return (
    <div className="cred-credit">
      <div className="min-w-0">
        <div className="truncate text-[12.5px] text-ink">{credential.email || credential.id}</div>
        <div className="num mt-[3px] text-[11px]">
          <span className="text-ink-2">{plural(credits.availableCount, "reset")} banked</span>
          <span className="text-ink-4"> · </span>
          <Expiry expiresAtEpoch={credits.expiresAtEpoch} now={now} />
        </div>
      </div>

      {/* Absent, not disabled, when this credential cannot be redeemed against:
        * a greyed control invites a click that explains nothing, and the count
        * is still worth showing on its own.
        *
        * Two separate judgements. The server decides whether the credential can
        * be spent at all; the browser decides whether it is the kind of session
        * that can spend it, because CPA dispatches only GET to the resource
        * route the token door uses. */
      }
      {credits.redeemable && canRedeemHere() && (
        <button type="button" className="qg-btn qg-btn-go shrink-0" disabled={busy} onClick={() => setAsking(true)}>
          Use one
        </button>
      )}
      {credits.redeemable && !canRedeemHere() && (
        <span className="shrink-0 text-[10.5px] text-ink-3">sign in to the console to use</span>
      )}

      {notice && (
        <p
          role="status"
          className={`col-span-full text-[11.5px] leading-[1.55] ${notice.tone === "ok" ? "text-ink-2" : "text-crit"}`}
        >
          {notice.text}
        </p>
      )}

      {asking && (
        <ConfirmDialog
          credential={credential}
          count={credits.availableCount}
          busy={busy}
          onConfirm={() => void confirm()}
          onCancel={() => setAsking(false)}
        />
      )}
    </div>
  )
}

export function BankedResets({ credentials, onRedeemed }: { credentials: Credential[]; onRedeemed: () => void }) {
  const holding = credentials.filter((credential) => credential.resetCredits !== null)
  if (holding.length === 0) return null

  return (
    <div className="mb-[10px] rounded-[12px] border border-line bg-card px-4 pb-[10px] pt-[13px]">
      <div className="mb-[2px] flex items-baseline gap-[8px] border-b border-line pb-[9px]">
        <span className="text-[13px] font-[550] text-ink">Banked resets</span>
        <span className="text-[11px] text-ink-3">clears this account&rsquo;s windows when spent</span>
      </div>
      {holding.map((credential) => (
        <CreditRow key={credential.id} credential={credential} onDone={onRedeemed} />
      ))}
    </div>
  )
}
