# Connect your Apple Developer account

Tagged releases use **Developer ID Application** signing and Apple notarization.
This requires an active Apple Developer Program membership. The app is
distributed through GitHub, so no App Store listing or review is needed.

## 1. Export your signing certificate on your Mac

1. Open **Xcode → Settings → Accounts**, sign in with your Apple Account, and
   select your developer team.
2. Open **Manage Certificates**. If you already have a usable **Developer ID
   Application** certificate on this Mac, use it. Otherwise click **+** and
   create one. This certificate type signs apps distributed outside the Mac
   App Store; Developer ID Installer is for installer packages and is not used.
3. Control-click the certificate and choose **Export Certificate**. Save the
   password-protected `.p12` somewhere outside the repository. The export must
   contain the certificate **and its private key**; a downloaded `.cer` alone
   is insufficient. Keep the export password for the next step.

Creating a Developer ID certificate requires the team's Account Holder. If
that is someone else, they need to create or provide the signing identity.
On an individual membership, that is normally you.

Apple's [Sharing your team's signing certificates](https://developer.apple.com/documentation/xcode/sharing-your-teams-signing-certificates)
describes creating and exporting certificates in Xcode.

## 2. Create credentials for notarization

- Find your **Team ID** in your membership details at
  [developer.apple.com/account](https://developer.apple.com/account/). It is a
  10-character identifier; it must match the certificate's team.
- At [account.apple.com](https://account.apple.com/), open **Sign-In and Security
  → App-Specific Passwords**, then generate one for `Quota Glance GitHub`.
  Apple requires two-factor authentication for this. Use this generated
  password, not your usual Apple Account password.
- Use the email address for that Apple Account as `APPLE_ID`.

This workflow uses Apple Account authentication, so an App Store Connect API
key is not required. See Apple's
[app-specific password instructions](https://support.apple.com/en-us/102654).

## 3. Add five GitHub Actions secrets

Open this repository's [Actions secrets settings](https://github.com/NoorChasib/cpa-plugins/settings/secrets/actions)
and add these **repository secrets**, preserving the names exactly:

| Secret | Value |
| --- | --- |
| `MACOS_CERTIFICATE_P12_BASE64` | The base64-encoded `.p12` export, including its private key |
| `MACOS_CERTIFICATE_PASSWORD` | The password you set when exporting the `.p12` |
| `APPLE_ID` | Your Apple Account email address |
| `APPLE_APP_SPECIFIC_PASSWORD` | The app-specific password generated above |
| `APPLE_TEAM_ID` | Your 10-character developer Team ID |

If you have GitHub CLI on your Mac, you can upload them without putting secret
values in commands or writing a base64 copy to disk. Adjust the first command's
file path to your certificate export:

```sh
base64 -i "$HOME/Desktop/DeveloperID.p12" | gh secret set MACOS_CERTIFICATE_P12_BASE64 --repo NoorChasib/cpa-plugins
gh secret set MACOS_CERTIFICATE_PASSWORD --repo NoorChasib/cpa-plugins
gh secret set APPLE_ID --repo NoorChasib/cpa-plugins
gh secret set APPLE_APP_SPECIFIC_PASSWORD --repo NoorChasib/cpa-plugins
gh secret set APPLE_TEAM_ID --repo NoorChasib/cpa-plugins
```

The final four commands prompt for their values. For browser setup instead,
`base64 -i "$HOME/Desktop/DeveloperID.p12" | pbcopy` copies the first secret's
value to your Mac's clipboard. Add it in GitHub's secret form. Do not paste
these values into chat or commit the certificate/private key to the repo.

The script selects the imported Developer ID Application identity automatically;
you do not need to configure a signing identity name or provision the app in
App Store Connect. No extra entitlements or provisioning profile are used by
this web view wrapper.

## 4. Verify once, then release

Once the app and workflow are committed and pushed to `main`, open **Actions →
Quota Glance Menu Bar → Run workflow**, select `main`, and enable **Sign and
notarize artifacts from main without publishing**. Or run:

```sh
gh workflow run quota-glance-menubar-ci.yml --repo NoorChasib/cpa-plugins --ref main -f notarize=true
```

This produces signed, notarized downloads as workflow artifacts without creating
a release. Test the downloaded DMG on your Mac. After a successful check, push
a fresh release tag against the same commit on `main`:

```sh
git tag quota-glance-menubar/v0.3.0 <verified-commit-on-main>
git push origin quota-glance-menubar/v0.3.0
```

GitHub then builds and publishes the DMG and ZIP on the repository's Releases
page. Tagged releases require the five Apple secrets, the separate
[Sparkle signing secret](updates.md), and successful notarization;
they never fall back to publishing an ad-hoc-signed app. Normal branch and PR
checks keep using ad-hoc signing and do not receive these credentials.

## What the workflow does

1. Run the existing tests, build both architectures, and verify the bundle.
2. Import the `.p12` into a temporary, password-protected keychain and verify
   there is one usable Developer ID Application identity for the configured team.
3. Sign the already-built app with Hardened Runtime and a secure timestamp.
4. Submit an app ZIP with `notarytool`, require Apple's `Accepted` result, and
   staple the notarization ticket to the app. Validate the ticket and assess it
   with Gatekeeper.
5. Recreate the downloadable ZIP from the stapled app. Create and sign the DMG,
   notarize it, staple its ticket, and check its signature and Gatekeeper status.
6. Generate and sign the Sparkle feed, verify its signing key and valid/altered
   feed behavior, then calculate checksums over the final downloads and feed.
7. Publish those bytes as a GitHub release, then advance the stable update feed.

The two submissions cover the separately downloadable app ZIP and DMG. ZIP files
cannot themselves be stapled, which is why the app inside is stapled before
the final ZIP is created. The ticket lets Gatekeeper validate notarization
without needing to retrieve it from Apple at installation time. macOS may still
show its normal first-open confirmation for an app downloaded from the internet.

The temporary keychain and exported certificate file are removed, and the
runner's keychain search list is restored. Apple credentials are provided only
to the signing step on trusted release tags or an explicitly requested manual
run from `main`. Release tags must point to a commit already on `main`.

## Troubleshooting and limits

- **Missing release secret:** add it with the exact name listed above, then
  rerun the failed workflow. Nothing is published from a failed build job.
- **No valid identity:** check that the export includes the private key, uses
  Developer ID Application, is valid, and belongs to `APPLE_TEAM_ID`.
- **Notary authentication failure:** check the Apple Account email, team
  membership, and app-specific password. Changing your main Apple password
  revokes existing app-specific passwords.
- **Apple rejects or delays the submission:** the workflow does not publish.
  Review the submission status/log in the failed step. It waits up to 30 minutes
  per submission; Apple's processing time can vary, especially for first
  submissions. A timeout does not mean Apple has rejected the app.
- **Downloaded app behavior:** native execution, popover interaction, login-item
  registration, and the first signed release still need verification on a Mac.

The [first signed workflow run](https://github.com/NoorChasib/cpa-plugins/actions/runs/35541336282)
passed on 2026-09-20 after the repository secrets were configured. Both the app
and DMG passed Developer ID signing, Apple notarization, stapled ticket
validation, and Gatekeeper assessment. Interactive behavior still needs a Mac
user session. Changing the hosted dashboard later does not require signing the
wrapper again; only new native app releases do.

Primary-source details are recorded in [apple-signing-sources.md](apple-signing-sources.md).
