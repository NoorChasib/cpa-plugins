# CPA Plugins

Native plugins for CLIProxyAPI (CPA), developed together and installed separately.

| Plugin | What you get |
| --- | --- |
| [Account Health Pushover](plugins/account-health-pushover/README.md) | Notifications when credentials need attention; optional weekly quota alerts |
| [Auto Baseline](plugins/auto-baseline/README.md) | Automatically updated Claude Code and Codex CLI fingerprint baselines |
| [Reset Priority](plugins/reset-priority/README.md) | Account priority ordered by the next weekly quota reset |
| [Quota Cache](plugins/quota-cache/README.md) | One scheduled quota poller with cached observations for other plugins (opt-in preview) |
| [Quota Glance](plugins/quota-glance/README.md) | One page showing remaining capacity across every credential and window, and which credential is taking the requests |
| [Token Usage](plugins/token-usage/README.md) | Persistent token statistics in a CPA sidebar page |

For macOS, [Quota Glance Menu Bar](plugins/quota-glance-menubar/README.md) embeds
your hosted dashboard in a compact menu bar popover. It installs on your Mac
separately from the CPA plugins.

## Get started

Open the README for the plugin you want. Each has a short installation/configuration guide. Install only the plugins you need.

For updates and reinstalls, see [settings and data preservation](docs/migration.md).

Add this one source in CPA's Plugin Store:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

All six catalog entries, download assets, and current native repository links use **NoorChasib/cpa-plugins**. Current packages target Linux amd64/glibc and include the optional [quota-cache preview](docs/quota-cache-preview.md). `preview/registry.json` is a compatibility alias with the same entries; if you already added that URL, keep it to avoid changing CPA's installed source identity.

Each plugin works independently. Account Health and Reset Priority use Quota Cache only when you opt in with `use-quota-cache: true`; unavailable cache data waits without direct-provider fallback. Auto Baseline and Token Usage do not consume provider quotas. Quota Glance reads the same cache and makes no provider requests of its own; the routing activity it shows comes from CPA's own request counter, in-process.


Publish updates independently with a tag such as `quota-cache/v0.1.1`. The root workflow verifies that plugin, publishes its Linux amd64 package, and updates only its catalog entry in both source aliases. CPA then offers that plugin's update; installation/restarts remain under your control. See [release commands and recovery](docs/releases.md).

## Development

Each directory retains its own Go module, tests, and Makefile:

```sh
cd plugins/account-health-pushover
make ci
```

See each plugin's reference documentation for its exact build and verification commands. Detailed existing documentation is retained in `REFERENCE.md` and `docs/`. Root GitHub workflows run each plugin's existing CI checks from its new directory.


## Compatibility

Account Health, Auto Baseline, Reset Priority, Quota Cache, and Quota Glance currently declare ABI 1 / RPC schema 4. Token Usage declares ABI 1 / RPC schema 6 and documents Linux amd64 runtime validation. See each plugin's reference for exact CPA versions, platform requirements, evidence, and limitations. Consult each plugin’s verification record for tested behavior.

## Shared quota polling

[Quota Cache](plugins/quota-cache/README.md) and cache-only consumer support are available in an [installable preview](docs/quota-cache-preview.md). See the quota-cache README for configuration and the verification record for tested scope.

CPA v7.2.155 has no native quota-provider capability. Its stock dashboard can still issue independent quota requests; installing this cache does not redirect those requests. Auto Baseline and Token Usage do not poll provider quota endpoints.
