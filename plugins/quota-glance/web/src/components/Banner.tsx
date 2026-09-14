import { SUPPORTED_SCHEMA, type Summary } from "../lib/types"

/**
 * Why the data underneath might not be what it looks like.
 *
 * Every one of these renders *above* the cards and none of them replaces them.
 * A blank page is indistinguishable from a broken install, and the figures from
 * ten minutes ago are almost always still worth reading — what the reader needs
 * is to know which ones they are looking at.
 */
const STALE_MESSAGES: Record<string, string> = {
  neverObserved: "No credential has been polled yet — these figures are not in yet.",
  cacheMissing: "The quota cache could not be read. Showing the last document that was.",
  cacheStale: "The quota cache has not been updated recently. These figures are older than they should be.",
  snapshotSchemaUnsupported: "The quota cache is newer than this dashboard — update the plugin.",
  rosterUnavailable: "CPA could not list credentials, so this may be missing some.",
}

function Strip({ tone, children }: { tone: "warn" | "crit"; children: React.ReactNode }) {
  const border = tone === "crit" ? "border-crit/45" : "border-warn/40"
  const dot = tone === "crit" ? "bg-crit" : "bg-warn"
  return (
    <div
      role="status"
      className={`mb-[18px] flex items-start gap-[9px] rounded-[12px] border ${border} bg-card px-4 py-[11px]`}
    >
      <span className={`mt-[6px] size-[6px] shrink-0 rounded-full ${dot}`} aria-hidden="true" />
      <p className="text-[12.5px] leading-[1.5] text-ink-2">{children}</p>
    </div>
  )
}

export function Banners({ summary, offline }: { summary: Summary | undefined; offline: boolean }) {
  return (
    <>
      {offline && (
        <Strip tone="crit">
          {summary
            ? "Can’t reach the plugin right now. Everything below is the last data that arrived."
            : "Can’t reach the plugin right now."}
        </Strip>
      )}

      {summary && summary.schemaVersion > SUPPORTED_SCHEMA && (
        // Additive evolution means a document from a newer plugin still mostly
        // renders, so this warns and carries on rather than refusing to draw.
        <Strip tone="warn">
          This dashboard reads schema {SUPPORTED_SCHEMA} and the plugin is serving schema {summary.schemaVersion}. Some
          of what follows may be missing — update the dashboard.
        </Strip>
      )}

      {summary?.stale && (
        <Strip tone="warn">
          {STALE_MESSAGES[summary.staleReason ?? ""] ??
            `The quota cache reported "${summary.staleReason}". These figures may be out of date.`}
        </Strip>
      )}
    </>
  )
}
