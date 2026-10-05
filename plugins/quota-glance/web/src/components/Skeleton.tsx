/**
 * First paint, before the first response.
 *
 * Cards in the shape of the real ones rather than a spinner on an empty page:
 * the layout does not jump when the data lands, and on a slow phone over
 * Tailscale the reader can already see that this is the right page.
 */
function SkeletonCard({ rows }: { rows: number }) {
  return (
    <div className="qg-win">
      <span className="block h-[13px] w-[84px] rounded bg-card-2" />
      <span className="qg-bar mt-[13px]" />
      <div className="qg-hfig">
        <span className="h-[28px] w-[86px] rounded bg-card-2" />
        <span className="h-[12px] w-[42%] rounded bg-card-2" />
      </div>
      <span className="mt-[10px] block h-[9px] w-[58%] rounded bg-card-2" />
      <div className="qg-fold" aria-hidden="true">
        <span className="h-[10px] w-[70px] rounded bg-card-2" />
      </div>
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="qg-acct">
          <div className="qg-aline">
            <span className="h-[11px] w-[52%] rounded bg-card-2" />
            <span className="h-[18px] w-[54px] rounded-full bg-card-2" />
          </div>
          <span className="qg-abar" />
          <div className="qg-afig">
            <span className="h-[11px] w-[54px] rounded bg-card-2" />
            <span className="ml-auto h-[11px] w-[74px] rounded bg-card-2" />
          </div>
        </div>
      ))}
    </div>
  )
}

export function Skeleton() {
  return (
    <div aria-busy="true" aria-label="Loading capacity" className="animate-pulse">
      <div className="qg-pbar is-bare">
        <span className="h-[13px] w-[58px] rounded bg-card-2" />
      </div>
      <SkeletonCard rows={3} />
      <SkeletonCard rows={3} />
    </div>
  )
}
