import { useState } from "react"

/**
 * The two ways in, in the order that costs the reader least.
 *
 * Opened from the CPA console sidebar this screen never appears: the page finds
 * the console's own session and spends it. It appears when there is no such
 * session — a phone, a bookmark, a browser that has never signed in — and then
 * it offers both routes rather than only the one it cannot perform for you.
 *
 * The password is the plugin's `web-token`. Saved here, it is what gets sent on
 * every later visit from this browser, so the screen is seen once.
 */
export function SignIn({ rejected, onPassword }: { rejected: boolean; onPassword: (value: string) => void }) {
  const [value, setValue] = useState("")
  const trimmed = value.trim()
  const consoleHref = `${window.location.pathname.split("/v0/")[0] || ""}/`

  return (
    <main className="mx-auto flex min-h-dvh max-w-[420px] flex-col justify-center px-[22px] py-8">
      <h1 className="mb-[10px] text-[19px] font-[650] tracking-[-0.015em]">
        {rejected ? "That didn’t work" : "Sign in"}
      </h1>
      <p className="mb-[22px] text-[12.5px] leading-[1.6] text-ink-2">
        {rejected
          ? "Your CPA session or saved password was refused. Sign in to the console again, or enter the password below."
          : "This browser has no CPA console session, so there are two ways in."}
      </p>

      {/* First, because it costs nothing and is what the sidebar does. */}
      <a
        href={consoleHref}
        className="rounded-[8px] border border-line bg-card-2 px-[13px] py-[10px] text-center text-[12.5px]
          font-medium text-ink transition-colors hover:border-accent"
      >
        Sign in to the CPA console
      </a>
      <p className="mb-[18px] mt-[7px] text-[11.5px] leading-[1.55] text-ink-3">
        Then reopen this page, or use the <span className="text-ink-2">Quota Glance</span> entry in the console
        sidebar, where you are already signed in.
      </p>

      <div className="mb-[18px] flex items-center gap-[10px]">
        <span className="h-px flex-1 bg-line" />
        <span className="text-[11px] text-ink-3">or</span>
        <span className="h-px flex-1 bg-line" />
      </div>

      <form
        onSubmit={(event) => {
          event.preventDefault()
          if (trimmed) onPassword(trimmed)
        }}
      >
        {/* type=password so it is not left legible on a screen someone else can
          * see; autoComplete off so browsers do not keep a second copy. */}
        <input
          type="password"
          value={value}
          onChange={(event) => setValue(event.target.value)}
          placeholder="Dashboard password"
          aria-label="Dashboard password"
          autoComplete="off"
          autoCorrect="off"
          autoCapitalize="off"
          spellCheck={false}
          className="num w-full rounded-[8px] border border-line bg-card px-[11px] py-[9px] text-[12.5px]
            text-ink outline-none placeholder:text-ink-3 placeholder:font-sans focus:border-accent"
        />
        <button
          type="submit"
          disabled={!trimmed}
          className="mt-[8px] w-full rounded-[8px] border border-line bg-card px-[11px] py-[9px]
            text-[12.5px] font-medium text-ink transition-colors
            hover:border-accent disabled:cursor-not-allowed disabled:opacity-45 disabled:hover:border-line"
        >
          Save and continue
        </button>
      </form>
      <p className="mt-[7px] text-[11.5px] leading-[1.55] text-ink-3">
        This is <code className="num">web-token</code> from the plugin’s configuration. Leave it unset and one is
        generated and printed once in the CPA log at startup. Saved in this browser, so you are asked once.
      </p>
    </main>
  )
}
