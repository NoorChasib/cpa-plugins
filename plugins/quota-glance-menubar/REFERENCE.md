# Quota Glance Menu Bar reference

## App boundary

This directory is a standalone Swift macOS companion app, not a CPA native
plugin. It is deliberately absent from the Linux plugin registry and release
workflow. It does not have a Go module or load into CPA.

The native app owns the status icon, popover, URL preference, login-item setting,
and WebKit lifetime. The hosted page owns the dashboard UI and sign-in. A small
bundled JavaScript bridge observes its summary responses and supplies quota
labels and remaining percentages to the native menu bar. No copy of the
dashboard HTML or CSS is bundled here.
WebKit displays the page at normal scale using its existing narrow layout.

The popover is 400 × 620 points, reduced only when the current screen's usable
area is smaller. The status item acts on the press, as system menu bar items
do: a press opens the popover, and a press while it is open closes it. A
Control-press opens the context menu; a right-click opens it on release, since
acting on the right press can leave the icon highlighted and make the next
click only clear it. The popover also closes on outside clicks and on Escape.
The page gets Escape first, and the bridge closes an open page dialog and keeps
the key, so Escape closes the dialog and the next one closes the popover; while
the loading or error view shows, Escape closes the popover at once. A single
web view lives for the lifetime of the process; closing the popover does not
destroy its data store or start another page instance.

## Loading and page updates

Quota Glance serves the page with `Cache-Control: no-cache` and an ETag, and
the app loads it under the normal HTTP cache rules, so at launch WebKit
revalidates its cached copy: a 304 while the page is unchanged, the new page
after a deployment. Reopening the popover preserves the loaded document, scroll
position, and unfinished input (except on a dashboard left signed out; see
[Updates and freshness](#updates-and-freshness)). Saving Settings with the
same URL also preserves the page. An in-progress navigation is left alone. The
same applies to the CPA console’s sign-in screen. Use **Reload Page** to pick
up a web app deployment; there is no need to rebuild the Mac app.

The context menu's **Show Dashboard** returns to the configured URL after
console sign-in or a failed load; when the dashboard is already showing, it
opens the popover without reloading it. **Reload Page** downloads the current
page again; it does not force a Quota Cache poll. While open, the page uses its
own existing data-refresh behavior. A code deployment does not replace an
already loaded document until the next page load or reload.

Failed page loads show a native error with **Try Again** and **Settings**.
Reopening retries a failed load. HTTP error responses identify the status code;
network failures do not echo potentially sensitive URLs. Web content process
termination offers recovery instead of leaving an empty popover. Errors in the
page's own quota API continue to use the page's existing UI.

The app also retries a failed load by itself, whatever **Show** is set to. A
load that got no answer (offline, a host that can't be found or reached, a
dropped connection, or a timeout) is retried after 2, 5, 10, 20 and 40 seconds,
then 1, 2, 5 and 10 minutes, then every 15 minutes. Any other failure, such as
an HTTP error, a redirect or navigation away from the server, or a stopped page
process, starts on the slow steps: 1, 2, 5 and 10 minutes, then every 15
minutes, because a quick retry would not change it. The steps start over, and a
waiting retry runs after about 2 seconds, when the network comes back (a usable
connection after none; a VPN or Tailscale connecting or changing alongside a
working connection does not count), when the Mac wakes, and when you open the
popover or choose **Try Again**, **Show Dashboard** or **Reload Page**.

## Menu bar readout

**Settings → Menu bar** chooses what the status item shows. The design is the
signed-off round 2 board,
`../quota-glance/web/design/menubar-readout/refined.html`.

| Show | Draws |
| --- | --- |
| Icon only | the chart icon; no background polling |
| Percent | the icon and the first window's remaining percentage, `59%` (0.3's form) |
| Lettered pair | one to three windows as letter + number, `S94 W59`, plain text |
| Split pill | the same windows in round 1's box, one half per window, `S94 \| W59` |

The percentage is the aggregate **remaining** quota, matching the page.
Windows are chosen from every row the dashboard summary lists, grouped by
provider, in any order (drag to reorder) and from any provider: one to three,
no duplicates. A chosen window that leaves the summary stays in the list,
marked unavailable, and reads **—** until it returns; it keeps the title the
dashboard gives it (Claude · Weekly (Fable), using quota-glance's own provider
and row titles). Until the first summary arrives, chosen windows have no
reading yet and are not reported as missing.

### Letters

The letter belongs to the window, not to its slot, and comes from the
summary's `rowId`:

| rowId | Letter |
| --- | --- |
| `session`, `weekly`, `weekly_fable`, `credits`, `monthly` | S, W, F, C, M |
| `model_session:<model>`, `model_weekly:<model>` | S or W plus the model's first letter: `Ss`, `Ws` (a lowercase `l` is skipped) |
| `raw:<provider>:<id>` | the id's initial, or two letters when that is S, W, F, C, M or digit-like (O, Q, I) |

Letters are compared only within one provider. If two rows of one provider
share a form (two models starting with the same letter), both take one more
letter (`Wsp`, `Wsa`), so a new row in the summary can lengthen an existing
letter. Across providers letters may repeat (Codex W, Claude W); the badge
tells them apart. Settings warns, below the preview, when two chosen windows
would still look alike: same letters and same provider, or same letters with
no badge drawn on either (badges off, or a provider without a bundled logo).
Saving is still allowed. The rules are in
`Sources/GlanceCore/ReadoutLetters.swift`; `scripts/letters-fixture.mjs`
holds the board's own copy and generates the fixture the Swift tests check.

### Badges

**Badge** draws the provider's logo, 9pt square, 1pt after each number with
its top 3pt above the digits' cap height. **Always** is the default; **When
providers differ** draws badges only when the chosen windows span more than
one provider; **Off** draws letters only. A provider without a bundled logo
draws no badge and is spaced as if badges were off (Lettered pair slots 7pt
apart, not 6pt). Logos are bundled SVG files (Claude, OpenAI for Codex, Grok for xAI); replacing them is described in
[docs/logos.md](docs/logos.md).

### Drawing

Lettered pair and Split pill are drawn into one template image per update,
so letters, numbers, badges and the pill's fill share one ink and macOS
supplies white or black, the highlight and the inactive-display fade. There
is **one ink at every level**: no colour or dimming for a low reading, and a
stale or missing reading is **—** in the same ink. Letters are a fixed amount
lighter than their numbers. The image is redrawn when the menu bar's
appearance changes (on macOS 26, also when the wallpaper behind it changes
brightness) and when Increase Contrast changes; with Increase Contrast,
letters are drawn in full ink (Lettered pair: semibold) and the pill's fill
is stronger.

- **Lettered pair**: letter and number in 12pt medium, 1pt apart. Each
  number has a two-digit slot, so 94, 6 and — keep the width; a 100 widens
  its own slot by one digit. Slots are 6pt apart with badges, 7pt without.
- **Split pill**: round 1's box, 18pt tall, 5pt outer corners, square inner
  corners, a 1pt clear divider. Letter 12.5pt semibold, number 12.5pt bold.
  The halves are equal: each is as wide as the widest half's two-digit ink
  (letter to badge, or to the number without badges) plus 5.5pt each side,
  rounded to a whole point. A 100 fits in that padding, so the pill keeps
  its width at every reading; other readings are centred by their ink.
- **Show the Quota Glance icon** adds the chart icon, 7.5pt before either
  style. It is off for a new install and on for anyone updating from 0.3.

The status item is written only when something it shows changes. Before each
update the app compares what it would show (the length, the drawn image's
readings and options, the title, the tooltip and the VoiceOver label) with what
it already shows, and writes only the parts that differ; when nothing differs
it writes nothing. Each write makes macOS redraw the item's copy on every other
display's menu bar, and each copy briefly gives the button that bar's
appearance, which reports an appearance change. 0.4.0 rewrote the item on every
such change, which could keep a CPU core busy on a Mac with more than one menu
bar (two or more displays with **Displays have separate Spaces** on). The app
reads the appearance only after macOS has restored it, so a real light/dark or
wallpaper change still redraws once. Icon only and Percent draw a template
symbol and a title, so an appearance change writes nothing in those styles.

The status item's tooltip has one line per window; VoiceOver reads, for
example, "Quota Glance. Claude session 94 percent remaining, Codex weekly 66
percent remaining", or "no current reading" for a dash.

### Updates and freshness

Whenever Show is not Icon only, a native timer requests the existing cached
summary every 60 seconds (with up to six seconds of timer tolerance), even when
the popover is closed. The request does not invoke provider polling, navigate,
or reload the page. The bridge reuses the page’s successful same-origin GET
request, including its authentication, proxy prefix, and ETag. Native messages
contain only quota IDs, labels, percentages, and freshness state. Unauthorized
responses stop background credential retries until the page signs in
successfully again. With Icon only no readout timer runs; opening Settings
before any summary has loaded requests it once (now, or when the page next
finishes loading) so the windows can be chosen.

Before repeating a request made with the CPA console's key, the bridge checks
that the server answers at all: a `HEAD` request, without credentials, for the
dashboard page itself. The page is on CPA's resource tree, where CPA checks no
key and serves only `GET`, so CPA answers the check with a 404 that its sign-in
ban does not count. Any answer, whatever its status, lets the console request
go ahead, unless the page read through the console itself while the check was
out, as it does when the network comes back: the page's answer stands, and
after a refusal or no answer the bridge does not present the key again. With
no answer to the check the key was never sent, so the bridge keeps the
request and the app tries again after 2, 5, 10, 20 and 40 seconds, then at the
regular minute. The app asks for a reading as soon as the Mac wakes, usually
before Wi-Fi or a VPN is back, and the check is what keeps a console sign-in's
readout from going dark then.

A console request that still gets no answer after the server answered the check
(a network error, the 20-second deadline, or a redirect, which may be a proxy
hiding CPA's refusal) is not repeated, because CPA may have counted it. The
bridge forgets that request until the page reads through the console again, and
the app does not reload the page for lack of a reading: a reload would make the
page forget that the key went unanswered and present it again. The readout then
shows **—** until the page reads through the console again. When the bridge's
request was the one unanswered, opening the popover is enough, because the page
reads again while it is showing. When the page's own read was, the page stops
reading too and says so in a banner, and its **Try again** resumes both. (0.4.0
repeated that request at the next minute.) A background request made with the
dashboard password is sent without the check, and one that gets no answer is
retried on the same quick steps.

A scoped App Nap activity keeps this user-requested readout active while the
Mac is awake; it permits normal system sleep. Wake requests a fresh reading.
Icon-only mode stops the readout timer and activity. macOS scheduling and
network availability may delay updates. Missing quota data, stale server data,
failed requests, and readings older than 150 seconds display **—** (**—%** in
Percent). A Claude session whose every reporting account has spent its weekly
limit shows **0**, because Quota Glance 0.6.0 and newer report those accounts
as held out rather than missing. The last quota list remains available in
Settings across temporary failures. Settings is built the first time it opens,
not at launch, and while it is closed new readings cost it nothing; it shows
the latest list when it opens. A failed page load or a stopped WebKit process
is retried as described under
[Loading and page updates](#loading-and-page-updates). While the readout is
on, a page that loaded but has produced no reading after three polls is also
loaded again, on the slow steps, but only while the popover is closed: an open
page retries its own reads and may be in use. The app tries this at most three
times (on the 1, 2 and 5-minute steps), then stops until a reading arrives or
the steps start over. A page that still reads nothing by then, usually a
dashboard left signed out, is waiting for you, and each load would download it
again and discard anything typed into it.

The normal page continues using its own refresh behavior. Background readout
requests do not rewrite the dashboard’s UI or affect its open dialogs. No
hosted-page change is required for summary schema version 1. A future breaking
summary schema change would require updating this small native bridge.

## Authentication and navigation

Use the full page URL, preserving any reverse-proxy prefix. HTTP and HTTPS are
supported, including a local server or a server reachable over a VPN. The bundle
permits HTTP in WebKit for these deployments; it does not override TLS
certificate validation.

The saved URL must not contain URL userinfo or common password/token query
parameters. Sign in on the page using `web-token`, or navigate to the CPA
console through the page's existing link. The bridge reuses the successful
summary request inside WebKit; credentials never cross the native message
boundary or enter preferences or logs. The app does not inherit a session
from another browser.

Settings also refuses an address whose path contains `/v0/management/`, CPA's
Management API. CPA takes the management key only in a request header, which a
page load cannot send, and counts every request without one toward its sign-in
ban. Use the page's own address, ending in
`/v0/resource/plugins/quota-glance/app`. A management address saved by an
earlier version is no longer loaded. Settings opens at launch with that address
in the field and the reason it was refused below it, and the address stays
saved until a valid one replaces it.

Once signed in with `web-token`, **Use one** spends a banked reset from the
popover as it does on the page, with Quota Glance 0.5.0 or newer. The page
sends that press itself, as a GET to its `/spend` route; the bridge passes it
through untouched and never repeats it, and credentials still never cross the
native boundary. Which sign-in the page uses, and how it avoids costing CPA
management sign-ins, is in
[Quota Glance's access reference](../quota-glance/docs/access.md).

Same-origin navigation stays inside the web view, including the console and
its sign-in flow. A user-clicked external HTTP(S) link opens in the default
browser. Other cross-origin navigation and non-web URL schemes are blocked.
If a server moves to a different origin, update the configured URL. Same-origin
links targeting a new window load in the existing web view. HTML dialogs work
in WebKit; JavaScript alert, confirm, and prompt use native dialogs.

**Open in Browser** opens the configured URL without passing any web view
session or password. You may need to sign in separately in that browser.

## Preferences and login items

The bundle ID is `com.noorchasib.quota-glance-menubar` and the executable is
`QuotaGlance`. The `dashboardURL` preference is stored in that app's standard
UserDefaults domain. `menuBarReadout` stores the menu bar choice as one JSON
value: `style` (`iconOnly`, `percent`, `letteredPair` or `splitPill`), the
ordered `windows` (one to three `{providerID, rowID}`), `badge` (`always`,
`whenProvidersDiffer` or `off`) and `showsAppIcon`. Unknown or malformed
values fall back to their defaults. Saving Settings always stores it; saving
a different dashboard URL resets the windows to Claude Session + Claude
Weekly and keeps the style. If a dashboard's first summary has no Claude
rows and the windows are still that default pair, they become its first
provider's first two rows (recorded per URL in `menuBarReadoutFirstSummary`,
so this happens once).

On the first launch of 0.4.0 the value is resolved once and written back,
first match wins:

1. a stored `menuBarReadout`, as is;
2. 0.3's `menuBarQuota` → Percent with that window, badge Always, icon on
   (nothing visible changes);
3. a saved `dashboardURL` without `menuBarQuota` (Icon only in 0.3) → Icon
   only, with Claude Session + Claude Weekly ready, icon on;
4. nothing saved (a new install) → Lettered pair, Claude Session + Claude
   Weekly, badge Always, icon off.

`menuBarQuota` is left in place, so reinstalling 0.3.1 shows what it showed
before. To repeat the migration, quit the app and run
`defaults delete com.noorchasib.quota-glance-menubar menuBarReadout`.
`WKWebsiteDataStore.default()` keeps website data on disk
under the app's WebKit storage, including the page's saved sign-in. Replacing
the app in place retains these stores. Changing the URL does not delete the
previous server's web data.

**Open at login** uses `SMAppService.mainApp` and reflects the system's actual
registration status. Install the app at its final location before enabling it.
If macOS requires approval, enable Quota Glance in System Settings → General →
Login Items. Disable Open at login before deleting or moving the app.
Registration from a read-only volume, such as the mounted DMG, is refused with
instructions to copy the app to Applications first. Once enabled, macOS opens
the installed app after you sign in following a restart; it does not run as a
system service before sign-in.

The app uses macOS accessory mode and `LSUIElement`, so there is no Dock icon.
The native Edit menu supplies standard copy, paste, undo, and select-all
shortcuts to settings and web inputs.

## Build commands

Run from this directory on an Apple silicon Mac with Xcode Command Line Tools
(Swift 5.9+). The test suite also requires Node.js 20+; the installed app does
not.

```sh
make test           # core state, readout, logos, lifecycle, hidden WebKit, and JS tests
make build          # Apple silicon (arm64) .app
make verify-bundle  # bundle metadata, arm64-only binaries, and signature
make install        # build, copy to ~/Applications, open
make run            # build and open directly from dist
make package        # build and verify, then create a versioned ZIP
make dmg            # build, create and mount-verify a drag-to-Applications DMG
make sign-release   # sign/notarize the existing built app; requires Apple secrets
make appcast        # after signing: generate, sign, and test the update feed
make ci             # scripts, tests, arm64 build, verification, ZIP + DMG
```

`VERSION` defaults to `0.5.0` and must be three numeric components. The app is
Apple silicon only: there is no Intel or universal build. Every goal except
`test` and `check-scripts` stops with an error if `ARCHS` names anything other
than `arm64`, whether it is set on the command line or in the environment;
plain `make` runs `test`, so it ignores `ARCHS` too. `INSTALL_DIR` can
override the default `~/Applications` install directory. Quit a running copy
before installing its replacement.

Builds compile one arm64 executable in the `.build/arm64` SwiftPM scratch
directory. An AppKit script renders the app icon, and `iconutil` creates its
ICNS. The provider logos in `Resources/Logos/` are copied into
`Contents/Resources/Logos/`, and `verify-bundle.sh` checks all three. SwiftPM verifies the checksum of the pinned Sparkle 2.10.0 binary
package. Sparkle ships only a universal framework, so the build copies it with
`ditto --arch arm64`, which removes the Intel slice from every binary inside
(the framework, Autoupdate, Updater.app, and both XPC services) and keeps its
symlinks; it also embeds Sparkle's license. All nested helpers, the framework,
and the app are then signed inside out. `verify-bundle.sh` finds every Mach-O
file in the app by its magic number (thin, fat or fat64) and fails if any
contains an architecture other than arm64, or if it finds fewer than the six
known binaries. The local bundle is ad-hoc signed; no signing account or
provisioning profile needs configuring. Distribution signed with Developer ID
and notarization is not part of this local build flow.

The DMG uses macOS `hdiutil` to create a compressed, read-only image containing
the signed app, an Applications shortcut, and installation instructions. The
packaging script checks the image, mounts it read-only, verifies the app's
signature and arm64-only binaries inside it, and checks the installation
shortcut before publishing `dist/Quota-Glance-<version>-macOS.dmg`. It detaches
the verification volume on completion. Finder styling and third-party DMG
tools are not required.

The active workflow is
[quota-glance-menubar-ci.yml](../../.github/workflows/quota-glance-menubar-ci.yml).
It runs on an Apple silicon macOS runner, builds the arm64 app, and uploads the
ZIP, DMG, and checksums. It does not modify either CPA registry or run a plugin
release.

## In-app updates

Sparkle 2.10.0 owns downloading, signature verification, installation, and
relaunch. **Check for Updates…** is available from the status icon and application
menu; its enabled state follows Sparkle’s updater state. The popover closes when
a manual check starts. Automatic checks run daily by default. Settings binds
directly to Sparkle’s persistent preferences, so launch never resets a user’s
choice. Automatic downloading and installation is opt-in. Sparkle may install
on quit or prompt a long-running app to relaunch; it handles authorization if
the install location needs it. Installation from a mounted DMG is unsupported;
copy the app to Applications first.

Both the archive and appcast have Ed25519 signatures. `SUPublicEDKey` is embedded
in the signed bundle; `SUVerifyUpdateBeforeExtraction` and `SURequireSignedFeed`
require verification before extracting updates and when reading feed content.
Apple Developer ID signatures and notarization remain required. The native
updater contacts GitHub independently of the dashboard; it does not send the
dashboard URL, credentials, or quota data. System profiling is disabled by
Sparkle’s default.

Releases after v0.4.0 are Apple silicon only. Because the app has no Intel
slice, Sparkle's `generate_appcast` adds
`<sparkle:hardwareRequirements>arm64</sparkle:hardwareRequirements>` to the
signed feed item, and `publish-appcast.py` refuses a feed without it. Sparkle
2.9 and later, including the 2.10.0 in every updater-enabled release, skip such
an item on an Intel Mac, so universal v0.3.0 to v0.4.0 installs on Intel stay on
their version; a manual check reports that the Mac is too old. Details are in
[docs/updates.md](docs/updates.md#apple-silicon-only-updates).

The fixed feed URL is the `appcast.xml` file on the repository’s
`quota-glance-updates` branch. It contains the latest complete DMG from a public
app-specific GitHub release; no deltas are generated. The release job generates
and signs the feed using the exact Sparkle tools verified by SwiftPM. It checks
that the signing seed matches the bundle public key, then probes the valid and
altered feeds using Sparkle and the actual app bundle; the valid feed must also
carry the arm64 requirement as Sparkle reads it. After publishing the
GitHub release, the job commits the signed feed bytes unchanged through GitHub’s
Contents API. A retry is idempotent, and a slower older release cannot replace
a newer feed. GitHub’s raw-file cache can delay visibility for a few minutes.

The `SPARKLE_ED25519_PRIVATE_KEY` Actions secret stores the base64-encoded
32-byte seed, separately from Apple signing credentials. Keep an offline backup;
never commit it. Setup and recovery are in [docs/updates.md](docs/updates.md).
Users on versions before v0.3.0 must install the updater-enabled DMG once.

## GitHub Releases

Complete the one-time [Apple signing setup](docs/apple-signing.md), push the
committed app and its root workflow to `main`, then push an app-specific tag:

```sh
git tag quota-glance-menubar/v0.5.0 <verified-commit-on-main>
git push origin quota-glance-menubar/v0.5.0
```

The tag must be `quota-glance-menubar/vMAJOR.MINOR.PATCH`. Its version is passed
into the build, bundle metadata, and asset filenames. Ordinary branch builds,
pull requests, and manual workflow runs produce artifacts without publishing.
Manual runs optionally sign and notarize artifacts when requested from `main`.

For a tag, GitHub's macOS runner runs the checks, Developer ID signs and notarizes
the app and DMG, staples and validates tickets, assesses Gatekeeper,
and creates the final ZIP, DMG, and SHA-256 checksums. Only after success does a separate job with
`contents: write` download and verify those exact artifacts, stage a draft
release with the files, and publish it. The machine pushing the tag needs no Mac
or local certificate; the runner uses the configured Apple secrets. GitHub Actions must be enabled and the repository
must permit the workflow's GitHub token to create releases.

Release titles use **Quota Glance for Mac v<version>**. This workflow does not
replace the monorepo's Latest release, overwrite existing release assets, or
update a CPA catalog. If publication fails after creating a draft, inspect the
draft and attached assets before publishing it manually or deleting the failed
draft and rerunning the publish job. Failed build checks publish nothing.

Local and ordinary CI builds remain ad-hoc signed. Tagged releases require
the configured Developer ID certificate/private key and notarization credentials;
missing secrets, invalid signatures, or failed notarization stop publication.
Full setup, keychain handling, and troubleshooting are in
[docs/apple-signing.md](docs/apple-signing.md). The login-item feature works
with either signing mode and remains a user preference.

## Verification

The suite contains 114 Swift tests and 36 JavaScript tests. GlanceCore (81
tests, which also run on Linux) covers the readout preference and its
migration, letters against the board-generated fixture and the look-alike
warning, cells before and after the first summary, fallback titles,
accessibility and tooltip text, the pill and slot arithmetic, the SVG logo
reader against the three bundled files, which status item writes each change
needs (none for an unchanged readout), the retry schedule (including when it
stops reloading a page that never reads), the status item press that closed the
popover, and dashboard URL validation, including refused Management API
addresses. The app tests (33, macOS only) cover popover reopening without
reload, Show Dashboard leaving a showing dashboard alone, the cache policy,
retry scheduling (started over only when the network comes back, not on a VPN's
interface changes), Escape and focus, migration and first-summary adoption
through real UserDefaults, rendering (template images, stable widths for any
two-digit reading, a 100 that fits the pill, one ink at every level, providers
without a logo, light versus dark and Increase Contrast drawings), the
appearance loop (a stand-in for macOS's snapshot of another menu bar must cause
exactly one write, and more than 50 with the comparison turned off), and a
native refresh through the full message bridge in a real WebKit view with no
window attached, including a console read with no answer that is never repeated
and a console replay that waits until the server answers. Bridge fixtures also
exercise conditional responses, authentication fallback, failures/recovery, the
reachability check before a console replay (and the page's own console read
during it, whose answer stands), console reads with no answer, concurrent
refreshes, preservation of dashboard action request bodies, and Escape with a
page dialog open. Multi-minute updates, sleep/wake, and native Settings
interaction still need a user session on a Mac; the hidden-WebKit test checks
the update mechanism, not OS scheduling.

The [first native macOS build](https://github.com/NoorChasib/cpa-plugins/actions/runs/35541273761)
passed on 2026-09-20: all eight Swift tests, Apple silicon and Intel compilation
(builds have since become arm64 only), bundle metadata and ad-hoc signature
checks, ZIP creation, and DMG creation, mounting, and payload verification.
Native popover interaction and login-item behavior still require a user session
on a Mac.

On the Linux development host, the same eight core tests passed in the official
Swift 6.0.3 container. Swift source parsing, shell syntax, bundle plist checks,
ShellCheck, and the root workflow's `actionlint` check also passed. Developer ID
signing, notarization, and Gatekeeper assessments run separately on GitHub's Mac
runner before a tagged release can be published.

The [first signed build](https://github.com/NoorChasib/cpa-plugins/actions/runs/35541336282)
also passed on 2026-09-20: Developer ID signing, Apple notarization of the app
and DMG, stapled ticket validation, and Gatekeeper assessments for both.

Before treating a Mac build as ready to use:

1. Run `make ci`, then `make install`. Confirm the menu bar icon appears with no
   Dock icon and Settings opens on first launch.
2. Enter the hosted dashboard URL. Sign in with the dashboard password. Open
   and close the popover, scroll to later providers, then quit and relaunch;
   the saved session should still work.
3. Verify ordinary typing, Command-V, Command-A, outside-click dismissal, and
   right-click/Control-click on the icon. With a page dialog open, Escape
   closes the dialog and keeps the popover; the next Escape closes the popover.
   With the popover open, a click on the icon closes it and it stays closed,
   and the next click opens it, with a physical click and with tap-to-click.
   Right-click opens the menu on release and Control-click on press; the
   icon's highlight clears when the menu closes, and the next click opens the
   popover. Escape, then an immediate click on the icon, opens the popover
   again. Check each display if you use more than one.
4. Use the page's CPA console sign-in link, close and reopen while on that
   screen, and return with **Show Dashboard**. On the dashboard itself, **Show
   Dashboard** opens the popover without reloading the page. Exercise the
   page's existing confirmation UI against development data, not a real quota
   action.
5. Deploy a visible page change or use **Reload Page**; verify the next dashboard
   reload shows it. Test an unreachable server, a wrong path, and recovery with
   **Try Again**. Launch with the network off, then turn it on: the page loads
   within seconds without **Try Again**. Verify Settings remains accessible and
   refuses a `/v0/management/` address.
6. In Settings → Menu bar, try each Show style, one to three windows from
   different providers, reordering by drag, and each Badge mode; check the
   preview and the clash warning (Codex Weekly + Claude Weekly with badges
   Off). Save, close the popover for several minutes, and verify the readout
   updates on a light and a dark menu bar, with Increase Contrast, and with
   VoiceOver and the tooltip. Try an unreachable server, sign-out/sign-in,
   sleep/wake, and Icon only. Unavailable data should show —, and valid zero
   quota should show 0. With two or more displays that have different
   wallpapers, **Displays have separate Spaces** on and Settings closed, leave
   each Show style for 5 minutes: Quota Glance's CPU in Activity Monitor must
   stay under 1%, and the readout must still follow a Light/Dark switch.
7. Enable Open at login from the installed app and verify macOS registration.
   Test login itself on a Mac with a user session; CI cannot establish it.
8. Run `make dmg`, open the resulting image, and drag the app to Applications.
   Eject the DMG, launch the installed app, and enable Open at login. Confirm it
   opens after logging out and back in. Opening directly from the DMG should
   explain that installation is required before enabling Open at login.

The web preview in `../quota-glance/web/design/redesign/option-d.html` (open it
with `?view=menubar`) records the selected compact design; the earlier
candidates are in `../quota-glance/web/design/menubar-options.html`. Both are
browser mockups, not native runtime evidence.
Verified API sources are in [docs/apple-api-notes.md](docs/apple-api-notes.md).
