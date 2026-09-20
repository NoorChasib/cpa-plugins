import { useState } from "react"

import { BankedResets } from "./BankedResets"
import { WindowCard } from "./WindowCard"
import { cardKey, collapsedKeys, setCollapsed } from "../lib/collapsed"
import type { Credential, Provider } from "../lib/types"

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

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

  // Read once at mount rather than per render: this is a preference the reader
  // sets here, so storage is the record and this is the working copy.
  const [folded, setFolded] = useState(collapsedKeys)

  return (
    <section className="mb-[30px]">
      <div className="mb-[11px] flex items-baseline gap-[9px] pl-[2px]">
        <h2 className="text-[14px] font-semibold tracking-[-0.01em]">{provider.title}</h2>
        <span className="text-[11.5px] text-ink-3">{plural(provider.credentialCount, "credential")}</span>
      </div>

      {/* Above the cards, and absent entirely when this provider has none.
        * A banked reset belongs to the account rather than to any one window:
        * spending it clears them together, so it is shown once here instead of
        * once per card. */}
      <BankedResets credentials={held} onRedeemed={onRedeemed} />

      {rows.length === 0 ? (
        // A provider CPA knows about but quota-cache does not poll, or has not
        // polled yet. A bare heading with nothing under it reads as a bug.
        <p className="rounded-[12px] border border-line bg-card px-4 py-[14px] text-[12.5px] text-ink-3">
          No quota windows reported for this provider yet.
        </p>
      ) : (
        rows.map((row) => {
          const key = cardKey(provider.id, row.rowId)
          return (
            <WindowCard
              key={row.rowId}
              row={row}
              credentials={credentials}
              collapsed={folded.has(key)}
              onToggle={() => setFolded(setCollapsed(key, !folded.has(key)))}
            />
          )
        })
      )}
    </section>
  )
}
