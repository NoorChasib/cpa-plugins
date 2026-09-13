# CPA Plugins

Native plugins for CLIProxyAPI (CPA), developed together and installed separately.

| Plugin | What you get |
| --- | --- |
| [Account Health Pushover](plugins/account-health-pushover/README.md) | Notifications when credentials need attention; optional weekly quota alerts |
| [Auto Baseline](plugins/auto-baseline/README.md) | Automatically updated Claude Code and Codex CLI fingerprint baselines |
| [Reset Priority](plugins/reset-priority/README.md) | Account priority ordered by the next weekly quota reset |
| [Token Usage](plugins/token-usage/README.md) | Persistent token statistics in a CPA sidebar page |

## Get started

Open the README for the plugin you want. Each has a short installation/configuration guide. Install only the plugins you need.

Already using these plugins? Start with [the migration guide](docs/migration.md). Keep your existing settings and data paths; a repository move does not require a fresh installation.

The root `registry.json` combines the four existing store entries and continues to resolve their existing published releases. It can become one store source once this repository is published. There are no newly published binaries from this repository yet.

## Development

Each directory retains its own Go module, tests, and Makefile:

```sh
cd plugins/account-health-pushover
make ci
```

See each plugin's reference documentation for its exact build and verification commands. Detailed existing documentation is retained in `REFERENCE.md` and `docs/`. Root GitHub workflows run each plugin's existing CI checks from its new directory.

Source histories were imported from the existing repositories. Historical tags are namespaced as `legacy/<plugin>/<tag>`. See [source provenance](docs/migration.md#source-provenance).

## Compatibility

Account Health, Auto Baseline, and Reset Priority currently declare ABI 1 / RPC schema 4. Token Usage declares ABI 1 / RPC schema 6 and documents Linux amd64 runtime validation. See each plugin's reference for exact CPA versions, platform requirements, evidence, and limitations. A successful source import is not a new compatibility certification.
