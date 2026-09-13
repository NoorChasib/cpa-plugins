# Release auto-baseline

Releases are published independently from `NoorChasib/cpa-plugins`. The active workflow is the root `.github/workflows/release-plugin.yml`. See the [shared release guide](../../../docs/releases.md) for version selection, checks, publication, catalog updates, and recovery.

From the repository root, after committing the version bump and required changes to `main`:

```sh
git push origin main
git tag auto-baseline/v0.1.4
git push origin auto-baseline/v0.1.4
```

Use a fresh version for each release. The workflow builds and tests only this candidate with the other published plugins, publishes its Linux amd64 ZIP plus checksums and release metadata, and updates only its entry in both combined catalogs. A normal branch push runs CI without publishing.

CPA discovers available updates from the catalog. It does not select the repository-wide latest GitHub release, which could belong to a different plugin. Operators install the offered update through CPA and follow any restart prompt. Settings and persistent data paths remain unchanged.

## Local checks

Run the Makefile checks from `plugins/auto-baseline`. The authoritative full release checks live in root `scripts/verify-plugin-release.sh`, followed by `scripts/quota-cache-smoke.py --candidate auto-baseline`. The root workflow executes both before packaging the exact tested bytes.

Plugin-local workflow and registry files are nonexecuting contract fixtures for existing format/platform validation. They are not store sources or publication workflows. Keep their checks meaningful when changing packaging; install from the root catalog.
