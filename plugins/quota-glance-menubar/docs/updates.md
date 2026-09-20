# Update publishing and signing

The app uses [Sparkle 2.10.0](https://github.com/sparkle-project/Sparkle/releases/tag/2.10.0),
pinned in Package.swift and Package.resolved. Its binary archive is checksum
verified by SwiftPM. The framework and helper processes are embedded in the app,
re-signed inside out with the same Developer ID identity, and notarized with it.

## Signing key

The initial key is already configured in `SPARKLE_ED25519_PRIVATE_KEY` on
`NoorChasib/cpa-plugins`. Only its public key is in Info.plist. The one-time
backup on the development host is
`~/.local/share/quota-glance-menubar/sparkle-ed25519.key` (mode 0600, outside Git).
Back it up securely. Do not regenerate it for ordinary releases.

Sparkle's current private-key export format is a base64-encoded 32-byte Ed25519
seed. The initial seed was generated with OpenSSL's Ed25519 generator, which is
compatible with this documented format. CI derives its public key using Apple
CryptoKit and requires it to match the public key sealed into the app.

To create a key for a new independent deployment on a Mac, use Sparkle's
`bin/generate_keys`, then `bin/generate_keys -x /safe/location/private-key`.
Put the printed public key in `SUPublicEDKey`. Upload the exported key without
printing it or putting its value in a command argument:

```sh
gh secret set SPARKLE_ED25519_PRIVATE_KEY --repo OWNER/REPO < /safe/location/private-key
```

For this app, changing the key requires following Sparkle's key rotation rules.
Because verification happens before extraction, a lost Ed25519 key can only
be rotated through an update DMG signed with the existing Apple Developer ID
identity. Do not change both identities at once. Keep the archive format as a
signed DMG when doing such a rotation.

## Feed and publication order

The public `quota-glance-updates` branch hosts `appcast.xml`; the app's fixed URL
uses raw.githubusercontent.com. The branch is independent of the source branch
and only contains update metadata. The workflow's `contents: write` permission
must permit updates to it. It should not require a pull request for every feed
change; the app still requires cryptographic signatures on the feed and DMG.

A trusted release tag builds and tests the app, signs and notarizes the app and
DMG, and uses Sparkle's `generate_appcast` on that final DMG. The signing secret
is written to a temporary private file and removed on exit. The generator signs
the archive and feed; `sign_update --verify` verifies the feed. An actual
SPUUpdater probes a local copy of the signed feed using the packaged app's key
and confirms an altered feed is rejected. No updater installation is attempted
by this probe.

The publish job first creates the public release with its DMG, ZIP, signed feed,
and checksums, then publishes the feed unchanged. It checks release visibility,
version, asset URL and size before writing. Feed updates use the current GitHub
file SHA and retry conflicts; late releases cannot roll the feed backward. Do
not edit the signed XML manually. A main-branch manual run with `notarize=true`
exercises signing and feed verification without publishing.

If feed publication fails after a successful release, download that release's
assets, check `SHA256SUMS`, and run `scripts/publish-appcast.py --validate
appcast.xml --version X.Y.Z --assets /path/to/downloads --publish`. There is no
need to rebuild or replace published assets. A release already published with
the wrong content needs a new version, not an overwritten download.

## Verification limits

CI exercises compilation on Apple silicon and Intel, nested code signatures,
notarization, feed/key validation and tamper rejection. It cannot establish a
user's install permissions, dialogs, relaunch behavior, or login-item behavior.
For an interactive end-to-end check, install v0.3.0 in Applications, then use
Check for Updates when the next release is published. Confirm the install and
relaunch preserve the URL, session, selected quota, and Open at login setting.

## Official sources checked on 2026-09-20

- [Sparkle setup, signatures, distribution and feeds](https://sparkle-project.org/documentation/)
- [Programmatic updater and API expectations](https://sparkle-project.org/documentation/programmatic-setup/)
- [Updater preferences and signed feeds](https://sparkle-project.org/documentation/customization/)
- [Manual signing of nested helpers](https://sparkle-project.org/documentation/sandboxing/#code-signing)
- [Exported seed format](https://github.com/sparkle-project/Sparkle/blob/2.10.0/generate_keys/main.swift)
