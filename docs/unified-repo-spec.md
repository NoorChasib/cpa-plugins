# CPA plugin repository structure

`NoorChasib/cpa-plugins` is the source, issue tracker, and release location for every plugin.

```text
.github/workflows/       # Active CI and independent release automation
plugins/
  account-health-pushover/
  auto-baseline/
  quota-cache/
  reset-priority/
  token-usage/
scripts/                 # Catalog validation, packaging, publication, integration checks
docs/                    # Shared installation, update, and architecture guidance
registry.json            # CPA Plugin Store catalog
preview/registry.json    # Identical compatibility alias
README.md                # Plugin overview and quick start
```

Each plugin owns its Go module, Makefile, tests, README, detailed reference, and runtime data defaults. Plugin IDs remain stable. Build and test from that plugin's directory.

## Independent installation and releases

Users add one catalog source and install only the plugins they want. Linux amd64 is the current publication target. Each plugin has its own version and `<plugin>/vX.Y.Z` release tag. The root release workflow tests and publishes one plugin, then updates only its entry in both catalog aliases. See [releases](releases.md).

Plugin-local workflow files are nonexecuting contract fixtures for packaging checks. Root `.github/workflows/` files perform CI and publication.

## Optional shared quota polling

Quota Cache serializes provider requests and persists observations and cooldowns. Account Health and Reset Priority may opt in through `use-quota-cache`; unavailable or stale cache data causes them to wait. With the option disabled, they operate independently. Auto Baseline and Token Usage do not poll provider quota endpoints.

Plugin settings and persistent paths belong to the operator. Updates preserve them. See [settings and data preservation](migration.md).
