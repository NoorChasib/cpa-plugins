import { useEffect, useId, useRef, useState } from "react"

import { useChecks } from "../lib/checking"
import { isChecking, startCheck } from "../lib/checks"
import { useNowSeconds } from "../lib/now"
import {
  CLAUDE_RULE,
  countOf,
  kindWords,
  KINDS_UNREPORTED,
  opening,
  othersSentence,
  spendsSentence,
} from "../lib/outcome"
import { type AccountName, shortName } from "../lib/pool"
import {
  canRedeemHere,
  describeError,
  describeOutcome,
  fellShort,
  mayHaveSpent,
  partlyApplied,
  RedeemError,
  redeemReset,
} from "../lib/redeem"
import {
  anyRedeemable,
  groupOf,
  isSpendableKind,
  type KindCount,
  type KindGroup,
  kindEntry,
  layoutResets,
  othersHeld,
  UNKNOWN_KIND,
} from "../lib/resetKinds"
import { formatDuration } from "../lib/time"
import type { Credential, ResetKindCredits } from "../lib/types"
import { CheckingPill } from "./CheckingPill"

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
 * Claude's come in kinds that do different things — a full reset clears the
 * 5-hour and weekly limits, a Fable reset only gives back to the weekly limit
 * what Fable used — so on Claude the rows are grouped by kind, one group per
 * kind any account holds, each with its own mark, heading, effect and button.
 * An account holding two kinds has a row in each group. Nothing on the card
 * spends "a reset": every Claude button and dialog names the kind it spends,
 * and the press carries it, so the plugin spends that kind or nothing. Codex's
 * are all one sort and keep the rows and the "Use one" they always had.
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
 * a refusal afterwards is not the first the reader hears of it. On Claude
 * what one clears depends on its kind, which each group and the dialog say in
 * the kind's own words (outcome.ts KIND_WORDS); the card's subtitle only says
 * that they differ.
 */
type Effect = { subtitle: string; clears: string; rule?: string }

const EFFECTS: Record<string, Effect> = {
  codex: {
    subtitle: "Clears the session and weekly windows when spent.",
    clears: "It clears that account’s session and weekly Codex windows and moves its weekly reset date.",
  },
  claude: {
    subtitle: "Grouped by kind: each refills different limits.",
    clears: "",
    rule: CLAUDE_RULE,
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
 * The class that gives an element its kind's colour: `--kind` and
 * `--kind-ink`, which the mark, the heading, the button and the dialog read.
 * Only the five groups resetKinds.ts sorts into, so a kind newer than this
 * bundle never names a class nobody defined.
 */
const kindClass = (kind: string) => `qg-k-${groupOf(kind)}`

/**
 * One mark per kind, so two kinds never look alike even to a reader who
 * cannot tell their colours apart: the circular arrow of a reset that clears
 * everything, a spark for Fable, a calendar for the weekly limits, an
 * hourglass for the 5-hour one, and a question for a kind this bundle does not
 * know or Quota Cache did not report. Always beside the kind's name in words,
 * never instead of it.
 */
function KindIcon({ kind }: { kind: string }) {
  switch (groupOf(kind)) {
    case "full":
      return <ResetIcon />
    case "fable":
      return (
        <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path
            d="M8 1.9 9.4 6.6 14.1 8 9.4 9.4 8 14.1 6.6 9.4 1.9 8 6.6 6.6Z"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinejoin="round"
          />
        </svg>
      )
    case "weekly":
      return (
        <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <rect x="2.3" y="3.3" width="11.4" height="10.4" rx="2" stroke="currentColor" strokeWidth="1.5" />
          <path d="M2.6 6.9h10.8M5.5 1.8v2.6M10.5 1.8v2.6" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      )
    case "session":
      return (
        <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path d="M3.8 2.2h8.4M3.8 13.8h8.4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          <path
            d="M5 2.2v1.4C5 5.5 8 6.2 8 8s-3 2.5-3 4.4v1.4M11 2.2v1.4C11 5.5 8 6.2 8 8s3 2.5 3 4.4v1.4"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinejoin="round"
          />
        </svg>
      )
    default:
      return (
        <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <circle cx="8" cy="8" r="5.9" stroke="currentColor" strokeWidth="1.5" />
          <path
            d="M6.3 6.4a1.8 1.8 0 1 1 2.6 1.6c-.6.3-.9.7-.9 1.3v.2"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
          <circle cx="8" cy="11.5" r="0.9" fill="currentColor" />
        </svg>
      )
  }
}

/** A kind's mark: its icon on its colour. Decoration beside the kind's name, never alone. */
function KindMark({ kind }: { kind: string }) {
  return (
    <span className="qg-kmark">
      <KindIcon kind={kind} />
    </span>
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
 *
 * For a Claude reset it is the last place to catch the wrong kind, so it says
 * the kind four times over: the title, the mark with the kind's effect, how
 * many of that kind the account holds, and — when the account holds another
 * kind too — that those stay banked. The confirm button is the kind's own.
 */
function ConfirmDialog({
  credential,
  kind,
  count,
  others,
  busy,
  onConfirm,
  onCancel,
}: {
  credential: Credential
  /** The kind of Claude reset to spend; "" for Codex. */
  kind: string
  count: number
  /** What else the account holds, by kind, which this press leaves alone. */
  others: KindCount[]
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
  const who = credential.email || credential.id

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

  const words = kind === "" ? null : kindWords(kind)
  const kept = othersSentence(others)

  return (
    <dialog
      ref={dialog}
      aria-labelledby="redeem-title"
      className={`qg-dialog ${words ? kindClass(kind) : ""}`}
      onCancel={(event) => {
        // A request is in flight and the credit may already be spent; closing
        // the dialog now would hide the only place the outcome is reported.
        if (busy) event.preventDefault()
        else onCancel()
      }}
    >
      <h3 id="redeem-title" className="text-[14px] font-semibold text-ink">
        {words ? `Use a ${words.one}?` : "Use a banked reset?"}
      </h3>
      {words ? (
        <>
          {/* The kind, as the card's group shows it, and what it does. */}
          <div className="qg-dkind">
            <KindMark kind={kind} />
            <p className="min-w-0">
              <b className="qg-dkind-name">{opening(words.one)}</b>
              <span className="qg-dkind-effect">{words.effect}</span>
            </p>
          </div>
          <p className="mt-[10px] text-[12.5px] leading-[1.65] text-ink-2">
            {spendsSentence(kind, count, who)}
            {effect.rule && ` ${effect.rule}`}
          </p>
          {kept && <p className="mt-[9px] text-[12.5px] leading-[1.65] text-ink">{kept}</p>}
        </>
      ) : (
        <p className="mt-[10px] text-[12.5px] leading-[1.65] text-ink-2">
          {/* A count of one is said as "the only" one: "one of … 1 banked
            * reset" is a sentence nobody would write, and an account holding a
            * single Claude grant is the common case. */}
          This spends {count === 1 ? "the only banked reset" : `one of the ${plural(count, "banked reset")}`} that{" "}
          {who} holds, straight away. {effect.clears}
          {effect.rule && ` ${effect.rule}`}
        </p>
      )}
      {/* The sentence that matters. A banked reset is an entitlement the
        * account already holds, so spending it is not free capacity and there
        * is no way to take it back. */}
      <p className="mt-[9px] text-[12.5px] leading-[1.65] text-ink-2">
        It cannot be undone, and it does not add allowance — it brings your existing allowance forward.
      </p>

      <div className="mt-[18px] flex justify-end gap-[9px]">
        <button type="button" className="qg-btn" disabled={busy} onClick={onCancel}>
          {words ? "Keep it" : "Cancel"}
        </button>
        <button type="button" className="qg-btn qg-btn-go" disabled={busy} onClick={onConfirm}>
          {busy ? (words ? `Using ${words.one}…` : "Redeeming…") : words ? words.button : "Use one reset"}
        </button>
      </div>
    </dialog>
  )
}

/**
 * What a press came to, kept against the address it was made on and the kind
 * it spent.
 *
 * The address is copied in rather than looked up, because the press is often
 * what takes the account out of the list: once quota-cache polls and sees the
 * last reset gone, the row goes, and the outcome must not go with it. Keyed by
 * account and kind, so a full reset's outcome does not replace the line saying
 * an earlier Fable press on the same account may have gone through.
 */
type Notice = { key: string; who: string; kind: string; tone: Tone; text: string }

/**
 * How a notice is marked: the press did what it was for; it was refused and
 * nothing was spent; the answer was lost and the reset may be gone; or the
 * reset was spent and CPA may still be skipping the account. The middle two
 * used to share the refusal's red, which left "may have been applied" looking
 * exactly like "nothing was spent" at a glance.
 */
type Tone = "ok" | "bad" | "unknown" | "partial"

/**
 * The press being asked about: the account, the kind of Claude reset ("" on
 * Codex), and that kind's figures when the dialog opened, which stand in if a
 * poll drops the kind while the dialog is still up.
 */
type Asking = { credential: Credential; kind: string; entry: ResetKindCredits | null }

/** One provider's rows, the press in progress, and what earlier presses came to. */
export interface BankedResets {
  /** The section's provider, which says what spending one does even once no account holds one. */
  provider: string
  holding: Credential[]
  asking: Asking | null
  busy: boolean
  notices: Notice[]
  ask: (credential: Credential, kind: string) => void
  cancel: () => void
  confirm: () => Promise<void>
  dismiss: (key: string) => void
}

const noticeKey = (id: string, kind: string) => JSON.stringify([id, kind])

/**
 * The state behind a provider's banked resets, shared by the three places it
 * shows: the rows, the outcome lines under them, and the dialog.
 *
 * One dialog per provider is enough. It is modal and stays open until its
 * press has an answer, so a second button cannot be pressed while one is in
 * flight.
 */
export function useBankedResets(provider: string, credentials: Credential[], onRedeemed: () => void): BankedResets {
  // Every account with any, in the catalog's order — no other filter. See the
  // note on ResetRow.
  const holding = credentials.filter((credential) => credential.resetCredits !== null)
  const [asking, setAsking] = useState<Asking | null>(null)
  const [busy, setBusy] = useState(false)
  const [notices, setNotices] = useState<Notice[]>([])

  function report(credential: Credential, kind: string, tone: Tone, text: string) {
    const key = noticeKey(credential.id, kind)
    const notice = { key, who: credential.email || credential.id, kind, tone, text }
    // A second press of the same kind on the same account replaces what the
    // first said rather than stacking under it; presses on other accounts, or
    // of another kind, keep theirs.
    setNotices((current) => [...current.filter((item) => item.key !== key), notice])
  }

  /**
   * Marks the account as being checked once the plugin says Quota Cache was
   * asked to poll it, at `askedAt` by CPA's clock. The check waits for an
   * observation from that instant on, not from the press: the instant is
   * after the spend, it is on the clock Quota Cache's observations are
   * stamped with, and a replayed answer carries the earlier press's.
   */
  function check(credential: Credential, askedAt: number | null) {
    if (askedAt !== null) startCheck(credential.id, askedAt, Math.floor(Date.now() / 1000))
  }

  async function confirm() {
    if (!asking) return
    const { credential, kind } = asking
    setBusy(true)
    try {
      // The kind is the group's, fixed when the dialog opened, and sent only
      // on Claude: the plugin spends that kind or nothing.
      const result = await redeemReset(credential.id, kind === "" ? undefined : kind)
      const tone = fellShort(result) ? "bad" : partlyApplied(result) ? "partial" : "ok"
      report(credential, kind, tone, describeOutcome(result, credential.provider))
      check(credential, result.refreshRequestedAtEpoch)
      // Ask for a fresh document. It will not show a smaller count until
      // quota-cache polls again — the message says when — but everything else
      // on the page stays current.
      onRedeemed()
    } catch (error) {
      report(credential, kind, mayHaveSpent(error) ? "unknown" : "bad", describeError(error, credential.provider, kind))
      // An unknown outcome may have spent the reset, so the plugin asked
      // quota-cache to look, and the card waits for what it finds.
      const asked = error instanceof RedeemError && error.refreshRequested
      if (asked) check(credential, error.refreshRequestedAtEpoch)
      // A refusal that shut the last way in also stopped the poll, which would
      // leave these figures on screen looking current. Asking once more costs
      // nothing — with no door open the read fails before the network — and
      // that failure is what puts the refused banner over them.
      if (asked || !canRedeemHere()) onRedeemed()
    } finally {
      setBusy(false)
      setAsking(null)
    }
  }

  // The latest reading of the account being asked about, so a poll landing
  // while the dialog is open updates its count. The copy taken at the press
  // stands in if the poll has dropped the account, or the kind, which keeps a
  // dialog with a press in flight on screen until it has its answer.
  let current: Asking | null = null
  if (asking) {
    const credential = holding.find((one) => one.id === asking.credential.id) ?? asking.credential
    const entry = asking.kind === "" ? null : (kindEntry(credential, asking.kind) ?? asking.entry)
    current = { credential, kind: asking.kind, entry }
  }

  return {
    provider,
    holding,
    asking: current,
    busy,
    notices,
    ask: (credential, kind) =>
      setAsking({ credential, kind, entry: kind === "" ? null : kindEntry(credential, kind) }),
    cancel: () => setAsking(null),
    confirm,
    dismiss: (key) => setNotices((current) => current.filter((item) => item.key !== key)),
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

/** A row's figures: the account's whole inventory on Codex, one kind's on Claude. */
type Figures = {
  availableCount: number
  expiresAtEpoch: number | null
  hold?: string
  holdUntilEpoch?: number | null
}

/**
 * One account's banked resets — of one kind, in a Claude group — who, how
 * many, the provider's hold and the deadline, and, when this page can spend
 * one, the button.
 *
 * A button, not a link or a clickable div, and only when both judgements say
 * yes. The server decides whether the credential, or on Claude this kind on
 * it, can be spent at all; the browser decides whether it still has a way in —
 * the dashboard password or a console session neither of which has been
 * refused — because a press with none could only be refused, and one with a
 * console key CPA has already refused would cost another failed management
 * sign-in. When either says no the row still shows the count and the
 * deadline, and has no button: a greyed button invites a press that explains
 * nothing.
 *
 * Nothing else is consulted. Every account holding a reset gets its own row
 * and, when both say yes, its own button — whatever its routing status or the
 * provider's hold — because which account to spend on is the reader's choice,
 * and a cooling-down account is the one most worth spending on.
 *
 * The button says which kind it spends — "Use full reset", "Use Fable reset"
 * — and its accessible name says the kind and the account, so no Claude
 * button anywhere reads as "a reset". Codex's says "Use one", as it always has.
 *
 * Just after a reset spent here, while Quota Cache polls the account, the
 * hold and the deadline give way to "Checking the new limits…": every figure
 * on the row is from before the reset, and this is the one row of the
 * account's that is never folded away.
 */
function ResetRow({
  credential,
  name,
  figures,
  kind,
  unsorted = false,
  offered,
  busy,
  onAsk,
}: {
  credential: Credential
  name: AccountName
  figures: Figures
  /** The Claude kind this row is of; null for Codex, and for a Claude account whose kinds are not known. */
  kind: string | null
  /** A Claude account whose kinds are not known: the unknown kind's mark, not Codex's arrow. */
  unsorted?: boolean
  /** The server offers it and this browser can spend. */
  offered: boolean
  busy: boolean
  onAsk: () => void
}) {
  const now = useNowSeconds()
  // Quota Cache is polling the account after a reset spent here: the count,
  // the hold and the deadline on this row are all from before it.
  const checking = isChecking(useChecks(), credential, now)
  // `?? ""` and `?? null` for a document from a plugin older than the hold.
  const hold = holdText(figures.hold ?? "", figures.holdUntilEpoch ?? null, now)
  const cooling = (figures.hold ?? "") === "cooldown" && hold !== null
  // Short beside a hold, which already fills most of the line.
  const expiry = expiryOf(figures.expiresAtEpoch, now, hold !== null)
  const address = credential.email || credential.id
  const long = expiryOf(figures.expiresAtEpoch, now, false).text
  const count = figures.availableCount
  // Everything the row says, whole, for the hover and for assistive
  // technology — of this kind, on Claude.
  const banked = kind === null ? plural(count, "reset") : countOf(count, kind)
  const detail = `${banked} banked, ${long}${hold ? `, ${hold}` : ""}${checking ? ", checking the new limits" : ""}`
  const words = kind === null ? null : kindWords(kind)
  const mark = kind ?? (unsorted ? UNKNOWN_KIND : null)

  return (
    <div className={`qg-brow ${hold || !offered ? "is-held" : ""}`} title={`${address}: ${detail}`}>
      <span className="qg-pico">{cooling ? <HoldIcon /> : mark === null ? <ResetIcon /> : <KindIcon kind={mark} />}</span>
      {/* The qualifier never gives way: when two accounts share a local part
        * it is the half that tells their rows apart. */}
      <span className="qg-bwho">
        <span className="qg-plocal">{name.local}</span>
        {name.qualifier && <span className="qg-pqual">{name.qualifier}</span>}
        <span className="qg-pn">· {plural(count, "reset")}</span>
      </span>
      <span className="qg-bst">
        {checking ? (
          <CheckingPill />
        ) : (
          <>
            {hold && (
              <>
                <span className="qg-hold">{hold}</span>
                {" · "}
              </>
            )}
            <span className={expiry.tone}>{expiry.text}</span>
          </>
        )}
      </span>
      {offered && (
        <button
          type="button"
          className="qg-use"
          aria-haspopup="dialog"
          aria-label={
            words ? `${words.button} on ${address}: ${detail}` : `Use one of ${address}'s banked resets: ${detail}`
          }
          disabled={busy}
          onClick={onAsk}
        >
          {/* The verb, said on the button itself: it is what the page's own
            * messages tell the reader to press — after an unknown outcome, to
            * retry the same claim — and on Claude it names the kind, so the
            * retry is never made with another kind's button. */}
          {words ? words.button : "Use one"}
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

/** A row's name, as the section calls the account. */
const rowName = (names: Map<string, AccountName>, credential: Credential): AccountName =>
  names.get(credential.id) ?? { local: shortName(credential, credential.id), qualifier: "" }

/**
 * One kind's group on a Claude card: its mark and heading, what spending one
 * does, and a row per account holding that kind with that kind's own count,
 * hold and deadline. An unrecognised kind has no button on any row, and its
 * line says where it can be spent instead.
 */
function KindGroupView({
  group,
  names,
  state,
}: {
  group: KindGroup
  names: Map<string, AccountName>
  state: BankedResets
}) {
  const heading = useId()
  const words = kindWords(group.kind)
  const spendable = isSpendableKind(group.kind)
  return (
    <div role="group" aria-labelledby={heading} className={`qg-kgroup ${kindClass(group.kind)}`}>
      <h4 id={heading} className="qg-khead">
        <KindMark kind={group.kind} />
        {words.heading}
      </h4>
      <p className="qg-keffect">{words.effect}</p>
      <div className="qg-brows">
        {group.rows.map(({ credential, credits }) => (
          <ResetRow
            key={credential.id}
            credential={credential}
            name={rowName(names, credential)}
            figures={credits}
            kind={group.kind}
            offered={spendable && credits.redeemable && canRedeemHere()}
            busy={state.busy}
            onAsk={() => state.ask(credential, group.kind)}
          />
        ))}
      </div>
    </div>
  )
}

/**
 * Claude accounts holding resets whose kinds Quota Cache did not report — it
 * is older than the kinds. Their counts are true and shown; no button, because
 * a press must name a kind, and guessing "full" is the mistake the kinds are
 * there to prevent. For the same reason the group and its rows wear the
 * unknown kind's question mark, never the circular arrow, which on a Claude
 * card is the full reset's mark.
 */
function UnsortedGroup({
  holding,
  names,
  state,
}: {
  holding: Credential[]
  names: Map<string, AccountName>
  state: BankedResets
}) {
  const heading = useId()
  return (
    <div role="group" aria-labelledby={heading} className="qg-kgroup qg-k-unknown">
      <h4 id={heading} className="qg-khead">
        <KindMark kind={UNKNOWN_KIND} />
        Kind not reported
      </h4>
      <p className="qg-keffect">{KINDS_UNREPORTED}</p>
      <div className="qg-brows">
        {holding.map((credential) => (
          <ResetRow
            key={credential.id}
            credential={credential}
            name={rowName(names, credential)}
            figures={credential.resetCredits!}
            kind={null}
            unsorted
            offered={false}
            busy={state.busy}
            onAsk={() => undefined}
          />
        ))}
      </div>
    </div>
  )
}

/**
 * The card: what spending one does on this provider, a row per account
 * holding any — grouped by kind on Claude — and what each press came to.
 * Present while any account holds one or a press still has something to
 * report — the last reset spent takes its row away, and the outcome must not
 * go with it.
 */
export function BankedResetsCard({ state, names }: { state: BankedResets; names: Map<string, AccountName> }) {
  if (state.holding.length === 0 && state.notices.length === 0 && !state.asking) return null
  // One provider per card: ProviderSection hands over only its own, and names
  // it. Named rather than read off a row, because the card outlives its last
  // row while a notice is up, and the generic "clears the account's windows"
  // above "Fable reset used" would claim what a Fable reset does not do.
  const effect = effectOf(state.provider)
  const layout = layoutResets(state.holding)
  // Said once for the card rather than on every row: it is a fact about this
  // browser's session, and it is the same for every account in it. With
  // figures on screen it means every way in was refused since they arrived.
  const signedOut = !canRedeemHere() && anyRedeemable(state.holding)
  return (
    <article className="qg-win qg-bank" aria-label="Banked resets">
      <div className="qg-whead">
        <h3 className="qg-wtitle">Banked resets</h3>
      </div>
      <p className="qg-bsubt">{effect.subtitle}</p>
      {layout.plain.length > 0 && (
        <div className="qg-brows">
          {layout.plain.map((credential) => (
            <ResetRow
              key={credential.id}
              credential={credential}
              name={rowName(names, credential)}
              figures={credential.resetCredits!}
              kind={null}
              offered={credential.resetCredits!.redeemable && canRedeemHere()}
              busy={state.busy}
              onAsk={() => state.ask(credential, "")}
            />
          ))}
        </div>
      )}
      {layout.groups.map((group) => (
        <KindGroupView key={group.kind} group={group} names={names} state={state} />
      ))}
      {layout.unsorted.length > 0 && <UnsortedGroup holding={layout.unsorted} names={names} state={state} />}
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
 * card's first spend button, else the provider's heading. Without this a
 * keyboard reader who dismisses a notice is dropped back at the top of the
 * page.
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
 * dismissed or its account is pressed again for the same kind: an outcome
 * that may say a reset is spent is not something to time out.
 *
 * A Claude line carries its kind's mark and name after the address, so it is
 * read as news of that kind even where the sentence after it — a refusal that
 * reached nothing kind-specific — does not say.
 */
function ResetNotices({ state }: { state: BankedResets }) {
  if (state.holding.length === 0 && state.notices.length === 0) return null
  return (
    <div role="status" className="qg-notices">
      {state.notices.map((notice) => {
        const words = notice.kind === "" ? null : kindWords(notice.kind)
        return (
          <p key={notice.key} className={`qg-notice ${TONE_CLASS[notice.tone]}`}>
            {(notice.tone === "unknown" || notice.tone === "partial") && <UnknownMark />}
            <span className="min-w-0">
              <b className="qg-notice-who">{notice.who}</b>{" "}
              {words && (
                <>
                  <span className={`qg-ntag ${kindClass(notice.kind)}`}>
                    <KindIcon kind={notice.kind} />
                    {opening(words.one)}
                  </span>{" "}
                </>
              )}
              {notice.text}
            </span>
            <button
              type="button"
              className="qg-notice-x"
              aria-label={
                words ? `Dismiss the message about ${notice.who}’s ${words.one}` : `Dismiss the message about ${notice.who}`
              }
              onClick={(event) => {
                focusAfterDismiss(event.currentTarget)
                state.dismiss(notice.key)
              }}
            >
              <svg viewBox="0 0 10 10" aria-hidden="true">
                <path d="M2 2 8 8M8 2 2 8" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
              </svg>
            </button>
          </p>
        )
      })}
    </div>
  )
}

/** The dialog for the row being pressed, while one is. */
function ResetConfirm({ state }: { state: BankedResets }) {
  const asking = state.asking
  if (!asking?.credential.resetCredits) return null
  const { credential, kind, entry } = asking
  // On Claude, the count of the kind pressed — never the account's total,
  // which would count resets of another kind as this one's.
  const count = kind === "" ? credential.resetCredits!.availableCount : (entry?.availableCount ?? 0)
  return (
    <ConfirmDialog
      credential={credential}
      kind={kind}
      count={count}
      others={kind === "" ? [] : othersHeld(credential.resetCredits, kind)}
      busy={state.busy}
      onConfirm={() => void state.confirm()}
      onCancel={state.cancel}
    />
  )
}
