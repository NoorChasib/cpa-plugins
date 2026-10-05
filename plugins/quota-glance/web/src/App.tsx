import { useQuery, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"

import { Access } from "./components/Access"
import { BalanceSections } from "./components/BalanceSections"
import { Banners } from "./components/Banner"
import { Footer } from "./components/Footer"
import { Header } from "./components/Header"
import { SignIn } from "./components/SignIn"
import { ProviderSection } from "./components/ProviderSection"
import { Skeleton } from "./components/Skeleton"
import type { Refusal } from "./lib/access"
import { ConsoleUnansweredError, fetchSummary, NoSessionError, resetCache, summaryRefetchInterval } from "./lib/client"
import { resumeConsole, subscribeReads, useAccess } from "./lib/session"
import * as token from "./lib/token"
import { NowProvider, useClock } from "./lib/now"
import type { Credential, Summary } from "./lib/types"

/** The catalog, by id. Entries name a credential; they do not repeat it. */
function catalogOf(summary: Summary): Map<string, Credential> {
  return new Map(summary.credentials.map((credential) => [credential.id, credential]))
}

function Body({
  summary,
  offline,
  paused,
  onRedeemed,
}: {
  summary: Summary | undefined
  offline: boolean
  /** Reads have stopped until the reader asks again; the banner says why. */
  paused: boolean
  onRedeemed: () => void
}) {
  if (!summary) {
    // Nothing has ever arrived. If the request is still in flight that is a
    // first paint; if it failed there is no data to keep showing, and a
    // skeleton that never resolves would imply one is still coming.
    return offline ? (
      <p className="qg-empty">
        {paused
          ? "Nothing has loaded yet."
          : "Nothing has loaded yet. The dashboard keeps retrying; if this persists, check that the plugin is running."}
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
  // Below the quota providers, and whether or not there are any: a prepaid
  // balance belongs to no CPA credential, so an install with none still has
  // one to show.
  const held = summary.balances ?? []
  const balances = <BalanceSections balances={held} />
  if (providers.length === 0) {
    // Not an error, and not a blank page. A pool with no pollable provider in
    // it is an ordinary state on a fresh install.
    return (
      <>
        <p className={`qg-empty ${held.length > 0 ? "mb-[30px]" : ""}`}>
          No credentials with a quota provider yet. Once quota-cache polls one, its windows appear here.
        </p>
        {balances}
      </>
    )
  }

  const credentials = catalogOf(summary)
  return (
    <>
      {providers.map((provider) => (
        <ProviderSection
          key={provider.id}
          provider={provider}
          credentials={credentials}
          onRedeemed={onRedeemed}
        />
      ))}
      {balances}
    </>
  )
}

function Dashboard({
  summary,
  offline,
  refused,
  unanswered,
  actions,
  onSignIn,
  onRetry,
  onRedeemed,
}: {
  summary: Summary | undefined
  offline: boolean
  refused: boolean
  unanswered: boolean
  actions: React.ReactNode
  onSignIn: () => void
  onRetry: () => void
  onRedeemed: () => void
}) {
  return (
    // 820px wide at most, which is the CPA sidebar's page; the same layout
    // narrows to 400px for the menu bar's popover and 390px for a phone, both
    // of which load this exact page.
    <div className="qg-page">
      <Header summary={summary} offline={offline} actions={actions} />
      <Banners
        summary={summary}
        offline={offline}
        refused={refused}
        unanswered={unanswered}
        onSignIn={onSignIn}
        onRetry={onRetry}
      />
      <Body summary={summary} offline={offline} paused={unanswered} onRedeemed={onRedeemed} />
      {summary && <Footer counters={summary.counters} />}
    </div>
  )
}

export function App() {
  const now = useClock()
  const queryClient = useQueryClient()
  // Which ways in this browser has, re-read whenever one opens or closes —
  // here or in another tab — so the key in the header, the poll and the reset
  // buttons all follow it.
  const access = useAccess()
  // The reader asked for the sign-in screen: from the Access dialog, or from
  // the banner over figures whose credential was refused.
  const [signingIn, setSigningIn] = useState(false)

  const query = useQuery({
    queryKey: ["summary"],
    queryFn: ({ signal }) => fetchSummary(signal),
    // The endpoint is a memory read on the plugin and answers If-None-Match
    // with a 304, so a minute is cheap. Nothing downstream polls faster than
    // quota-cache does, so nothing is gained by asking more often. While no
    // way in is open there is nothing to ask with, and the poll stops: a
    // function, because TanStack keeps a fixed interval running whatever the
    // last attempt came to.
    refetchInterval: () => summaryRefetchInterval(),
    refetchIntervalInBackground: false,
    // A missing or rejected credential stays that way until the reader does
    // something about it; retrying only repeats the same rejection. A console
    // read that got no answer is not retried either: CPA may have counted it.
    retry: (failureCount, error) =>
      !(error instanceof NoSessionError || error instanceof ConsoleUnansweredError) && failureCount < 2,
  })

  // Another tab signed in or out of the console, saved a password, or latched
  // a key. A second's pause, because the console writes several keys for one
  // sign-in and each is its own event; then the document is asked for afresh
  // through whichever door is open now. A key refused anywhere stays held
  // back — this only re-reads — and a latch lifted or a password removed in
  // another tab is left to that tab's own request (access.ts rereadAfter).
  useEffect(() => {
    let timer: number | undefined
    const stop = subscribeReads(() => {
      window.clearTimeout(timer)
      timer = window.setTimeout(() => {
        resetCache()
        void queryClient.invalidateQueries({ queryKey: ["summary"] })
      }, 1000)
    })
    return () => {
      stop()
      window.clearTimeout(timer)
    }
  }, [queryClient])

  const refusal = query.error instanceof NoSessionError ? query.error : null
  const unanswered = query.error instanceof ConsoleUnansweredError
  // With nothing on screen the form already is, and asking for it again must
  // not keep it up once another tab's sign-in brings the figures in.
  const signIn = () => {
    if (query.data) setSigningIn(true)
  }
  const actions = <Access state={access} onSignIn={signIn} />

  // Nothing has ever loaded and neither way in worked, or the reader asked for
  // the form. Once a document is in hand a lapsed credential does not replace
  // it — the banner says so and the last good figures stay up — because they
  // are still worth reading while the reader decides what to do about it.
  if ((refusal && !query.data) || signingIn) {
    // What was refused: the last attempt's answer when it found no way in,
    // else what the doors say now, for a reader who opened the form while the
    // figures still arrive through the other door.
    const reason: Refusal | null =
      refusal?.reason ?? (access.tokenRefused ? "token" : (access.consoleLatch?.reason ?? null))
    return (
      <NowProvider value={now}>
        <SignIn
          reason={reason}
          rejected={refusal ? refusal.hadCredential : reason !== null}
          actions={actions}
          onPassword={(value) => {
            token.write(value)
            resetCache()
            void queryClient.resetQueries({ queryKey: ["summary"] })
            setSigningIn(false)
          }}
          onCancel={query.data ? () => setSigningIn(false) : undefined}
        />
      </NowProvider>
    )
  }

  return (
    <NowProvider value={now}>
      {/* query.data survives a failed refetch, which is what keeps the last
        * good figures on screen while the banner explains the silence. */}
      {/* Spending a credit changes nothing this document can show until
        * quota-cache polls again — it owns the count — but everything else on
        * the page is worth bringing forward, and the outcome message says which
        * part is still lagging. */}
      <Dashboard
        summary={query.data}
        offline={query.isError}
        refused={!!refusal && refusal.hadCredential && !!query.data}
        unanswered={unanswered}
        actions={actions}
        onSignIn={signIn}
        // One click, one ask: the hold is lifted and the document asked for
        // once, so a key whose answer is lost again is held back again.
        onRetry={() => {
          resumeConsole()
          void query.refetch()
        }}
        onRedeemed={() => void query.refetch()}
      />
    </NowProvider>
  )
}
