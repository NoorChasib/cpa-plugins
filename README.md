# CPA Plugins

Native plugins for CLIProxyAPI (CPA), developed together and installed separately.

| Plugin | What you get |
| --- | --- |
| [Account Health Pushover](plugins/account-health-pushover/README.md) | Notifications when credentials need attention; optional weekly quota alerts |
| [Auto Baseline](plugins/auto-baseline/README.md) | Automatically updated Claude Code and Codex CLI fingerprint baselines |
| [Reset Priority](plugins/reset-priority/README.md) | Account priority ordered by the next weekly quota reset |
| [Quota Cache](plugins/quota-cache/README.md) | One scheduled quota poller with cached observations for other plugins (opt-in preview) |
| [Token Usage](plugins/token-usage/README.md) | Persistent token statistics in a CPA sidebar page |

## Get started

Open the README for the plugin you want. Each has a short installation/configuration guide. Install only the plugins you need.

Already using these plugins? Start with [the migration guide](docs/migration.md). Keep your existing settings and data paths; a repository move does not require a fresh installation.

Add this one source in CPA's Plugin Store:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

All five catalog entries, download assets, and current native repository links use **NoorChasib/cpa-plugins**. Current packages target Linux amd64/glibc and include the optional [quota-cache preview](docs/quota-cache-preview.md). `preview/registry.json` is a compatibility alias with the same entries; if you already added that URL, keep it to avoid changing CPA's installed source identity.

Each plugin works independently. Account Health and Reset Priority use Quota Cache only when you opt in with `use-quota-cache: true`; unavailable cache data waits without direct-provider fallback. Auto Baseline and Token Usage do not consume provider quotas.

Historical releases and their original bytes are preserved under `legacy/<plugin>/<tag>` in this repository.

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

## Shared quota polling

[Quota Cache](plugins/quota-cache/README.md) and cache-only consumer support are available in an [installable preview](docs/quota-cache-preview.md). See the quota-cache README for configuration and the verification record for tested scope.

CPA v7.2.155 has no native quota-provider capability. Its stock dashboard can still issue independent quota requests; installing this cache does not redirect those requests. Auto Baseline and Token Usage do not poll provider quota endpoints.
