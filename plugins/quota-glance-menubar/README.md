# Quota Glance for Mac

Your hosted Quota Glance page in a **400 × 620** menu bar popover. The app loads
the page directly in WebKit, so its design and features stay in the web app.

Requires macOS 13 or newer and a working
[Quota Glance](../quota-glance/README.md) page. Supports Apple silicon and Intel.

## Install from a DMG

Open `Quota-Glance-0.4.0-macOS.dmg`, drag **Quota Glance** to **Applications**,
then eject the disk image and open the installed app. Right-click its menu bar
icon, choose **Settings**, and enable **Open at login** to start it whenever you
sign in, including after a restart. Your saved URL and session persist.

Tagged app releases publish the DMG and ZIP on the repository's
[GitHub Releases page](https://github.com/NoorChasib/cpa-plugins/releases).
Look for **Quota Glance for Mac**. The **Quota Glance Menu Bar** workflow also
keeps downloads as build artifacts. Tagged releases require Developer ID
signing and Apple notarization; see the one-time
[Apple account setup](docs/apple-signing.md) before publishing the first release.

## Update from the menu bar

From v0.3.0 onward, right-click the icon and choose **Check for Updates…**.
The app downloads verified updates from GitHub, installs them, and relaunches.
It also checks daily by default. **Settings** lets you turn checks off or opt
into automatic downloading and installation. Your dashboard and preferences
are retained. If you are on v0.2.0 or earlier, install the new DMG once to get
this updater.

## Build on your Mac

On your Mac, install Xcode Command Line Tools if needed (`xcode-select --install`),
then run these commands from this repository:

```sh
cd plugins/quota-glance-menubar
make install
```

This builds a universal app, installs it at `~/Applications/Quota Glance.app`,
and opens it. SwiftPM downloads the pinned Sparkle updater dependency.

To create the drag-to-Applications disk image instead:

```sh
make dmg
```

The image is written to `dist/Quota-Glance-0.4.0-macOS.dmg`.
Local builds are ad-hoc signed; notarized release builds run through GitHub
after the Apple signing secrets are configured.

On first launch, paste your complete dashboard URL, for example:

```text
https://your-server/v0/resource/plugins/quota-glance/app
```

Choose **Save and Open**, then sign in using the page's dashboard password
(`web-token`) or its CPA console link. The app remembers its own session;
Safari and Chrome sessions are separate. Enter the password on the page instead
of adding `?token=` to the saved URL. Once signed in with the dashboard
password, **Use one** spends a banked reset from the popover just as it does on
the page (Quota Glance 0.5.0 or newer).

Click the menu bar item to open or close the page. Scroll inside the
popover to see more. Right-click or Control-click the item for **Settings**,
**Show Dashboard**, **Reload Page**, **Open in Browser**, **Check for Updates…**,
and **Quit**.

## Choose what the menu bar shows

In **Settings → Menu bar**, **Show** picks one of four styles:

- **Icon only**: the chart icon.
- **Percent**: the icon and one window's remaining percentage, for example
  `59%` (the form before 0.4.0).
- **Lettered pair**: one to three windows as a letter and a number, for
  example `S94 W59`.
- **Split pill**: the same windows in a rounded box with one half per window.

Under **Windows**, pick one to three windows from any provider and drag them
into order. The letter beside each is the one the menu bar draws: **S**
session, **W** weekly, **F** weekly (Fable), **C** credits. **Badge** adds a
small provider logo to each number: **Always** (the default), **When
providers differ**, or **Off**. The preview shows the result on a dark and a
light menu bar; the menu bar changes when you click **Save and Open**.
Readings update every minute while your Mac is awake, including when the
popover is closed. A missing or out-of-date reading shows **—**.

Updating from 0.3 keeps your menu bar as it was: a chosen quota stays
**Percent** and Icon only stays **Icon only**. A new install starts with
Lettered pair, Claude Session and Claude Weekly. Enable **Open at login** if
desired.

See [REFERENCE.md](REFERENCE.md) for refresh behavior, builds, stored data,
and Mac verification steps.
