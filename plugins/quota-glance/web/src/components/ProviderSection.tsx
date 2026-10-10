import { useId, useState } from "react"

import { AccountsCard, SoloAccount } from "./Accounts"
import { APICreditsCard } from "./APICredits"
import { BankedResetsCard, useBankedResets } from "./BankedResets"
import { WindowCard } from "./WindowCard"
import { CARD, cardKey, openedKeys, setOpened } from "../lib/collapsed"
import { accountNames } from "../lib/pool"
import { accountsCardNeeded } from "../lib/accounts"
import type { APICredits, Credential, Provider, RenewalOrphan } from "../lib/types"

/**
 * A section's heading: the provider's name and a hairline to the edge,
 * nothing else. What it counts and what it holds are the cards' business, and
 * a heading that repeated them was the busiest line on the page.
 *
 * Shared with the prepaid balances below the providers, so an OpenRouter
 * section reads as the same kind of thing as a Codex one.
 */
export function SectionHead({ id, title }: { id: string; title: string }) {
  return (
    <div className="qg-phead">
      <h2 id={id}>{title}</h2>
    </div>
  )
}

/**
 * One provider: its windows, then — on Claude — the monthly API credit, then
 * what each account is, then its banked resets, always last.
 *
 * The order runs from what a reader came to check to what they might act on.
 * Banked resets close the section because spending one moves every figure
 * above it, so those are read before the button that changes them.
 */
export function ProviderSection({
  provider,
  credentials,
  apiCredits = null,
  renewalOrphans = [],
  onRedeemed,
}: {
  provider: Provider
  credentials: Map<string, Credential>
  /** The monthly API credit, which belongs in the Claude section; null anywhere else. */
  apiCredits?: APICredits | null
  /** Renewal dates stored for credentials no longer listed, which Claude's Accounts editor lists; empty anywhere else. */
  renewalOrphans?: RenewalOrphan[]
  /** Re-reads the document: after a reset is spent, or settings are saved. */
  onRedeemed: () => void
}) {
  // Sorted by the server's key, not by anything this app decides. `order` is
  // the sort key and the contract is explicit that the client holds no opinion
  // about what the values mean.
  const rows = [...provider.rows].sort((a, b) => a.order - b.order)
  // The provider's own credentials, in the catalog's order, which is the order
  // every card already uses.
  const held = [...credentials.values()].filter((credential) => credential.provider === provider.id)
  // One name per account for the whole section, so every card and the
  // recovery lines all call an account the same thing.
  const names = accountNames(held)
  const resets = useBankedResets(held, onRedeemed)
  const headingID = useId()

  // Read once at mount rather than per render: this is a preference the reader
  // sets here, so storage is the record and this is the working copy. Every
  // card starts shut; these are the ones the reader opened.
  const [opened, setOpenedKeys] = useState(openedKeys)
  const fold = (name: string) => {
    const key = cardKey(provider.id, name)
    return { open: opened.has(key), onToggle: () => setOpenedKeys(setOpened(key, !opened.has(key))) }
  }

  return (
    <section className="qg-prov" aria-labelledby={headingID}>
      <SectionHead id={headingID} title={provider.title} />

      {rows.length === 0 && !apiCredits ? (
        // A provider CPA knows about but quota-cache does not poll, or has not
        // polled yet. A bare heading with nothing under it reads as a bug.
        <p className="qg-empty qg-empty-card">No quota windows reported for this provider yet.</p>
      ) : (
        rows.map((row) => (
          <WindowCard
            key={row.rowId}
            row={row}
            credentials={credentials}
            names={names}
            single={held.length === 1}
            {...fold(row.rowId)}
          />
        ))
      )}

      {apiCredits && <APICreditsCard credits={apiCredits} onSaved={onRedeemed} {...fold(CARD.apiCredits)} />}

      {/* Claude keeps its renewal editor even with one account. Other
        * providers' solo accounts need only the slim read-only line. */}
      {accountsCardNeeded(provider.id, held.length, renewalOrphans.length) ? (
        <AccountsCard
          held={held}
          names={names}
          renewalOrphans={renewalOrphans}
          editingOffer={apiCredits?.editing ?? null}
          onSaved={onRedeemed}
          {...fold(CARD.accounts)}
        />
      ) : held.length === 1 ? (
        <SoloAccount credential={held[0]!} title={provider.title} />
      ) : null}

      <BankedResetsCard state={resets} names={names} />
    </section>
  )
}
