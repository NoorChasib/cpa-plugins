import { useId, useState } from "react"

import { ResetConfirm, ResetNotices, ResetTiles, useBankedResets } from "./BankedResets"
import { WindowCard } from "./WindowCard"
import { cardKey, collapsedKeys, setCollapsed } from "../lib/collapsed"
import { accountNames } from "../lib/pool"
import type { Credential, Provider } from "../lib/types"

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

/**
 * A provider's band: its name and what it counts on the left, and on the right
 * whatever belongs to the provider rather than to one of its windows.
 *
 * Shared with the prepaid balances below the providers, so an OpenRouter
 * section reads as the same kind of thing as a Codex one.
 */
export function ProviderBand({
  id,
  title,
  detail,
  children,
}: {
  id: string
  title: string
  detail: string
  children?: React.ReactNode
}) {
  return (
    <div className={`qg-pbar ${children ? "" : "is-bare"}`}>
      <div className="qg-pname">
        <h2 id={id}>{title}</h2>
        <span className="qg-pcount">{detail}</span>
      </div>
      {children}
    </div>
  )
}

export function ProviderSection({
  provider,
  credentials,
  onRedeemed,
}: {
  provider: Provider
  credentials: Map<string, Credential>
  onRedeemed: () => void
}) {
  // Sorted by the server's key, not by anything this app decides. `order` is
  // the sort key and the contract is explicit that the client holds no opinion
  // about what the values mean.
  const rows = [...provider.rows].sort((a, b) => a.order - b.order)
  // The provider's own credentials, in the catalog's order, which is the order
  // every card already uses.
  const held = [...credentials.values()].filter((credential) => credential.provider === provider.id)
  // One name per account for the whole section, so the tile, the legend and
  // the recovery line all call an account the same thing.
  const names = accountNames(held)
  const resets = useBankedResets(held, onRedeemed)
  const headingID = useId()

  // Read once at mount rather than per render: this is a preference the reader
  // sets here, so storage is the record and this is the working copy.
  const [folded, setFolded] = useState(collapsedKeys)

  return (
    <section className="qg-prov" aria-labelledby={headingID}>
      {/* Banked resets live in the band, one tile per account holding any,
        * because a reset belongs to the account rather than to any one window:
        * spending it clears them together. What a press came to is printed
        * straight under the band, beside the tiles that made it. */}
      <ProviderBand
        id={headingID}
        title={provider.title}
        detail={plural(provider.credentialCount, "credential")}
      >
        {resets.holding.length > 0 ? <ResetTiles state={resets} names={names} /> : null}
      </ProviderBand>
      <ResetNotices state={resets} />
      <ResetConfirm state={resets} />

      {rows.length === 0 ? (
        // A provider CPA knows about but quota-cache does not poll, or has not
        // polled yet. A bare heading with nothing under it reads as a bug.
        <p className="qg-empty">No quota windows reported for this provider yet.</p>
      ) : (
        rows.map((row) => {
          const key = cardKey(provider.id, row.rowId)
          return (
            <WindowCard
              key={row.rowId}
              row={row}
              credentials={credentials}
              names={names}
              collapsed={folded.has(key)}
              onToggle={() => setFolded(setCollapsed(key, !folded.has(key)))}
            />
          )
        })
      )}
    </section>
  )
}
