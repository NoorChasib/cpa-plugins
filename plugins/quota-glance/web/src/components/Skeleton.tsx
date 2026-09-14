/**
 * First paint, before the first response.
 *
 * Cards in the shape of the real ones rather than a spinner on an empty page:
 * the layout does not jump when the data lands, and on a slow phone over
 * Tailscale the reader can already see that this is the right page.
 */
function SkeletonCard({ rows }: { rows: number }) {
  return (
    <div className="mb-[10px] rounded-[12px] border border-line bg-card px-4 pb-[6px] pt-[15px]">
      <div className="mb-[4px] grid grid-cols-[1fr_auto] items-baseline gap-x-3 border-b border-line pb-3">
        <span className="h-[13px] w-[84px] rounded bg-card-2" />
        <span className="h-[22px] w-[64px] justify-self-end rounded bg-card-2" />
        <span className="col-span-full mt-[9px] h-[9px] w-[58%] rounded bg-card-2" />
      </div>
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="cred">
          <span className="cred-mail h-[11px] w-[70%] rounded bg-card-2" />
          <span className="cred-plan h-[15px] w-[42px] rounded-[5px] bg-card-2" />
          <span className="cred-bar h-[6px] w-full rounded-[3px] bg-track" />
          <span className="cred-pct h-[10px] w-full rounded bg-card-2" />
          <span className="cred-eta h-[10px] w-full rounded bg-card-2" />
        </div>
      ))}
    </div>
  )
}

export function Skeleton() {
  return (
    <div aria-busy="true" aria-label="Loading capacity" className="animate-pulse">
      <div className="mb-[11px] flex items-baseline gap-[9px] pl-[2px]">
        <span className="h-[13px] w-[58px] rounded bg-card-2" />
      </div>
      <SkeletonCard rows={5} />
      <SkeletonCard rows={5} />
    </div>
  )
}
