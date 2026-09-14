import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from "vite"
import { viteSingleFile } from "vite-plugin-singlefile"

import { goldenFixtureRoute } from "./dev/fixture-route.ts"

// The plugin serves this app from a CPA resource route, and those are
// exact-path and GET-only: there is no prefix match, so a directory of hashed
// assets would need a registered route per file. Everything therefore inlines
// into one document, which go:embed picks up at web/dist/index.html.
//
// The same build also has to make zero external requests. The access token
// arrives in the URL on first visit, and a single third-party fetch would carry
// the page URL in a Referer header. No CDN fonts, no icon service, no
// analytics: system font stacks and inline SVG only.
export default defineConfig(({ command }) => ({
  plugins: [
    react(),
    tailwindcss(),
    viteSingleFile(),
    // Serve-time only. It reads the golden fixtures off disk and never enters
    // the bundle, which is what keeps fixture emails out of the public shell.
    command === "serve" ? goldenFixtureRoute() : null,
  ],
  build: {
    outDir: "dist",
    // Inline everything. A stray asset reference is a broken page here, not a
    // second request.
    assetsInlineLimit: 100_000_000,
    cssCodeSplit: false,
    // A source map would inline into the same document and triple its size.
    sourcemap: false,
    // There is nothing to preload: the module is already in the document that
    // the browser is parsing. Without this the bundle carries Vite's
    // modulepreload polyfill to manage links that will never exist.
    modulePreload: false,
    // The bundle is committed and `make ci` diffs it against a fresh build, so
    // the output has to be reproducible: no timestamps, one chunk.
    reportCompressedSize: false,
  },
  server: {
    port: 5173,
    // Point at a real CPA to develop against live data; without it the golden
    // fixture route above answers instead, which is the default path and needs
    // nothing running.
    proxy: process.env.QUOTA_GLANCE_PROXY
      ? { "/v0": { target: process.env.QUOTA_GLANCE_PROXY, changeOrigin: true } }
      : undefined,
  },
}))
