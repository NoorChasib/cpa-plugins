# Apple signing and notarization sources

Verified against official Apple and GitHub documentation on 2026-09-20.
This note records the release design and its sources; see the release setup
instructions for the repository's exact secret names and commands.

## Developer account setup

- Use a **Developer ID Application** signing identity for this app and its DMG.
  Developer ID Installer is for installer packages, and Apple Development,
  Mac Distribution, and ad hoc identities are not valid for this notarization
  workflow. Creating a Developer ID certificate requires the Account Holder
  role. [Apple: Developer ID certificates][certificates]
  [Apple: notarization requirements][notarization]
- On a Mac, open **Xcode → Settings → Accounts**, select the Apple Account
  and team, then **Manage Certificates → + → Developer ID Application**.
  Control-click the resulting certificate and choose **Export Certificate**
  to save a password-protected PKCS#12 (`.p12`) signing identity. The export
  must include the private key: a downloaded `.cer` file alone cannot sign
  software. [Apple: synchronizing code signing identities][identities]
- Get the Developer Team ID from the [Apple Developer account page][account].
  Apple recommends passing it explicitly even when the account belongs to
  only one team. [Apple TN3147][notarytool]
- For this small release workflow, use an Apple Account email, Team ID, and
  an app-specific password for notarization. Generate the password at
  **account.apple.com → Sign-In and Security → App-Specific Passwords**.
  Two-factor authentication must be enabled. Changing the main Apple Account
  password revokes existing app-specific passwords.
  [Apple: app-specific passwords][passwords]
  [Apple: custom notarization workflow][customization]
- An App Store Connect API key is an alternative, not an additional
  requirement. The documented team-key form passes the `.p8` file with
  `--key`, its key ID with `--key-id`, and issuer UUID with `--issuer`.
  The Apple Account route avoids introducing that separate API-key setup.
  [Apple TN3147][notarytool]

## Release sequence

1. Build the complete app bundle, then sign it using Developer ID Application
   with Hardened Runtime and a secure timestamp. Do not include
   `com.apple.security.get-task-allow=true`.
   [Apple: notarization requirements][notarization]
2. Create a ZIP containing the app with `ditto -c -k --keepParent`, submit
   with `xcrun notarytool submit ... --wait`, require an `Accepted` result,
   and retrieve the notary log, including for successful submissions.
   [Apple: custom notarization workflow][customization]
3. Staple and validate the ticket on the `.app`, then recreate the final ZIP
   from that stapled app. ZIP archives can be submitted for notarization but
   cannot themselves have tickets stapled to them.
   [Apple: custom notarization workflow][customization]
4. Build the DMG from the stapled app, sign the DMG with Developer ID
   Application, submit it for notarization, then staple and validate it.
   Apple supports notarizing disk images and generates tickets for nested
   signed software too. This two-submission sequence is a project choice
   that prepares both a ZIP and a DMG for distribution; it is not a universal
   requirement for every DMG-only app.
   [Apple: custom notarization workflow][customization]
   [Apple TN2206: signing disk images][disk-images]
5. Verify code signatures and Gatekeeper acceptance before publishing.
   Apple's documented DMG assessment is
   `spctl -a -t open --context context:primary-signature -v MyImage.dmg`.
   Use an execution assessment for the app. Stapling allows Gatekeeper to
   find the ticket without an internet connection.
   [Apple TN2206][disk-images]
   [Apple: custom notarization workflow][customization]

Hardened Runtime exceptions should be added only when functionality requires
them. The current wrapper uses the system `WKWebView` and does not implement
its own JIT or load third-party native plug-ins. There is no demonstrated
reason to add executable-memory or library-validation exceptions; verify
the signed app's web rendering on macOS as part of release testing.
[Apple: Hardened Runtime][runtime]
[Apple: WKWebView][webview]

Notarization is an automated security check, not App Store review. It supports
direct distribution through GitHub Releases. A notarized app can still show
the normal first-launch confirmation dialog.
[Apple: notarizing macOS software][notarization]

## GitHub Actions credentials

Store the password-protected P12 as a Base64-encoded GitHub Actions secret,
and store its export password and notarization credentials as separate
secrets. Base64 is transport encoding, not encryption.
GitHub documents `base64 -i BUILD_CERTIFICATE.p12 | pbcopy` for preparing the
certificate secret. [GitHub: certificates on macOS runners][github]

GitHub's reference workflow creates a temporary keychain, unlocks it,
imports the P12, runs `security set-key-partition-list -S apple-tool:,apple:`,
and updates the keychain search list. Preserve the existing search list
when adding the temporary keychain and restore it in cleanup; this avoids
discarding existing keychains during signing. Explicit `codesign --keychain`
selection alone was not verified as sufficient for all certificate-chain
lookup behavior. GitHub-hosted runners are destroyed after the job;
explicit cleanup additionally removes imported signing material on failure.
[GitHub: certificates on macOS runners][github]
[Apple TN3137: file-based keychain search lists][keychains]

[account]: https://developer.apple.com/account
[certificates]: https://developer.apple.com/help/account/certificates/create-developer-id-certificates/
[identities]: https://developer.apple.com/documentation/xcode/sharing-your-teams-signing-certificates
[passwords]: https://support.apple.com/en-us/102654
[notarization]: https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution
[customization]: https://developer.apple.com/documentation/security/customizing-the-notarization-workflow
[notarytool]: https://developer.apple.com/documentation/technotes/tn3147-migrating-to-the-latest-notarization-tool
[runtime]: https://developer.apple.com/documentation/security/hardened-runtime
[webview]: https://developer.apple.com/documentation/webkit/wkwebview
[disk-images]: https://developer.apple.com/library/archive/technotes/tn2206/_index.html
[github]: https://docs.github.com/en/actions/how-tos/deploy/deploy-to-third-party-platforms/sign-xcode-applications
[keychains]: https://developer.apple.com/documentation/technotes/tn3137-on-mac-keychains
