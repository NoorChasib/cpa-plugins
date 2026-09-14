import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { App } from "./App"
import { captureTokenFromURL } from "./lib/token"
import "./index.css"

// Before anything renders or fetches, so a `…/app?token=…` link is honoured and
// the value is out of the address bar in the same tick.
captureTokenFromURL()

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Every consumer reads the same document from the same key; refetching is
      // driven by the poll interval rather than by mounting.
      staleTime: 30_000,
      gcTime: Infinity,
    },
  },
})

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
)
