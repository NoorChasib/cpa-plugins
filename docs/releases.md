# Release or update one plugin

Every plugin keeps its own version. Releases currently target **Linux amd64 only**.

## Publish a change

1. Change the plugin and bump its native version. Account Health, Quota Cache, and Token Usage have `Version` in `internal/plugin/plugin.go` and a matching Makefile `VERSION`. Auto Baseline and Reset Priority use `PluginVersion` in `internal/plugin/runtime.go`; their Makefiles derive it automatically.
2. Update any plugin-specific version fixtures and run its existing checks. Token Usage deliberately binds the candidate version in its package helper, local registry, native/browser fixtures, browser package metadata, and historical workflow references. Its `scripts/check-candidate.py` and existing tests must all agree. Do not edit the root catalogs yet: they describe published downloads.
3. Commit and push the change to `main`. Then tag that exact commit with the plugin ID and numeric version:

   ```sh
   git tag quota-cache/v0.1.1
   git push origin quota-cache/v0.1.1
   ```

Use a version higher than that plugin's catalog version. Other IDs are `account-health-pushover`, `auto-baseline`, `reset-priority`, and `token-usage`. Generic `v*`, historical `legacy/*`, and prerelease version strings do not trigger supported publications. Ordinary pushes to `main` run CI; they do not publish a release.

The root **Release one plugin** workflow automatically:

- Runs the selected plugin's tests, race/static checks, and existing native/package acceptance. Token Usage retains its browser, SQLite/native, and official-image store checks.
- Loads the candidate with the four currently published peers in pinned CPA v7.2.155. It checks coexistence, status access, persistence/restart, and optional Quota Cache operation. Peers are downloaded with catalog checksum verification, not rebuilt or released.
- Packages the exact tested library, license, and Token Usage third-party notices, with checksums and verification evidence.
- Uploads to a draft GitHub release, reads every asset back, verifies its bytes, and publishes the release. Published bytes are never overwritten.
- Verifies the anonymous ZIP download, then commits only that plugin's entry in both `registry.json` and `preview/registry.json`. Concurrent releases reapply against current `main` with fast-forward retries, preserving other entries.

No version is inferred from the repository-wide GitHub “latest release.” The catalog pins each plugin's own release URL and checksum.

## Verify the workflow without publishing

```sh
gh workflow run release-plugin.yml --ref main -f plugin=quota-cache
```

Manual dispatch verifies and packages the selected current source version. Its downloadable Actions artifact is a rehearsal; it does not publish a GitHub release or change either catalog. It can validate a proposed version before tagging it once the commit is on `main`.

## Install the update in CPA

Keep your existing source URL, including `preview/registry.json` if that is how you installed. Both aliases advance together. Refresh/reopen the Plugin Store and select **Update** for the plugin you want. CPA compares its installed version with that catalog entry; other plugins keep their installed versions and settings.

Publishing does **not** automatically replace a running CPA library, restart Coolify, or change your configuration. If CPA reports `plugin_update_requires_restart`, disable the affected plugin and follow CPA's restart/update instructions. Installing native updates remains an operator action.

## Failed or simultaneous releases

The publisher needs GitHub Actions `contents: write` and permission to push catalog commits to `main`. If branch rules block the push, the workflow fails visibly; it does not bypass those rules. Allow the intended bot update through your repository rules before rerunning the failed job.

Rerun the failed **publish** job to reuse the same verified Actions artifact. A partial draft upload resumes with missing files only; existing assets must match byte for byte. If assets were published but the catalog commit failed, rerunning verifies them and retries the catalog update. A catalog already containing the exact entry is a successful no-op. A newer entry or conflicting same-version entry is rejected: make a new version instead of moving a tag or replacing assets.

The old `scripts/package-quota-preview.py` remains historical tooling for the fixed suite preview. Do not use it for new releases: it rewrites all five catalog entries.
