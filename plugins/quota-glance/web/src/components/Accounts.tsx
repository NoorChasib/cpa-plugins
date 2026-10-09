import { useEffect, useId, useState } from "react"

import { Box, Dot, EditButton, EditFoot, EditLink, type FootStatus, RestFoot, SetHereDot } from "./Edit"
import { Flags, FoldButton } from "./Fold"
import {
  accountFlags,
  renewalFoot,
  renewalMark,
  renewalOrphanText,
  ROUTING_LABELS,
  yearlyEstimate,
} from "../lib/accounts"
import { editingOffText } from "../lib/apicredits"
import { useNowSeconds } from "../lib/now"
import { type AccountName, nameText, shortName } from "../lib/pool"
import { canSaveHere, saveSettings } from "../lib/save"
import {
  CONFLICT_FIELD,
  dateEpoch,
  EMPTY_RENEWAL_DRAFT,
  editRenewal,
  FIELD_FOOT,
  type FieldEdit,
  type FieldState,
  newerRevision,
  reconcileRenewals,
  removeRenewalOrphan,
  type RenewalDraft,
  renewalField,
  renewalPlan,
  SIGN_IN_FOOT,
  utcDay,
} from "../lib/settings"
import { calendarDaysUntil, formatAgo, formatDate, formatDuration } from "../lib/time"
import type { Activity, APICreditEditing, Credential, Credits, RenewalOrphan, RenewalSetting } from "../lib/types"

/**
 * What each of a provider's accounts is, said once for the provider: its plan,
 * when its subscription renews, what it holds in credit, and what CPA has
 * routed to it. None of it belongs to a window, so none of it repeats on the
 * window cards above.
 */

/**
 * The address, with the domain in muted ink so the distinguishing word reads
 * first. Never initials: a column of initials is unreadable when four of them
 * begin with the same letter. `short` drops the domain for a line that has no
 * room for it; the whole address stays in the hover.
 */
function Email({ address, short = false }: { address: string; short?: boolean }) {
  const at = address.indexOf("@")
  const [local, domain] = at > 0 ? [address.slice(0, at), address.slice(at)] : [address, ""]
  return (
    <span className="qg-aname" title={address}>
      {local}
      {domain && <span className={`text-ink-3 ${short ? "qg-long" : ""}`}>{domain}</span>}
    </span>
  )
}

/**
 * The traffic strip: one block per bucket, oldest at the left, the bucket in
 * progress at the right.
 *
 * A block is inked from the server's intensity, never from its own count, so
 * the credential carrying the pool towers over the one taking a trickle instead
 * of every row looking equally busy. Any failure in a bucket colours it red
 * whatever its volume — a single failure inside a busy ten minutes is the thing
 * most worth not losing.
 *
 * Empty buckets are drawn, not skipped. The gaps are half of what the strip
 * says, and a row with its quiet buckets removed would read as continuous
 * traffic.
 */
function TrafficStrip({ activity, span }: { activity: Activity; span: string }) {
  return (
    <span className="act" role="img" aria-label={`request activity over the last ${span}`}>
      {activity.buckets.map((bucket, index) => (
        <i key={index} className={bucket.failed > 0 ? "act-f" : `act-${Math.min(bucket.intensity, 3)}`} />
      ))}
    </span>
  )
}

/**
 * The strip's words: how many, how many failed, and when the last landed.
 *
 * Successes and failures are named separately rather than summed: "3 req" over
 * a strip that is two thirds red describes the same numbers and none of the
 * situation. "now" comes from the server's own `live` flag rather than from
 * comparing `lastRequestAtEpoch` to the clock: only the server knows which
 * bucket is the one in progress. The success count is the part that gives way
 * on a narrow line (`qg-long`) — beside failures, or on a line that must also
 * hold the renewal (`brief`); the strip already shows roughly how much.
 */
function TrafficText({ activity, span, brief = false }: { activity: Activity; span: string; brief?: boolean }) {
  const now = useNowSeconds()
  if (activity.success === 0 && activity.failed === 0) {
    return (
      <span className="qg-atxt num text-ink-4" title={`no requests in the last ${span}`}>
        idle
      </span>
    )
  }
  const when = activity.live
    ? "now"
    : activity.lastRequestAtEpoch !== null
      ? formatAgo(activity.lastRequestAtEpoch, now)
      : span
  return (
    <span className={`qg-atxt num ${activity.live && activity.failed === 0 ? "text-accent" : "text-ink-3"}`}>
      {activity.success > 0 && (
        <span className={activity.failed > 0 || brief ? "qg-long" : ""}>
          {activity.success} req
          {" · "}
        </span>
      )}
      {activity.failed > 0 && (
        <>
          <span className="text-crit">{activity.failed} failed</span>
          {" · "}
        </>
      )}
      {when}
    </span>
  )
}

/**
 * A credit balance as the account line prints it. The figure is the server's,
 * already formatted; this only names what it is a figure of. "Unlimited" is a
 * word, not a figure, so it is not set in the figures' monospace.
 */
function creditsText(credits: Credits): { figure: string; unit: string; numeric: boolean } {
  switch (credits.unit) {
    case "credits":
      if (credits.unlimited) return { figure: "unlimited", unit: "credits", numeric: false }
      return { figure: credits.display, unit: credits.display === "1" ? "credit" : "credits", numeric: true }
    case "usd":
      return credits.unlimited
        ? { figure: "unlimited", unit: "credit", numeric: false }
        : { figure: credits.display, unit: "prepaid", numeric: true }
    default:
      // A unit this bundle does not know yet. The server's figure and its unit
      // are still the truth, so they are printed as sent.
      return { figure: credits.display, unit: credits.unit, numeric: !credits.unlimited }
  }
}

function CreditsFigure({ credits }: { credits: Credits }) {
  const text = creditsText(credits)
  return (
    <span>
      <b className={text.numeric ? "num" : ""}>{text.figure}</b> {text.unit}
    </span>
  )
}

const DAY_SECONDS = 86400

/** Calendar days between two instants' UTC dates, for a date set here and printed as UTC sees it. */
const utcDaysUntil = (epoch: number, now: number) => Math.floor(epoch / DAY_SECONDS) - Math.floor(now / DAY_SECONDS)

/**
 * When the subscription renews: the date, so it can be checked against a
 * receipt, and the distance, ticking like every other countdown here.
 *
 * An estimate carries "~" on its date, and a date set on the dashboard a dot
 * after it; the card's foot explains both once, and a hover in full. Either
 * one's distance is calendar days, so it agrees with the date beside it — a
 * date set here on the UTC calendar it was set on. One that comes round while
 * the page is open is due rather than passed, since the next document will
 * already carry the anniversary after it.
 */
function Renewal({ credential, now }: { credential: Credential; now: number }) {
  const mark = renewalMark(credential)
  if (mark === null) return null
  const remaining = mark.atEpoch - now
  const date = `${mark.estimated ? "~" : ""}${mark.utc ? utcDay(mark.atEpoch) : formatDate(mark.atEpoch)}`
  if (remaining <= 0) return <span>{mark.estimated || mark.setHere ? "renewal due" : "renewal date passed"}</span>
  const distance =
    mark.setHere && remaining >= DAY_SECONDS
      ? `${utcDaysUntil(mark.atEpoch, now)}d`
      : mark.estimated && remaining >= DAY_SECONDS
        ? `${calendarDaysUntil(mark.atEpoch, now)}d`
        : formatDuration(remaining)
  return (
    <span title={mark.estimated ? ESTIMATED_RENEWAL_TITLE : undefined}>
      renews <b>{date}</b>
      {mark.setHere && <SetHereDot title="Set here. Replaces the estimate." />}
      <span className="qg-long"> · in {distance}</span>
    </span>
  )
}

/** What an estimated renewal is, for the reader who wonders. */
const ESTIMATED_RENEWAL_TITLE =
  "Estimated from when the subscription started. Anthropic does not report the renewal date, so this can be off if the billing date has moved."

/**
 * Who the account is, its plan and whether CPA has parked it. Full plan
 * names, never truncated: "SuperGrok Heavy" and "Enterprise" are what the
 * account is sold as, and the address beside them is the part that gives way.
 * A plan the provider did not report is left out rather than shown as an
 * empty pill.
 */
function Who({ credential, short }: { credential: Credential; short: boolean }) {
  const parked = ROUTING_LABELS[credential.status]
  return (
    <span className="qg-awho">
      <Email address={credential.email || credential.id} short={short} />
      {credential.plan && <span className="qg-chip">{credential.plan}</span>}
      {parked && <span className="qg-chip qg-chip-route">{parked}</span>}
    </span>
  )
}

/** One account in a provider's Accounts card. */
function AccountRow({ credential, now }: { credential: Credential; now: number }) {
  const activity = credential.activity
  const span = activity ? formatDuration(activity.windowSeconds) : ""
  return (
    <div className="qg-arow qg-arow-acct">
      <Who credential={credential} short />
      <span className="qg-asub">
        {typeof credential.renewalAtEpoch === "number" ? (
          <Renewal credential={credential} now={now} />
        ) : (
          <span className="text-ink-4">no renewal date</span>
        )}
        {credential.credits && (
          <>
            {" · "}
            <CreditsFigure credits={credential.credits} />
          </>
        )}
      </span>
      {/* Absent entirely when the host reports no counter: null is not idle. */}
      {activity && (
        <>
          <TrafficStrip activity={activity} span={span} />
          <TrafficText activity={activity} span={span} />
        </>
      )}
    </div>
  )
}

/** Under a renewal date field: where the date comes from, or what is about to happen to it (F.3). */
function RenewalCap({
  credential,
  state,
  now,
  mark,
  busy,
  onEdit,
}: {
  credential: Credential
  state: FieldState
  now: number
  mark: string | null
  busy: boolean
  onEdit: (edit: FieldEdit | undefined) => void
}) {
  const shown = renewalMark(credential)
  const estimate = shown?.estimated ? `~${formatDate(shown.atEpoch)}` : null
  const undo = (
    <EditLink disabled={busy} onClick={() => onEdit(undefined)}>
      Undo
    </EditLink>
  )
  if (state.error !== null) return <span className="qg-ecap is-bad">{state.error}<Dot />{undo}</span>
  return (
    <>
      {mark && <span className="qg-ecap is-warn">{mark}</span>}
      {state.edit?.kind === "drop" ? (
        <span className="qg-ecap">
          <span className="qg-esrc is-dirty">back to the estimate</span>
          <Dot />
          {undo}
        </span>
      ) : state.changed ? (
        <span className="qg-ecap">
          <span className="qg-esrc is-dirty">changed</span>
          <Dot />
          <span>
            {state.stored ? (
              <>
                was <b>{utcDay(dateEpoch(state.stored))}</b>
              </>
            ) : estimate ? (
              <>
                was <b>{estimate}</b>, estimated
              </>
            ) : (
              "was not known"
            )}
          </span>
          <Dot />
          {undo}
        </span>
      ) : state.source === "dashboard" ? (
        <span className="qg-ecap">
          <span className="qg-esrc">
            <SetHereDot lead />
            set here
          </span>
          <Dot />
          <EditLink disabled={busy} onClick={() => onEdit({ kind: "drop" })}>
            Use estimate
          </EditLink>
        </span>
      ) : estimate ? (
        <span className="qg-ecap">
          <span className="qg-esrc">
            estimate <b>{estimate}</b>
          </span>
          {yearlyEstimate(credential, now) && (
            <>
              <Dot />
              <span>yearly plan</span>
            </>
          )}
        </span>
      ) : (
        <span className="qg-ecap">
          <span className="qg-esrc">not known yet</span>
        </span>
      )}
    </>
  )
}

/** One account in the renewal editor: a date field when it can take one, its date as text when not. */
function RenewalEditRow({
  credential,
  name,
  setting,
  edit,
  now,
  mark,
  busy,
  onEdit,
}: {
  credential: Credential
  name: string
  setting: RenewalSetting | null | undefined
  edit: FieldEdit | undefined
  now: number
  mark: string | null
  busy: boolean
  onEdit: (edit: FieldEdit | undefined) => void
}) {
  const who = <Who credential={credential} short />
  if (credential.renewalEditable !== true) {
    // Codex reports its own date, and a credential this page cannot key a
    // date by shows the one it has.
    return (
      <div className="qg-arow qg-earow is-locked">
        {who}
        <span className="qg-asub">
          {typeof credential.renewalAtEpoch === "number" ? (
            <Renewal credential={credential} now={now} />
          ) : (
            <span className="text-ink-4">no renewal date</span>
          )}
        </span>
      </div>
    )
  }
  const state = renewalField(setting, edit)
  return (
    <div className="qg-arow qg-earow">
      {who}
      <div className="qg-efield is-date">
        <Box state={{ changed: state.changed, dropped: state.edit?.kind === "drop", bad: state.error !== null, busy }}>
          <input
            type="date"
            value={state.text}
            min="2000-01-01"
            max="2099-12-31"
            disabled={busy}
            aria-invalid={state.error !== null}
            aria-label={`${name}: subscription renews on, UTC`}
            onChange={(event) =>
              onEdit({ kind: "set", text: event.target.value, incomplete: event.target.validity.badInput || undefined })
            }
          />
        </Box>
        <RenewalCap credential={credential} state={state} now={now} mark={mark} busy={busy} onEdit={onEdit} />
      </div>
    </div>
  )
}

/**
 * A provider's accounts, once: a card shut by default whose fold line names
 * any account whose requests are failing or that CPA has parked. Open, a
 * line per account.
 *
 * On Claude, Set renewal dates turns the renewal column into one date field
 * per subscription, in place, as the API credit card does (F.3). The draft
 * lives here, outside the fold's body, so folding keeps it; one Save sends
 * every changed date as one batch.
 *
 * The fold sits in the card's heading, so the card is found by heading like
 * every other; the button inside it is what opens it.
 */
export function AccountsCard({
  held,
  names,
  open,
  onToggle,
  renewalOrphans = [],
  editingOffer = null,
  onSaved = () => undefined,
}: {
  held: Credential[]
  names: Map<string, AccountName>
  open: boolean
  onToggle: () => void
  /** Renewal dates stored for credentials CPA no longer lists; shown in the editor's last line. */
  renewalOrphans?: RenewalOrphan[]
  /** Card-level editing availability, when API credits expose it. */
  editingOffer?: APICreditEditing | null
  /** Re-reads the document after a save. */
  onSaved?: () => void
}) {
  const now = useNowSeconds()
  const bodyID = useId()
  const lineID = useId()
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState<RenewalDraft>(EMPTY_RENEWAL_DRAFT)
  const [overlay, setOverlay] = useState<Record<string, RenewalSetting | null>>({})
  const [marks, setMarks] = useState<Record<string, string>>({})
  const [status, setStatus] = useState<FootStatus | null>(null)
  const [saved, setSaved] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [failed, setFailed] = useState(false)

  // A refreshed roster: a credential CPA no longer lists takes its edit with
  // it, said so; a conflict's dates give way once the document has them.
  // Keyed on what the roster says rather than on the arrays, which the
  // section builds afresh on every tick of the clock.
  const roster = [
    ...held.map((credential) => `${credential.id}@${credential.renewalSetting?.revision ?? ""}`),
    ...renewalOrphans.map((orphan) => `-${orphan.id}`),
  ].join(" ")
  useEffect(() => {
    setDraft((kept) => {
      const { draft: next, dropped } = reconcileRenewals(kept, held, renewalOrphans)
      if (dropped.length > 0) setStatus({ tone: "warn", text: dropped.join(" ") })
      return next
    })
    setOverlay((kept) => {
      const next = { ...kept }
      let changed = false
      for (const credential of held) {
        const latest = next[credential.id]
        if (latest !== undefined && !newerRevision(latest?.revision ?? "", credential.renewalSetting?.revision ?? "")) {
          delete next[credential.id]
          changed = true
        }
      }
      return changed ? next : kept
    })
  }, [roster])

  useEffect(() => {
    if (saved === null) return
    const timer = window.setTimeout(() => setSaved(null), 20_000)
    return () => window.clearTimeout(timer)
  }, [saved])

  const ring = held.find((credential) => credential.activity)?.activity
  const nameOf = (credential: Credential) =>
    nameText(names.get(credential.id) ?? { local: shortName(credential, credential.id), qualifier: "" })
  const flags = accountFlags(held, held.map(nameOf), (epoch) => formatAgo(epoch, now))
  // The strip's column, as wide as the longest ring among the accounts, so a
  // row without a counter keeps the same columns as one with: each row is a
  // grid of its own, and an `auto` track would be as wide as its own strip.
  const buckets = Math.max(0, ...held.map((credential) => credential.activity?.buckets.length ?? 0))
  const foot = renewalFoot(held)
  const settingOf = (credential: Credential) =>
    credential.id in overlay ? overlay[credential.id] : credential.renewalSetting
  const plan = renewalPlan(held, draft, settingOf)
  const doorOpen = canSaveHere()
  const editingOff = editingOffer ? editingOffText(editingOffer) : null
  const offered = editingOff === null && (foot.editable || renewalOrphans.length > 0)
  const canEdit = offered && doorOpen

  const startEditing = () => {
    setSaved(null)
    setStatus(null)
    setFailed(false)
    setEditing(true)
  }
  const cancel = () => {
    setDraft(EMPTY_RENEWAL_DRAFT)
    setMarks({})
    setStatus(null)
    setFailed(false)
    setEditing(false)
  }
  const edit = (credential: Credential, next: FieldEdit | undefined) => {
    setDraft((kept) => editRenewal(kept, { id: credential.id, renewalSetting: settingOf(credential) }, nameOf(credential), next))
    setMarks((kept) => {
      if (!(credential.id in kept)) return kept
      const rest = { ...kept }
      delete rest[credential.id]
      return rest
    })
  }

  const save = async () => {
    if (plan.batch === null || saving) return
    setSaving(true)
    setStatus(null)
    setMarks({})
    const outcome = await saveSettings(plan.batch)
    setSaving(false)
    switch (outcome.kind) {
      case "saved":
        setDraft(EMPTY_RENEWAL_DRAFT)
        setFailed(false)
        setEditing(false)
        setSaved(
          outcome.unchanged
            ? "Already saved. Nothing needed changing."
            : `Saved ${plan.changes} renewal ${plan.changes === 1 ? "date" : "dates"}.`,
        )
        onSaved()
        return
      case "conflict": {
        const nextOverlay = { ...overlay }
        const nextMarks: Record<string, string> = {}
        const rows = { ...draft.rows }
        const remove = { ...draft.remove }
        for (const id of outcome.ids) {
          const current = outcome.current[id]
          nextOverlay[id] = current && typeof current === "object" ? (current as RenewalSetting) : null
          nextMarks[id] = CONFLICT_FIELD
          delete rows[id]
          delete remove[id]
        }
        setDraft({ rows, remove })
        setOverlay(nextOverlay)
        setMarks(nextMarks)
        setStatus({ tone: "bad", text: outcome.text })
        setFailed(true)
        onSaved()
        return
      }
      case "field":
        setMarks({ [outcome.id]: outcome.text })
        setStatus({ tone: "bad", text: FIELD_FOOT })
        setFailed(true)
        return
      case "failed":
        setStatus({ tone: "bad", text: outcome.text })
        setFailed(true)
        return
      case "lost":
        setStatus({ tone: "unknown", text: outcome.text })
        setFailed(true)
        onSaved()
        return
    }
  }

  const blocked = editingOff ?? (!offered
    ? "Renewal dates cannot be set here right now. Your changes are kept until you reload."
    : !doorOpen
      ? SIGN_IN_FOOT
      : plan.tooLarge ? "Too many changes for one save; undo some, save, then make the rest." : null)

  return (
    <article
      className="qg-win qg-roster"
      aria-label="Accounts"
      style={{ "--qg-act-n": buckets } as React.CSSProperties}
    >
      <h3 className="qg-fold-h">
        <FoldButton open={open} controls={bodyID} labelledBy={lineID} onToggle={onToggle} className="qg-fold-head">
          <span id={lineID} className="qg-fold-line">
            <span className="qg-rtitle">Accounts</span>
            <span className="qg-rnote qg-long">
              {ring ? `plan, renewal and requests · last ${formatDuration(ring.windowSeconds)}` : "plan and renewal"}
            </span>
            {editing && (
              <span className="qg-fold-note">
                {" · "}
                <span className={`qg-estate ${failed ? "is-bad" : ""}`}>{failed ? "not saved" : "editing renewals"}</span>
                {plan.changes > 0 && ` · ${plan.changes} unsaved`}
              </span>
            )}
            <Flags flags={flags} />
          </span>
        </FoldButton>
      </h3>
      <div id={bodyID} hidden={!open} className="qg-rows">
        {open &&
          (editing ? (
            <>
              <form
                className="qg-eform"
                aria-label="Subscription renewal dates"
                onSubmit={(event) => {
                  event.preventDefault()
                  void save()
                }}
              >
                <div className="qg-eahead" aria-hidden="true">
                  <span>Account</span>
                  <span>Renews on, UTC</span>
                </div>
                {held.map((credential) => (
                  <RenewalEditRow
                    key={credential.id}
                    credential={credential}
                    name={credential.email || credential.id}
                    setting={settingOf(credential)}
                    edit={draft.rows[credential.id]?.date}
                    now={now}
                    mark={marks[credential.id] ?? null}
                    busy={saving}
                    onEdit={(next) => edit(credential, next)}
                  />
                ))}
                {renewalOrphans.map((orphan) => (
                  <p key={orphan.id} className="qg-eorphan">
                    {renewalOrphanText(orphan)}
                    <Dot />
                    {orphan.id in draft.remove ? (
                      <>
                        <span className="qg-esrc is-dirty">removing</span>
                        <Dot />
                        <EditLink disabled={saving} onClick={() => setDraft((kept) => removeRenewalOrphan(kept, orphan, false))}>
                          Undo
                        </EditLink>
                      </>
                    ) : (
                      <EditLink disabled={saving} onClick={() => setDraft((kept) => removeRenewalOrphan(kept, orphan, true))}>
                        Remove
                      </EditLink>
                    )}
                  </p>
                ))}
              </form>
              <EditFoot
                help="The date on claude.ai under Settings, Billing. It repeats monthly, or yearly on an annual plan, and replaces the estimate."
                changes={plan.changes}
                errors={plan.errors}
                saving={saving}
                blocked={blocked}
                status={status}
                failed={failed}
                onCancel={cancel}
                onSave={() => void save()}
              />
            </>
          ) : (
            <>
              {/* The catalog's order, which every window card above repeats. */}
              {held.map((credential) => (
                <AccountRow key={credential.id} credential={credential} now={now} />
              ))}
              {(foot.estimated || foot.setHere || canEdit || saved || editingOff) && (
                <RestFoot
                  saved={saved}
                  action={canEdit ? <EditButton onClick={startEditing}>Set renewal dates</EditButton> : null}
                >
                  {foot.estimated && (
                    <>
                      <b>~</b> estimated from when the subscription started
                    </>
                  )}
                  {foot.estimated && (foot.setHere || canEdit) && " · "}
                  {(foot.setHere || canEdit) && (
                    <>
                      <SetHereDot lead />
                      <b>set here</b>
                    </>
                  )}
                  {editingOff && <><br />{editingOff}</>}
                </RestFoot>
              )}
            </>
          ))}
      </div>
    </article>
  )
}

/**
 * A provider's only account, as one slim line: no fold, no visible title,
 * because there is nothing to choose between. Who, its plan, its requests,
 * and its renewal and credit at the far end — in the popover, the requests
 * and the renewal share the second line, the strip saying how many.
 *
 * The heading is for assistive technology alone, so the card is found by
 * heading like every other.
 */
export function SoloAccount({ credential, title }: { credential: Credential; title: string }) {
  const now = useNowSeconds()
  const activity = credential.activity
  const span = activity ? formatDuration(activity.windowSeconds) : ""
  const renews = typeof credential.renewalAtEpoch === "number"
  return (
    <div className="qg-win qg-solo" role="group" aria-label={`${title} account`}>
      <h3 className="sr-only">{title} account</h3>
      <Who credential={credential} short={false} />
      {activity && (
        <span className="qg-traffic">
          <TrafficStrip activity={activity} span={span} />
          <TrafficText activity={activity} span={span} brief />
        </span>
      )}
      {(renews || credential.credits) && (
        <span className="qg-ameta">
          {renews && <Renewal credential={credential} now={now} />}
          {renews && credential.credits && " · "}
          {credential.credits && <CreditsFigure credits={credential.credits} />}
        </span>
      )}
    </div>
  )
}
