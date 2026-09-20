# Quota Glance Menu Bar reference

## App boundary

This directory is a standalone Swift macOS companion app, not a CPA native
plugin. It is deliberately absent from the Linux plugin registry and release
workflow. It does not have a Go module or load into CPA.

The native app owns the status icon, popover, URL preference, login-item setting,
and WebKit lifetime. The hosted page owns every quota card, number, action,
credential, and data request. No copy of the dashboard HTML, CSS, API client, or
polling logic is bundled here, and the icon does not fetch its own quota value.
WebKit displays the page at normal scale using its existing narrow layout.

The popover is 400 × 620 points, reduced only when the current screen's usable
area is smaller. It closes on outside clicks or Escape. A single web view lives
for the lifetime of the process; closing the popover does not destroy its data
store or start another page instance.

## Loading and page updates

The first load requests the configured URL without satisfying it from the
local HTTP cache. Reopening the popover revalidates the current dashboard page
with `reloadFromOrigin()`, picking up deployed page changes without rebuilding
the Mac app. Reopening can therefore reset unfinished form input on that page.
An in-progress navigation is left alone. If the web view is on another path,
such as the CPA console's sign-in screen, reopening keeps that page intact.

The context menu's **Show Dashboard** returns to the configured URL after
console sign-in. **Reload Page** revalidates the current page; it does not force
a Quota Cache poll. While open, the page uses its own existing data-refresh
behavior. A code deployment does not replace an already loaded document until
the next page load or reload.

Failed page loads show a native error with **Try Again** and **Settings**.
Reopening retries a failed load. HTTP error responses identify the status code;
network failures do not echo potentially sensitive URLs. Web content process
termination offers recovery instead of leaving an empty popover. Errors in the
page's own quota API continue to use the page's existing UI.

## Authentication and navigation

Use the full page URL, preserving any reverse-proxy prefix. HTTP and HTTPS are
supported, including a local server or a server reachable over a VPN. The bundle
permits HTTP in WebKit for these deployments; it does not override TLS
certificate validation.

The saved URL must not contain URL userinfo or common password/token query
parameters. Sign in on the page using `web-token`, or navigate to the CPA
console through the page's existing link. The app never reads, injects, copies,
or logs that credential. It does not inherit a session from another browser.

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
UserDefaults domain. `WKWebsiteDataStore.default()` keeps website data on disk
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

Run from this directory on macOS with Xcode Command Line Tools (Swift 5.9+):

```sh
make test           # URL validation and navigation behavior
make build          # arm64 + x86_64, merged into a universal .app
make verify-bundle  # bundle metadata, architectures, and signature
make install        # build, copy to ~/Applications, open
make run            # build and open directly from dist
make package        # build and verify, then create a versioned ZIP
make dmg            # build, create and mount-verify a drag-to-Applications DMG
make sign-release   # sign/notarize the existing built app; requires Apple secrets
make ci             # scripts, tests, universal build, verification, ZIP + DMG
```

`VERSION` defaults to `0.1.0` and must be three numeric components. For a faster
local build, use `make build ARCHS=arm64` or `ARCHS=x86_64`. `INSTALL_DIR` can
override the default `~/Applications` install directory. Quit a running copy
before installing its replacement.

Builds use a separate SwiftPM scratch directory per architecture and `lipo` to
merge them. An AppKit script renders the app icon, and `iconutil` creates its
ICNS. The completed bundle is ad-hoc signed and verified. There are no package
dependencies, Xcode project files, signing accounts, or provisioning profiles
to configure. Distribution signed with Developer ID and notarization is not
part of this local build flow.

The DMG uses macOS `hdiutil` to create a compressed, read-only image containing
the signed app, an Applications shortcut, and installation instructions. The
packaging script checks the image, mounts it read-only, verifies the app's
signature and architecture slices inside it, and checks the installation
shortcut before publishing `dist/Quota-Glance-<version>-macOS.dmg`. It detaches
the verification volume on completion. Finder styling and third-party DMG
tools are not required.

The active workflow is
[quota-glance-menubar-ci.yml](../../.github/workflows/quota-glance-menubar-ci.yml).
It runs on macOS, builds both architectures, and uploads the ZIP, DMG, and
checksums. It does not modify either CPA registry or run a plugin release.

## GitHub Releases

Complete the one-time [Apple signing setup](docs/apple-signing.md), push the
committed app and its root workflow to `main`, then push an app-specific tag:

```sh
git tag quota-glance-menubar/v0.1.0 <verified-commit-on-main>
git push origin quota-glance-menubar/v0.1.0
```

The tag must be `quota-glance-menubar/vMAJOR.MINOR.PATCH`. Its version is passed
into the build, bundle metadata, and asset filenames. Ordinary branch builds,
pull requests, and manual workflow runs produce artifacts without publishing.
Manual runs optionally sign and notarize artifacts when requested from `main`.

For a tag, GitHub's macOS runner runs the checks, Developer ID signs and notarizes
the universal app and DMG, staples and validates tickets, assesses Gatekeeper,
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

On the Linux development host, the eight Swift core tests passed in the official
Swift 6.0.3 container. Swift source parsing, shell syntax, bundle plist checks,
and the root workflow's `actionlint` check also passed. Linux
cannot compile AppKit/WebKit, sign the app, or verify native interaction. The
macOS workflow is provided but has not been run as part of this local change.
DMG creation and mounting also require macOS and have not run on this host.
Developer ID signing, notarization, and Gatekeeper assessment additionally need
the user's Apple secrets and have not run as part of this change.

Before treating a Mac build as ready to use:

1. Run `make ci`, then `make install`. Confirm the menu bar icon appears with no
   Dock icon and Settings opens on first launch.
2. Enter the hosted dashboard URL. Sign in with the dashboard password. Open
   and close the popover, scroll to later providers, then quit and relaunch;
   the saved session should still work.
3. Verify ordinary typing, Command-V, Command-A, Escape, outside-click dismissal,
   and right-click/Control-click on the icon. Check a second monitor if used.
4. Use the page's CPA console sign-in link, close and reopen while on that
   screen, and return with **Show Dashboard**. Exercise the page's existing
   confirmation UI against development data, not a real quota action.
5. Deploy a visible page change or use **Reload Page**; verify the next dashboard
   reload shows it. Test an unreachable server, a wrong path, and recovery with
   **Try Again**. Verify Settings remains accessible.
6. Enable Open at login from the installed app and verify macOS registration.
   Test login itself on a Mac with a user session; CI cannot establish it.
7. Run `make dmg`, open the resulting image, and drag the app to Applications.
   Eject the DMG, launch the installed app, and enable Open at login. Confirm it
   opens after logging out and back in. Opening directly from the DMG should
   explain that installation is required before enabling Open at login.

The web preview in `../quota-glance/web/design/menubar-options.html` records the
selected compact design. It is a browser mockup, not native runtime evidence.
Verified API sources are in [docs/apple-api-notes.md](docs/apple-api-notes.md).
