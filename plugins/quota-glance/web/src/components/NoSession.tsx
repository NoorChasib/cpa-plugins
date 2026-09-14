/**
 * What to do when there is no CPA session to borrow.
 *
 * This page has no sign-in of its own by design: it authenticates as whoever is
 * already signed in to the CPA management console, on the same origin. So the
 * fix is never "paste something here" — it is to open the console. Reached two
 * ways: a browser that has never signed in to the console, and one whose key
 * CPA has since stopped accepting.
 */
export function NoSession({ rejected, onRetry }: { rejected: boolean; onRetry: () => void }) {
  const consoleHref = `${window.location.pathname.split("/v0/")[0] || ""}/`

  return (
    <main className="mx-auto flex min-h-dvh max-w-[440px] flex-col justify-center px-[22px] py-8">
      <h1 className="mb-[10px] text-[19px] font-[650] tracking-[-0.015em]">
        {rejected ? "Your session has expired" : "Sign in to CPA first"}
      </h1>

      <p className="mb-[6px] text-[12.5px] leading-[1.6] text-ink-2">
        {rejected
          ? "CPA no longer accepts this browser's management key. Signing in to the console again will restore it."
          : "This dashboard reads your quota through CPA's management API, using the session you already have in the CPA console."}
      </p>
      <p className="mb-[18px] text-[12.5px] leading-[1.6] text-ink-3">
        It keeps no password and no token of its own, so there is nothing to enter here. Open the console, sign in,
        then come back — or reach this page from the <span className="text-ink-2">Quota Glance</span> entry in the
        console sidebar, where you are already signed in.
      </p>

      <div className="flex flex-wrap gap-[8px]">
        <a
          href={consoleHref}
          className="rounded-[8px] border border-line bg-card-2 px-[13px] py-[9px] text-[12.5px]
            font-medium text-ink transition-colors hover:border-accent"
        >
          Open the CPA console
        </a>
        <button
          type="button"
          onClick={onRetry}
          className="rounded-[8px] border border-line bg-card px-[13px] py-[9px] text-[12.5px]
            text-ink-2 transition-colors hover:border-accent hover:text-ink"
        >
          Try again
        </button>
      </div>
    </main>
  )
}
