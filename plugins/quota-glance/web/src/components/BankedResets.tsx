import { useEffect, useRef, useState } from "react"

import { useNowSeconds } from "../lib/now"
import { type AccountName, shortName } from "../lib/pool"
import { canRedeemHere, describeError, describeOutcome, fellShort, mayHaveSpent, partlyApplied, redeemReset } from "../lib/redeem"
import { formatDuration } from "../lib/time"
import type { Credential } from "../lib/types"

/**
 * Banked rate-limit resets, as one card at the foot of its provider's section
 * with a row per account holding any.
 *
 * A card of their own rather than a line on any window card because a banked
 * reset is a fact about the account, not about a window: spending one clears
 * the session and weekly windows together. Repeating it on every card would
 * put three buttons on screen for one irreversible action. Last in the
 * section, below everything it would change, so the figures are read before
 * the button that moves them.
 *
 * Nothing at all is drawn when no credential in the provider holds one, which
 * is the ordinary case.
 */

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

/**
 * What spending one does, per provider, in the two places that say so: the
 * card's subtitle and the dialog that asks.
 *
 * Codex and Claude clear different things under different rules, and a
 * sentence vague enough to cover both would tell the reader nothing they could
 * check afterwards. `rule` is the condition the provider sets on spending one
 * at all, which Codex does not have and Claude does — said in the dialog, so
 * a refusal afterwards is not the first the reader hears of it.
 */
type Effect = { subtitle: string; clears: string; rule?: string }

const EFFECTS: Record<string, Effect> = {
  codex: {
    subtitle: "Clears the session and weekly windows when spent.",
    clears: "It clears that account’s session and weekly Codex windows and moves its weekly reset date.",
  },
  claude: {
    subtitle: "Clears the 5-hour and weekly limits, once at a limit.",
    clears: "It clears that account’s Claude usage limits — the 5-hour and the weekly.",
    rule: "Claude allows a reset only once the account has hit a limit, and makes it wait out a cooldown between resets.",
  },
}

const ANY_PROVIDER: Effect = {
  subtitle: "Clears the account’s windows when spent.",
  clears: "It clears that account’s current windows.",
}

const effectOf = (provider: string): Effect => EFFECTS[provider] ?? ANY_PROVIDER

/**
 * The provider's reason, at the last poll, that a press would be refused.
 *
 * Printed on the row and never instead of its button. The reading can be a poll old
 * and the plugin checks the provider afresh before spending anything, so hiding
 * the button on its say-so would strand a reset the reader could use the moment
 * the account reaches its limit. A cooldown that has run out since the poll
 * says nothing rather than counting past zero, and a reason this bundle does
 * not know says nothing either: a raw code beside an irreversible action reads
 * as an error.
 */
function holdText(hold: string, holdUntilEpoch: number | null, now: number): string | null {
  switch (hold) {
    case "notLimited":
      return "usable once at a limit"
    case "cooldown": {
      if (holdUntilEpoch === null) return "cooling down"
      const remaining = holdUntilEpoch - now
      if (remaining <= 0) return null
      return `cooling down · ${formatDuration(remaining)}`
    }
    case "paused":
      return "paused"
    case "ineligible":
      return "not eligible right now"
    default:
      return null
  }
}

/**
 * How close the credit is to lapsing, long ("expires in 5d 2h") or, beside a
 * hold that already fills the line, short ("exp 5d 2h").
 *
 * A Codex banked reset expires thirty days after it is granted and a Claude
 * grant on an end date of its own, and the count alone cannot say that one is
 * about to, so a deadline inside a week is called out in warning ink. An
 * undated credit says so rather than implying it is safe.
 */
function expiryOf(
  expiresAtEpoch: number | null,
  now: number,
  short: boolean,
): { text: string; tone: string } {
  if (expiresAtEpoch === null) return { text: short ? "no expiry" : "no expiry reported", tone: "" }
  const remaining = expiresAtEpoch - now
  if (remaining <= 0) return { text: "expired", tone: "text-crit" }
  const span = formatDuration(remaining)
  return { text: short ? `exp ${span}` : `expires in ${span}`, tone: remaining <= 7 * 86400 ? "text-warn" : "" }
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
  // Whatever had focus when the dialog was asked for — the button, from a
  // keyboard or a click. Read on the first render, before showModal moves
  // focus inside.
  const opener = useRef(document.activeElement)
  const effect = effectOf(credential.provider)

  useEffect(() => {
    const element = dialog.current
    if (element && !element.open) element.showModal()
    // The dialog leaves by unmounting rather than by close(), which skips the
    // platform's own return of focus, so it is handed back here: otherwise a
    // keyboard reader who cancels is dropped at the top of the page. Not when
    // the button has gone — a spend that took the account's last reset removes
    // it — which leaves focus where the platform puts it.
    const target = opener.current
    return () => {
      if (target instanceof HTMLElement && target.isConnected) target.focus()
    }
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
        {/* A count of one is said as "the only" one: "one of … 1 banked
          * reset" is a sentence nobody would write, and an account holding a
          * single Claude grant is the common case. */}
        This spends {count === 1 ? "the only banked reset" : `one of the ${plural(count, "banked reset")}`} that{" "}
        {credential.email || credential.id} holds, straight away. {effect.clears}
        {effect.rule && ` ${effect.rule}`}
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

/**
 * What a press came to, kept against the address it was made on.
 *
 * The address is copied in rather than looked up, because the press is often
 * what takes the account out of the list: once quota-cache polls and sees the
 * last reset gone, the row goes, and the outcome must not go with it.
 */
type Notice = { id: string; who: string; tone: Tone; text: string }

/**
 * How a notice is marked: the press did what it was for; it was refused and
 * nothing was spent; the answer was lost and the reset may be gone; or the
 * reset was spent and CPA may still be skipping the account. The middle two
 * used to share the refusal's red, which left "may have been applied" looking
 * exactly like "nothing was spent" at a glance.
 */
type Tone = "ok" | "bad" | "unknown" | "partial"

/** One provider's rows, the press in progress, and what earlier presses came to. */
export interface BankedResets {
  holding: Credential[]
  asking: Credential | null
  busy: boolean
  notices: Notice[]
  ask: (credential: Credential) => void
  cancel: () => void
  confirm: () => Promise<void>
  dismiss: (id: string) => void
}

/**
 * The state behind a provider's banked resets, shared by the three places it
 * shows: the rows, the outcome lines under them, and the dialog.
 *
 * One dialog per provider is enough. It is modal and stays open until its
 * press has an answer, so a second button cannot be pressed while one is in
 * flight.
 */
export function useBankedResets(credentials: Credential[], onRedeemed: () => void): BankedResets {
  // Every account with any, in the catalog's order — no other filter. See the
  // note on ResetRow.
  const holding = credentials.filter((credential) => credential.resetCredits !== null)
  const [asking, setAsking] = useState<Credential | null>(null)
  const [busy, setBusy] = useState(false)
  const [notices, setNotices] = useState<Notice[]>([])

  function report(credential: Credential, tone: Tone, text: string) {
    const notice = { id: credential.id, who: credential.email || credential.id, tone, text }
    // A second press on the same account replaces what the first said rather
    // than stacking under it; presses on other accounts keep theirs.
    setNotices((current) => [...current.filter((item) => item.id !== credential.id), notice])
  }

  async function confirm() {
    const credential = asking
    if (!credential) return
    setBusy(true)
    try {
      const result = await redeemReset(credential.id)
      const tone = fellShort(result) ? "bad" : partlyApplied(result) ? "partial" : "ok"
      report(credential, tone, describeOutcome(result, credential.provider))
      // Ask for a fresh document. It will not show a smaller count until
      // quota-cache polls again — the message says so — but everything else on
      // the page stays current.
      onRedeemed()
    } catch (error) {
      report(credential, mayHaveSpent(error) ? "unknown" : "bad", describeError(error, credential.provider))
      // A refusal that shut the last way in also stopped the poll, which would
      // leave these figures on screen looking current. Asking once more costs
      // nothing — with no door open the read fails before the network — and
      // that failure is what puts the refused banner over them.
      if (!canRedeemHere()) onRedeemed()
    } finally {
      setBusy(false)
      setAsking(null)
    }
  }

  return {
    holding,
    // The latest reading of the account being asked about, so a poll landing
    // while the dialog is open updates its count. The copy taken at the press
    // stands in if the poll has dropped the account, which keeps a dialog with
    // a press in flight on screen until it has its answer.
    asking: asking ? (holding.find((credential) => credential.id === asking.id) ?? asking) : null,
    busy,
    notices,
    ask: setAsking,
    cancel: () => setAsking(null),
    confirm,
    dismiss: (id) => setNotices((current) => current.filter((item) => item.id !== id)),
  }
}

/** The circular arrow of a reset ready to press. Inline, like every mark here. */
function ResetIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path d="M13.2 8.4A5.2 5.2 0 1 1 11.6 4.3" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
      <path d="M12.4 1.9v2.9H9.5" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

/** A clock, for a reset the provider is holding back for now. */
function HoldIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <circle cx="8" cy="8" r="5.6" stroke="currentColor" strokeWidth="1.5" />
      <path d="M8 5v3.2l2.1 1.3" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

/**
 * One account's banked resets: who, how many, the provider's hold and the
 * deadline — and, when this page can spend one, the "Use one" button.
 *
 * A button, not a link or a clickable div, and only when both judgements say
 * yes. The server decides whether the credential can be spent at all; the
 * browser decides whether it still has a way in — the dashboard password or a
 * console session neither of which has been refused — because a press with
 * none could only be refused, and one with a console key CPA has already
 * refused would cost another failed management sign-in. When either says no
 * the row still shows the count and the deadline, and has no button: a greyed
 * button invites a press that explains nothing.
 *
 * Nothing else is consulted. Every account holding a reset gets its own row
 * and, when both say yes, its own button — whatever its routing status or the
 * provider's hold — because which account to spend on is the reader's choice,
 * and a cooling-down account is the one most worth spending on.
 */
function ResetRow({
  credential,
  name,
  busy,
  onAsk,
}: {
  credential: Credential
  name: AccountName
  busy: boolean
  onAsk: () => void
}) {
  const now = useNowSeconds()
  const credits = credential.resetCredits
  if (!credits) return null
  // `?? ""` and `?? null` for a document from a plugin older than the hold.
  const hold = holdText(credits.hold ?? "", credits.holdUntilEpoch ?? null, now)
  const cooling = (credits.hold ?? "") === "cooldown" && hold !== null
  // Short beside a hold, which already fills most of the line.
  const expiry = expiryOf(credits.expiresAtEpoch, now, hold !== null)
  const offered = credits.redeemable && canRedeemHere()
  const address = credential.email || credential.id
  const long = expiryOf(credits.expiresAtEpoch, now, false).text
  // Everything the row says, whole, for the hover and for assistive
  // technology.
  const detail = `${plural(credits.availableCount, "reset")} banked, ${long}${hold ? `, ${hold}` : ""}`

  return (
    <div className={`qg-brow ${hold || !offered ? "is-held" : ""}`} title={`${address}: ${detail}`}>
      <span className="qg-pico">{cooling ? <HoldIcon /> : <ResetIcon />}</span>
      {/* The qualifier never gives way: when two accounts share a local part
        * it is the half that tells their rows apart. */}
      <span className="qg-bwho">
        <span className="qg-plocal">{name.local}</span>
        {name.qualifier && <span className="qg-pqual">{name.qualifier}</span>}
        <span className="qg-pn">· {plural(credits.availableCount, "reset")}</span>
      </span>
      <span className="qg-bst">
        {hold && (
          <>
            <span className="qg-hold">{hold}</span>
            {" · "}
          </>
        )}
        <span className={expiry.tone}>{expiry.text}</span>
      </span>
      {offered && (
        <button
          type="button"
          className="qg-use"
          aria-haspopup="dialog"
          aria-label={`Use one of ${address}'s banked resets: ${detail}`}
          disabled={busy}
          onClick={onAsk}
        >
          {/* The verb, said on the button itself: "Use one" is what the
            * page's own messages tell the reader to press — after an unknown
            * outcome, to retry the same claim. */}
          Use one
          <svg viewBox="0 0 10 10" fill="none" aria-hidden="true">
            <path
              d="M3.5 1.5 7 5 3.5 8.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>
      )}
    </div>
  )
}

/**
 * The card: what spending one does on this provider, a row per account
 * holding any, and what each press came to. Present while any account holds
 * one or a press still has something to report — the last reset spent takes
 * its row away, and the outcome must not go with it.
 */
export function BankedResetsCard({ state, names }: { state: BankedResets; names: Map<string, AccountName> }) {
  if (state.holding.length === 0 && state.notices.length === 0 && !state.asking) return null
  // One provider per card: ProviderSection hands over only its own. The
  // dialog's credential stands in once the last row has gone.
  const effect = effectOf(state.holding[0]?.provider ?? state.asking?.provider ?? "")
  // Said once for the card rather than on every row: it is a fact about this
  // browser's session, and it is the same for every account in it. With
  // figures on screen it means every way in was refused since they arrived.
  const signedOut = !canRedeemHere() && state.holding.some((credential) => credential.resetCredits?.redeemable)
  return (
    <article className="qg-win qg-bank" aria-label="Banked resets">
      <div className="qg-whead">
        <h3 className="qg-wtitle">Banked resets</h3>
      </div>
      <p className="qg-bsubt">{effect.subtitle}</p>
      {state.holding.length > 0 && (
        <div className="qg-brows">
          {state.holding.map((credential) => (
            <ResetRow
              key={credential.id}
              credential={credential}
              name={names.get(credential.id) ?? { local: shortName(credential, credential.id), qualifier: "" }}
              busy={state.busy}
              onAsk={() => state.ask(credential)}
            />
          ))}
        </div>
      )}
      {signedOut && <p className="qg-pnote">To spend a reset from here, sign in again.</p>}
      <ResetNotices state={state} />
      <ResetConfirm state={state} />
    </article>
  )
}

/** The mark on a notice whose outcome is unknown. Inline, like every mark here. */
function UnknownMark() {
  return (
    <svg className="qg-notice-mark" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path d="M8 1.8 15 14H1z" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
      <path d="M8 6.2v3.6" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      <circle cx="8" cy="11.9" r="0.95" fill="currentColor" />
    </svg>
  )
}

const TONE_CLASS: Record<Tone, string> = { ok: "is-ok", bad: "is-bad", unknown: "is-unknown", partial: "is-partial" }

/**
 * Where focus goes when a notice's own dismiss button is pressed, since that
 * button is about to go: the next notice's, else the one before, else the
 * card's first "Use one", else the provider's heading. Without this a keyboard
 * reader who dismisses a notice is dropped back at the top of the page.
 */
function focusAfterDismiss(button: HTMLElement) {
  const region = button.closest(".qg-notices")
  const buttons = region ? [...region.querySelectorAll<HTMLElement>(".qg-notice-x")] : []
  const at = buttons.indexOf(button)
  const sibling = buttons[at + 1] ?? buttons[at - 1]
  if (sibling) {
    sibling.focus()
    return
  }
  const section = button.closest("section")
  const use = section?.querySelector<HTMLElement>("button.qg-use:not(:disabled)")
  if (use) {
    use.focus()
    return
  }
  const heading = section?.querySelector<HTMLElement>("h2")
  if (heading) {
    heading.tabIndex = -1
    heading.focus()
  }
}

/**
 * What each press came to, right under the rows that made them.
 *
 * One live region, present whenever the provider has rows or anything to
 * report, so an outcome is announced when it arrives rather than when a region
 * is created around it — which some readers miss. Each line stays until it is
 * dismissed or its account is pressed again: an outcome that may say a reset
 * is spent is not something to time out.
 */
function ResetNotices({ state }: { state: BankedResets }) {
  if (state.holding.length === 0 && state.notices.length === 0) return null
  return (
    <div role="status" className="qg-notices">
      {state.notices.map((notice) => (
        <p key={notice.id} className={`qg-notice ${TONE_CLASS[notice.tone]}`}>
          {(notice.tone === "unknown" || notice.tone === "partial") && <UnknownMark />}
          <span className="min-w-0">
            <b className="qg-notice-who">{notice.who}</b> {notice.text}
          </span>
          <button
            type="button"
            className="qg-notice-x"
            aria-label={`Dismiss the message about ${notice.who}`}
            onClick={(event) => {
              focusAfterDismiss(event.currentTarget)
              state.dismiss(notice.id)
            }}
          >
            <svg viewBox="0 0 10 10" aria-hidden="true">
              <path d="M2 2 8 8M8 2 2 8" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
            </svg>
          </button>
        </p>
      ))}
    </div>
  )
}

/** The dialog for the row being pressed, while one is. */
function ResetConfirm({ state }: { state: BankedResets }) {
  const credential = state.asking
  if (!credential?.resetCredits) return null
  return (
    <ConfirmDialog
      credential={credential}
      count={credential.resetCredits.availableCount}
      busy={state.busy}
      onConfirm={() => void state.confirm()}
      onCancel={state.cancel}
    />
  )
}
