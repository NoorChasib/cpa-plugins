# Quota Glance for Mac

Your hosted Quota Glance page in a **400 × 620** menu bar popover. The app loads
the page directly in WebKit, so its design and features stay in the web app.

Requires macOS 13 or newer and a working
[Quota Glance](../quota-glance/README.md) page. Supports Apple silicon and Intel.

## Install from a DMG

Open `Quota-Glance-0.2.0-macOS.dmg`, drag **Quota Glance** to **Applications**,
then eject the disk image and open the installed app. Right-click its menu bar
icon, choose **Settings**, and enable **Open at login** to start it whenever you
sign in, including after a restart. Your saved URL and session persist.

Tagged app releases publish the DMG and ZIP on the repository's
[GitHub Releases page](https://github.com/NoorChasib/cpa-plugins/releases).
Look for **Quota Glance for Mac**. The **Quota Glance Menu Bar** workflow also
keeps downloads as build artifacts. Tagged releases require Developer ID
signing and Apple notarization; see the one-time
[Apple account setup](docs/apple-signing.md) before publishing the first release.

## Build on your Mac

On your Mac, install Xcode Command Line Tools if needed (`xcode-select --install`),
then run these commands from this repository:

```sh
cd plugins/quota-glance-menubar
make install
```

This builds a universal app, installs it at `~/Applications/Quota Glance.app`,
and opens it. No third-party Swift packages are required.

To create the drag-to-Applications disk image instead:

```sh
make dmg
```

The image is written to `dist/Quota-Glance-0.2.0-macOS.dmg`.
Local builds are ad-hoc signed; notarized release builds run through GitHub
after the Apple signing secrets are configured.

On first launch, paste your complete dashboard URL, for example:

```text
https://your-server/v0/resource/plugins/quota-glance/app
```

Choose **Save and Open**, then sign in using the page's dashboard password
(`web-token`) or its CPA console link. The app remembers its own session;
Safari and Chrome sessions are separate. Enter the password on the page instead
of adding `?token=` to the saved URL.

Click the menu bar chart icon to open or close the page. Scroll inside the
popover to see more. Right-click or Control-click the icon for **Settings**,
**Show Dashboard**, **Reload Page**, **Open in Browser**, and **Quit**.
In **Settings → Menu bar quota**, choose a window such as **Claude · Weekly
(Fable)** to show its remaining percentage beside the icon. It updates every
minute while your Mac is awake, including when the popover is closed. Choose
**Icon only** to hide it. Enable **Open at login** if desired.

See [REFERENCE.md](REFERENCE.md) for refresh behavior, builds, stored data,
and Mac verification steps.
