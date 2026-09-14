import { useQuery, useQueryClient } from "@tanstack/react-query"

import { Banners } from "./components/Banner"
import { Footer } from "./components/Footer"
import { Header } from "./components/Header"
import { SignIn } from "./components/SignIn"
import { ProviderSection } from "./components/ProviderSection"
import { Skeleton } from "./components/Skeleton"
import { fetchSummary, NoSessionError, resetCache } from "./lib/client"
import * as token from "./lib/token"
import { NowProvider, useClock } from "./lib/now"
import type { Credential, Summary } from "./lib/types"

/** The catalog, by id. Entries name a credential; they do not repeat it. */
function catalogOf(summary: Summary): Map<string, Credential> {
  return new Map(summary.credentials.map((credential) => [credential.id, credential]))
}

function Body({ summary, offline }: { summary: Summary | undefined; offline: boolean }) {
  if (!summary) {
    // Nothing has ever arrived. If the request is still in flight that is a
    // first paint; if it failed there is no data to keep showing, and a
    // skeleton that never resolves would imply one is still coming.
    return offline ? (
      <p className="rounded-[12px] border border-line bg-card px-4 py-[15px] text-[12.5px] leading-[1.6] text-ink-2">
        Nothing has loaded yet. The dashboard keeps retrying; if this persists, check that the plugin is running.
      </p>
    ) : (
      <Skeleton />
    )
  }

  // Sorted by the server's key. Nothing below this line re-sorts a credential
  // or an entry: those arrive in one order — soonest weekly reset first — and
  // every card repeats it, so the top row is always the credential that
  // recovers next.
  const providers = [...summary.providers].sort((a, b) => a.order - b.order)
  if (providers.length === 0) {
    // Not an error, and not a blank page. A pool with no pollable provider in
    // it is an ordinary state on a fresh install.
    return (
      <p className="rounded-[12px] border border-line bg-card px-4 py-[15px] text-[12.5px] leading-[1.6] text-ink-2">
        No credentials with a quota provider yet. Once quota-cache polls one, its windows appear here.
      </p>
    )
  }

  const credentials = catalogOf(summary)
  return (
    <>
      {providers.map((provider) => (
        <ProviderSection key={provider.id} provider={provider} credentials={credentials} />
      ))}
    </>
  )
}

function Dashboard({ summary, offline }: { summary: Summary | undefined; offline: boolean }) {
  return (
    <div className="mx-auto max-w-[820px] px-[22px] pb-14 pt-8 max-[640px]:px-[14px] max-[640px]:pb-11 max-[640px]:pt-[22px]">
      <Header summary={summary} />
      <Banners summary={summary} offline={offline} />
      <Body summary={summary} offline={offline} />
      {summary && <Footer counters={summary.counters} />}
    </div>
  )
}

export function App() {
  const now = useClock()
  const queryClient = useQueryClient()

  const query = useQuery({
    queryKey: ["summary"],
    queryFn: ({ signal }) => fetchSummary(signal),
    // The endpoint is a memory read on the plugin and answers If-None-Match
    // with a 304, so a minute is cheap. Nothing downstream polls faster than
    // quota-cache does, so nothing is gained by asking more often.
    refetchInterval: 60_000,
    refetchIntervalInBackground: false,
    // A missing or rejected credential stays that way until the reader does
    // something about it; retrying only repeats the same rejection.
    retry: (failureCount, error) => !(error instanceof NoSessionError) && failureCount < 2,
  })

  // Nothing has ever loaded and neither way in worked. Once a document is in
  // hand a lapsed credential becomes an ordinary failed poll — the banner says
  // so and the last good figures stay up, rather than the screen being replaced
  // by a sign-in form over data the reader can still use.
  if (query.error instanceof NoSessionError && !query.data) {
    return (
      <SignIn
        rejected={query.error.hadCredential}
        onPassword={(value) => {
          token.write(value)
          resetCache()
          void queryClient.resetQueries({ queryKey: ["summary"] })
        }}
      />
    )
  }

  return (
    <NowProvider value={now}>
      {/* query.data survives a failed refetch, which is what keeps the last
        * good figures on screen while the banner explains the silence. */}
      <Dashboard summary={query.data} offline={query.isError} />
    </NowProvider>
  )
}
