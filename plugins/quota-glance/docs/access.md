# Access: how the dashboard signs in, and what it may spend

The dashboard page has two ways in, and both of them read the document and
spend banked resets. This page is the reference for which one a browser uses,
what the page keeps in storage, what it does when a credential is refused, and
why none of it can add up to CPA's management IP ban on its own. The routes
themselves — headers, bodies, status codes — are in
[summary-contract.md](summary-contract.md#spending-one--post-redeem-and-get-spend).

Everything here that decides whether a credential may be sent lives in
`web/src/lib/access.ts`, and `make web-test` drives that file as written.

## The two doors

|  | Dashboard password (the token door) | CPA console session (the console door) |
| --- | --- | --- |
| Credential | `web-token`, typed in once per browser, or arriving once in an `…/app?token=` link | the management key the CPA console remembered in this browser |
| Kept in | `quota-glance.token`, this origin's local storage | the console's own `cli-proxy-auth` entry, which the page only reads |
| Reads | `GET /v0/resource/plugins/quota-glance/summary` | `GET /v0/management/plugins/quota-glance/summary` |
| Spends | `GET /v0/resource/plugins/quota-glance/spend` | `POST /v0/management/plugins/quota-glance/redeem` |
| Checked by | the plugin, against a stored SHA-256, in constant time | CPA's management middleware, before the plugin runs |
| What a refusal costs | nothing at CPA; past 20 failures in a minute the plugin answers `429` until the minute is up | one of the **5 failed management sign-ins** after which CPA locks the address out of its whole Management API for **30 minutes** — a missing key counts, and localhost is not exempt |
| Works from | anywhere the page loads | a browser on the console's own origin with a remembered session; from any machine but the one CPA runs on, only with CPA's `allow-remote` |

The asymmetry in the last rows is what everything below is shaped by. A wrong
password costs nothing anyone would notice. A wrong console key, presented on a
timer, costs the whole address its management access in about five minutes.

## Which door the page uses

- **The password first, then the console.** A browser holding both uses the
  password, so it never touches CPA's management sign-in at all.
- **The password door is open** when a password is saved in this browser and
  the plugin has not refused it in this document.
- **The console door is open** when the console's storage holds a key that
  passes the [strict reader](#reading-the-consoles-session) and no
  [latch](#when-cpa-refuses-a-key) holds that key back.
- **Doors are recomputed on every attempt.** A door shut by a refusal a moment
  ago — in this document, or through storage in another — is already skipped.
- **A read** that the plugin refuses on the password door (`401`, `403`, `429`)
  shuts that door for the document and is tried once through the console door,
  if that one is open.
- **A press** goes through one door, chosen before it is sent. A refusal is
  never retried and never falls through to the other door: a refused press may
  be one the reader would not have wanted repeated, and finding out by trying
  the other door could spend a second reset.
- **With no door open the page sends nothing** — no read, no press. It shows
  the sign-in screen, or, with figures already on screen, keeps them under a
  banner saying the sign-in was refused, and the reset buttons go.
- **The document is polled every 60 seconds while a door is open** and the page
  is visible, and not at all while none is.

## What the page keeps, and where

| Key | Kept in | What it is |
| --- | --- | --- |
| `quota-glance.token` | local storage | The dashboard password. Written when you sign in or open a `?token=` link (which is then taken out of the address bar), removed by **Sign out**. |
| `quota-glance.console-refused` | local storage | The console latch, below. |
| `quota-glance.opened` | local storage | Which cards you opened; every card starts shut. Not a credential. The older `quota-glance.collapsed` entry is removed on first load. |
| — | this document's memory | The password the plugin refused, the ids of presses whose answer never arrived, and any console key whose last request got no answer at all. A reload forgets all three. |

The page reads the console's own entries — `cli-proxy-auth`, and the legacy
`managementKey`, `isLoggedIn`, `apiBase` and `apiUrl` — and never writes any of
them. **Sign out** in the Access dialog forgets the dashboard password and
reloads the page; it does not sign you out of the console, which is the
console's own session.

A browser that refuses a storage write — a full quota, some private modes —
still keeps the latch in memory for the life of the document, so a refused key
is not presented again from it.

In the menu bar app all of this lives in the app's own WebKit storage, separate
from Safari's and Chrome's.

## Reading the console's session

The page uses a console key only when the console remembered it on purpose,
still counts itself signed in, and was talking to this CPA. The console's
storage is per origin, not per server, and it can be pointed at any CPA: a key
it remembers for another server is simply a wrong key here, and a counted
failure. The reader is a port of Codex Catalog Filter's, which audited the
console's storage format.

- **`cli-proxy-auth` is authoritative whenever it exists**, after a logout
  included. A value starting `enc::v1::` is base64 XORed with
  `cli-proxy-api-webui::secure-storage|<host>|<user agent>` — reversible
  obfuscation, not encryption. It must then decode as strict UTF-8 and parse as
  JSON. Anything that fails is no session, and the legacy entries are **not**
  consulted beside it.
- **It is accepted only if** `version` is `0`, `state.rememberPassword` is
  `true`, `state.isAuthenticated` is not `false`, `state.apiBase` is this
  page's CPA, and `state.managementKey` is a valid key.
- **The legacy entries** are read only when `cli-proxy-auth` is absent and
  `isLoggedIn` is exactly `"true"`: the base from `apiBase` or `apiUrl`, the key
  from `managementKey`, decoded loosely, under the same checks.
- **"This page's CPA"** means an `http` or `https` URL of at most 2048
  characters, with no user, password, query or fragment, on this page's own
  origin, whose path — with trailing slashes and a `/v8/management` or
  `/v0/management` suffix removed — is whatever sits in front of `/v0/` in the
  page's own path: a reverse proxy's mount point, or nothing.
- **A valid key** is 1 to 4096 characters, with no leading or trailing
  whitespace, no control characters and nothing outside Latin-1, and comes back
  unchanged from a `Headers` object. Anything a header would trim, fold or
  refuse is refused here instead, before it can become a request.
- **Any storage value over 32,768 characters** is no session.

A sidebar signed in with **Remember password** is unaffected by any of this.
What the reader stops using are exactly the sessions that used to cost failed
sign-ins: a legacy key left behind by a console logout, a session for another
server, an entry obfuscated under another user agent.

## When CPA refuses a key

**The console latch.** On the first `401` or `403` from any console request,
read or press, the page writes `quota-glance.console-refused` before whatever
asked for the request is handed anything — first on the status alone, then
again with the reason once the body has been read. Nothing can present the key
in between.

```json
{ "v": 1, "fp": "9f2c3e1a7b4d6c08", "reason": "banned", "status": 403, "at": 1789012800, "until": 1789014580 }
```

| Field | Meaning |
| --- | --- |
| `fp` | FNV-1a 64 of the page's origin and prefix, a newline, and the key, in hex. It binds the latch to that key on this CPA and nothing else. FNV rather than a SHA because `crypto.subtle` does not exist on an insecure origin, and the menu bar app commonly loads the page over plain HTTP on a LAN or tailnet address. It is not a secret, and the latch stores nothing the console's own entry does not. |
| `reason` | `refused`, `banned`, `remote`, `off` or `other`, from [the refusal](#how-a-refusal-is-read). |
| `status` | The status that refused it. |
| `at` / `until` | When, in epoch seconds; and, for a ban, when CPA said it lifts — read from CPA's "Try again in 29m40s", capped at a day, or 30 minutes on when it does not say. `null` otherwise. |

While a latch matches the console's current key, **no document on this origin
presents that key**: not the poll, a focus, a retry, a storage event, a reload,
another tab, or a press. The reset buttons go too, since a press could only be
refused again.

It is pruned whenever it is read and the console holds no key, or a different
one — so signing in to the console again with another key, or signing out of
it, clears it on its own. A latch the page cannot read holds back whatever key
is there. Otherwise it is lifted only by **Try it again** in the Access dialog,
which then asks for the document once. That button is disabled while a ban CPA
named has not lifted, and re-enables by itself when it does. One click presents
the key at most once; a second refusal latches it again.

**The token latch.** A `401`, a `429`, or a `403` other than `cross_site` on
the password door holds that password back for the rest of the document. It is
in memory, and cleared by saving a password, by **Sign out**, or by a reload.
None of these ever reaches CPA's counter: CPA authenticates nothing on a
resource route.

**The Access dialog** — the key at the top right of the page — shows both
doors, which one is in use, and for a latched console key what CPA said. The
key carries a red dot when the saved password has been refused, or when a
latched console key is all the page had to read with; a latched console key
behind a working password gets none, since nothing depends on it.

## Console requests go single file

Every console request in a document, reads and presses alike, goes through one
queue, one at a time, and each re-checks its door when its turn comes. However
many triggers arrive together with a stale key — a poll, a focus, a retry, a
press — the first is refused and latches, and the rest find the door shut and
send nothing.

**Across tabs**, where the browser offers Web Locks, every document on the
origin also takes one lock, `quota-glance:console`, from re-checking the door
to latching a refusal. Tabs a browser restores together, or tabs woken by a
click in another, then take their turns one by one, and each finds the latch
the one before it wrote. Browsers offer Web Locks only to a secure context:
HTTPS, `localhost` or `127.0.0.1`. On a plain-HTTP address — a LAN or tailnet
IP, which is also how the menu bar app often loads the page — each document
queues on its own, and documents that load at the same moment can each send a
stale key once before the first refusal reaches them.

A console read is never cancelled when the dashboard stops wanting it, as it
does on a refetch or an unmount: an abandoned request is one whose refusal the
page never sees, and that would leave a refused key unlatched. It keeps its own
20-second deadline instead, so a hung CPA still surfaces. A password read can be
cancelled, since abandoning it costs nothing.

**A console request that gets no answer** — the connection dropped, the
deadline passed, or something in front of CPA turned its refusal into a
redirect, which the page never follows — may still have been counted by CPA,
and whatever lost that answer would lose every later refusal the same way, so
nothing would ever latch the key. So the page stops reading with that key: no
retry, no poll, no focus refetch, and a banner says so, with **Try again**,
which asks once. Nothing is written to storage, because the key may be fine. A
reload, a different console key, or a press CPA accepts lifts it. A press is
never held back by it, since a press is the reader asking.

**Other tabs.** A `storage` event on the console's entries, the password or
the latch — the console signing in or out, a password saved, a latch written
elsewhere — makes the page wait a second, because the console writes several
keys for one sign-in, and then ask for the document again through whichever
door is open now. Two events are left alone: a latch lifted by **Try it
again**, and a password removed by **Sign out**. The only door either can open
is the console's, and the tab where they happened is about to present that key
itself; every other tab asking at once would turn one click into a request per
tab. Those tabs ask at their own next poll or focus. That listener never lifts
a latch.

## How a refusal is read

| Door | Status | Body `error` | The page's code | What it shuts |
| --- | --- | --- | --- | --- |
| console | 401 | `missing management key`, `invalid management key` | `console_refused` | the console key, `refused` |
| console | 403 | starts with `IP banned` | `ip_banned` | the console key, `banned`, until CPA said |
| console | 403 | `remote management disabled` | `remote_disabled` | the console key, `remote` |
| console | 403 | `remote management key not set` | `management_off` | the console key, `off` |
| console | 401 or 403 | anything else, or not JSON — something in front of CPA | `refused_other` | the console key, `other` |
| password | 401, 429 | — | `token_refused` | the password, for this document |
| password | 403 | `cross_site` | `cross_site` | nothing |
| password | 403 | anything else, or not JSON | `refused_other` | the password, for this document |
| either | 425 | — | `too_early` | nothing |
| either | 404 | `not_found` | `not_found`: redeeming is switched off | nothing |
| either | 404 | anything else | `route_missing`: the server does not offer the route | nothing |

Every row is a request that spent nothing. The plugin never answers `401` or
`403` on the console door, so one there came from CPA or from something in
front of it, before the plugin ran; on the password door the plugin answers
them before it looks at the press.

CPA's strings are matched exactly. They come from `AuthenticateManagementKey`
in CPA's `internal/api/handlers/management/handler.go`, at lines 330, 339, 368,
373 and 392 in CPA v8.0.15. CPA counts only the two `401`s toward its ban; the
`403`s are answered before anything is counted. The page latches all of them
anyway, because none starts working by being asked again. If CPA ever rewords
one, that refusal is read as `refused_other`: still latched, still nothing
spent, only the advice on screen gets vaguer. `make smoke` asserts the two
`401` strings against the pinned CPA image.

## Spending a reset

The press goes through the first open door, chosen before anything is sent.
On the password door it is a `GET` to `/spend` carrying the press in the
`X-Quota-Glance-Spend` header; on the console door it is a `POST` to the
management `/redeem` with the same press as its body. Both carry a press id.

**Press ids.** Each new press gets 16 random bytes from
`crypto.getRandomValues`, which, unlike `crypto.randomUUID`, works on an
insecure origin. The page keeps the id, for at most ten minutes per account,
only while it does not know what the plugin did with it:

- **The answer was not the plugin's.** The request failed; a response at 500
  or above did not carry one of the plugin's own error codes — a gateway's
  page, or a gateway's own JSON such as `{"message":"Endpoint request timed
  out"}`; or a 2xx could not be read.
- **A repeat of such a press was turned away before the plugin ran** — the
  password refused, CPA refusing the console key, redeeming switched off a
  moment, a proxy's own 4xx. That says nothing about the press it repeated,
  so the notice says that press is still unknown, and the press after it still
  sends the same id.

Pressing again in that time sends the same id, and the plugin's press ledger
answers it with whatever the first copy came to, marked with
`X-Quota-Glance-Replayed: 1`, rather than spending again; the page prefixes
that answer with "From your earlier press:". The plugin's own answer settles
the id, and so does a new press turned away. Ids live in memory, so a reload
forgets them, which is no worse than the plugin restarting.

The ledger, the claim journal behind it, and every status a press can come back
with are in
[summary-contract.md](summary-contract.md#one-press-one-spend).

## Why a GET spends

CPA v8.0.15 dispatches only `GET` to a plugin's resource routes
(`ServeResourceHTTP` in CPA's `internal/pluginhost/management.go`), and the
resource tree is the only part of a plugin a browser without the management
key can reach. So on the password door the press is a GET that acts, which is
everything a GET is not supposed to be.

RFC 9110 §9.2.1's requirement is about parameters in the target URI that
select an action, and `/spend` has none: the action is selected by
authenticated headers. It is still a deviation from GET being safe, and it is
fenced off from everything that treats a GET as safe:

- **Nothing in the URL.** The password, the account, the confirmation and the
  press id all travel in headers, so a link, a prefetch, a crawler, a link
  unfurler or a cache that holds the URL holds nothing that can spend.
- **An explicit confirmation**, as on the POST.
- **A single-use press id**, and a ledger that answers any second copy with the
  first copy's answer, so a transport that resends a GET it believes is safe
  gets an answer and not a second spend.
- **Fetch metadata.** A request the browser marks as a navigation, a prefetch
  or coming from another site is refused before the password is looked at.
- **No early data.** A request that may have arrived as TLS 0-RTT data, which
  anyone on the path can replay, is refused with `425`.
- **No caching.** The request carries `Authorization`, which shared caches do
  not store by default (RFC 9111 §3.5), and the answer is `no-store`.

**Retiring it.** The plugin also registers `POST /v0/resource/.../redeem`, which
already carries the password check. If CPA starts dispatching POST to resource
routes, that route works with no plugin change — `make smoke` reports which
behaviour the running CPA has — and the page can send its press there instead
and `/spend` can go. A CPA issue asking `ServeResourceHTTP` to dispatch POST is
the way to get there.

**Why not a management key in the page's settings**, which would have spent
through the existing POST:

- Any client that is not on the CPA host gets `403 remote management disabled`
  unless `allow-remote` is on, so the menu bar app and a phone would have needed
  the whole Management API put on the network.
- It would store a full-admin key — one that can read every OAuth credential
  and change configuration — in plaintext on every device.
- Every mistyped or stale key would be a counted CPA failure, and checking one
  would spend ban budget.

## By surface

| Where | Reads with | Spends with | Failed CPA sign-ins it can cause |
| --- | --- | --- | --- |
| CPA sidebar, no password saved | the console key, every 60 s while visible | `POST .../management/.../redeem` | at most 1 per refused key |
| CPA sidebar or any tab, password saved | the password | `GET .../resource/.../spend` | none |
| A browser tab or a phone | the password | `GET .../spend` | none, and no `allow-remote` needed |
| A tab with no password but a remembered console session on the same address | the console key | `POST .../redeem` | as the sidebar |
| Menu bar app | the password; the app's readout repeats the page's last successful summary request every 60 s or so | `GET .../spend`, which the readout leaves alone | none — unless signed in to the console inside the app with no password, then at most 2 per refused key |

The menu bar readout watches only `GET` requests to the two summary paths. It
passes everything else through untouched, `/spend` included, and drops its copy
of the request on `401`, `403` or `429`. Credentials never cross into the
native app. **Sign out** reloads the page, which discards that copy as well.

## Worst case

What Quota Glance can cost the address, counted in CPA's failed management
sign-ins:

| Situation | At most |
| --- | --- |
| A saved password that works | 0, automatic or manual |
| A console-only browser whose key goes stale | 1 per refused key per browser profile. On a plain-HTTP address, where there are no Web Locks, also 1 from each other tab whose request was already on its way when the first refusal landed — a browser restoring several tabs at once |
| A console request whose answer is lost | the requests the browser itself resends for that one request, then none until **Try again** |
| The menu bar app signed in to the console inside the app, with no password | 2 per refused key: 1 from the page, 1 from the readout |
| **Try it again** | 1 per click |

None of these reaches CPA's 5 without five deliberate actions, with one
exception: five or more tabs loading at the same moment on a plain-HTTP
address, with a stale console key and no saved password. A saved password, or
HTTPS, closes it.

## Guarantees

Each of these is held by a test, named after its number where it can be.

1. Nothing is sent to `/v0/management/*` without a key. No console door, no
   console request.
2. A management key is presented only if the strict reader accepts it and no
   latch holds it.
3. While the password door is open, no `/v0/management/*` request is made at
   all, reads and presses alike.
4. The first `401` or `403` from a console request writes the latch before any
   caller resumes. After that, interval ticks, focus, retries, invalidations,
   storage events, reloads, other tabs once the write reaches them, and later
   presses present that key zero times.
5. Console requests in a document run one at a time, are never cancelled by the
   dashboard abandoning them, and re-check their door right before sending.
   Any number of triggers with a stale key send one request — across every
   document on the origin, where the browser offers Web Locks. A console
   request that gets no answer holds its key back from every later read in the
   document until the reader asks again.
6. A confirmed press is one request, through a door fixed before it is sent,
   never retried and never passed to the other door.
7. With no door open there is no poll, and a read throws before touching the
   network.
8. A latch is lifted only when the console's key is gone or changed, or by
   **Try it again**, which is disabled during a ban and presents the key at
   most once per click. Other tabs do not ask again because a latch was lifted
   or a password removed elsewhere.
9. Refusals on the password door never reach CPA's counter. `401`, `429` and a
   `403` other than `cross_site` hold that password back in memory.
10. The page never writes `cli-proxy-auth`, `managementKey`, `apiBase`,
    `apiUrl` or `isLoggedIn`.
11. The menu bar readout is unchanged: it repeats only summary GETs and drops
    its copy on `401`, `403` or `429`.
12. On the plugin: `/spend` never passes CPA's management middleware; it spends
    only with a valid token, a confirmation, a press id and a credential the
    document offers; a press id spends at most once per plugin process within
    its ten minutes; and the fetch-metadata, early-data and validation
    refusals spend nothing and cost no CPA budget.
13. On the dev server, every spend and redeem path is answered by the stand-in,
    whatever the method, so none is forwarded to `QUOTA_GLANCE_PROXY`.

1 to 10 are in `web/test/access.test.ts`, which also checks 1 after every test
in the file. 11 is the menu bar app's
`plugins/quota-glance-menubar/scripts/tests/readout.test.mjs`. 12 is
`internal/api/spend_test.go`, `ledger_test.go` and `redeem_test.go`, and
`make smoke` against a real CPA. 13 is `web/test/fixture-route.test.ts`.

## Threats, and what bounds them

- **A script running on the same origin** — another plugin's page, the console
  itself — can already read the saved password and the console's remembered
  key; the console's obfuscation is not a security boundary. What changed in
  0.5.0 is that the password now spends. That is bounded by the server-marked
  `redeemable`, the confirmation, one press in flight per account, the press
  ledger, and the providers' own rules. The password cannot read OAuth
  credentials or change configuration. The page's Content-Security-Policy —
  `connect-src 'self'`, `frame-ancestors 'self'`, `form-action 'none'` — is
  defence in depth.
- **Another site** does not know the password, and CPA's
  `Access-Control-Allow-Headers: *` does not cover `Authorization`, which a
  browser requires to be named. The fetch-metadata check refuses a cross-site
  request, and `frame-ancestors 'self'` stops the page being framed. Out of
  scope and unchanged: such a page can still spend an address's ban budget by
  sending wrong keys to CPA's management routes directly.
- **Forged requests, prefetchers, crawlers, link unfurlers.** Nothing is
  ambient: neither CPA nor the plugin sets a cookie. These get `401` (the
  plugin's limiter only) or `403 cross_site`, and nothing is spent.
- **Replays.** A browser's own resend, a proxy retrying a GET against another
  upstream, and an edge replaying 0-RTT data all carry the same press id, so
  the ledger returns the first answer; 0-RTT also gets `425`. A plugin restart
  empties the ledger, and the worst case is then what it was before 0.5.0; the
  claim journal still covers an unknown outcome.
- **Logs.** CPA never request-logs a GET. Its access log records the path and a
  query with any `token` parameter masked. On `/spend` the account id travels
  only in a header, and the password only in `Authorization`; a `?token=` link
  is taken out of the address bar as soon as the page loads. A generated
  `web-token` is printed once in the CPA log, and that line now grants
  spending — set your own if the log is shared.
- **The network.** On plain HTTP, `Authorization` is visible on every request,
  as it always was. Use HTTPS, `tailscale serve` or a VPN. On the password door
  the management key never leaves the console's browser.
- **A shared machine or browser profile.** Anyone using it can view the page
  and, after the confirmation, spend. **Sign out** forgets the password; set a
  new `web-token`; `allow-redeem: false` closes both doors.
- **The menu bar app's storage.** The password sits in plaintext in the app's
  WebKit store. The app runs with no entitlements, so outside the App Sandbox,
  and same-user processes and backups can read it. That is where it always
  was; what it grants now includes spending.
- **Guessing.** A generated token is 256 random bits. Failed attempts are
  limited to 20 a minute across all callers, after authentication, so the right
  token is never slowed. Choose at least 20 random characters for your own.
- **Reverse proxies** must pass `Authorization` and `X-Quota-Glance-Spend` and
  allow at least 60 seconds. One that drops the header gets `400`, and nothing
  is spent. A proxy's own `401` or `403` spends nothing either; the page reads
  it as a refusal of what it sent, and on the console door names it as
  something in front of CPA refusing. A same-host proxy that makes every
  client look like `127.0.0.1` without CPA's `trusted-proxies` puts every
  console user in one ban bucket; that affects only the console door.

## Checking the menu bar app without spending anything

On the Mac, with this repository checked out:

1. In `plugins/quota-glance`, run `go run ./tools/fixtureserve -redeem reset`.
   It serves fixture data on `127.0.0.1:8787` with the password `dev-token`,
   and answers a press with a stand-in that contacts no provider.
2. In the app's **Settings**, set the URL to
   `http://127.0.0.1:8787/v0/resource/plugins/quota-glance/app`.
3. Sign in with `dev-token`.
4. Press **Use one** on any row of a **Banked resets** card and confirm. The outcome line should start
   "Reset applied".
5. Choose a quota under **Menu bar quota** and check the readout keeps
   updating.
6. Open the key at the top right and choose **Sign out**. The page reloads to
   the sign-in screen, and from the readout's next update, about a minute
   later, it shows **—%**.
7. Restore your real URL and choose the menu bar quota again; changing the URL
   clears it.

Never check this against a real CPA or a real account: a press there spends a
real reset.
