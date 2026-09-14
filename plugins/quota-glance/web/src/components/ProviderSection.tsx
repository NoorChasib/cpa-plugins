import { WindowCard } from "./WindowCard"
import type { Credential, Provider } from "../lib/types"

const plural = (n: number, one: string) => `${n} ${n === 1 ? one : `${one}s`}`

export function ProviderSection({ provider, credentials }: { provider: Provider; credentials: Map<string, Credential> }) {
  // Sorted by the server's key, not by anything this app decides. `order` is
  // the sort key and the contract is explicit that the client holds no opinion
  // about what the values mean.
  const rows = [...provider.rows].sort((a, b) => a.order - b.order)

  return (
    <section className="mb-[30px]">
      <div className="mb-[11px] flex items-baseline gap-[9px] pl-[2px]">
        <h2 className="text-[14px] font-semibold tracking-[-0.01em]">{provider.title}</h2>
        <span className="text-[11.5px] text-ink-3">{plural(provider.credentialCount, "credential")}</span>
      </div>

      {rows.length === 0 ? (
        // A provider CPA knows about but quota-cache does not poll, or has not
        // polled yet. A bare heading with nothing under it reads as a bug.
        <p className="rounded-[12px] border border-line bg-card px-4 py-[14px] text-[12.5px] text-ink-3">
          No quota windows reported for this provider yet.
        </p>
      ) : (
        rows.map((row) => <WindowCard key={row.rowId} row={row} credentials={credentials} />)
      )}
    </section>
  )
}
