# Account Health Pushover

Get a Pushover message when a Claude, Codex, or Grok credential needs reauthentication or stays unhealthy. Ordinary usage limits do not trigger credential-health alerts.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to the existing `plugins.store-sources` list, then install the plugin from CPA's Plugin Store.
3. Merge the configuration below into your existing configuration, enable the plugin, and follow CPA's restart prompt. Do not create a second `plugins:` mapping.

This source lists all five plugins from this repository, including the optional quota-cache preview. If this plugin is already installed from an old source, follow the [migration guide](../../docs/migration.md) before switching; CPA v7.2.155 will otherwise reject the source change.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
  configs:
    account-health-pushover:
      enabled: true
      pushover-app-token-env: CPA_PUSHOVER_APP_TOKEN
      pushover-user-key-env: CPA_PUSHOVER_USER_KEY
      quota-alerts: false
```

## First use and data

Before starting CPA, provide `CPA_PUSHOVER_APP_TOKEN` and `CPA_PUSHOVER_USER_KEY` as secret environment variables to its container/process. Create the application token in your Pushover account; see [Pushover setup](docs/pushover-setup.md).

Open **Account Health** in the CPA sidebar to inspect discovered accounts. Use the authenticated status page's **Test notification** action to verify delivery. Allow the initial health scan to complete.

Weekly quota notifications are optional. Enable `quota-alerts` for notifications; choose standalone provider polling or opt into Quota Cache below. Credential-health monitoring continues when quota alerts are off.

Preserve any existing `state-file` override. By default, incident and notification state lives under the detected auth directory at `.plugin-state/account-health-pushover/state.ahp`, so persist and back up the auth volume too.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
- [All configuration options](config.example.yaml)

## Optional shared quota cache

Version 0.4.3 works independently by default. Quota Cache is **not required**.

To share quota observations, install Quota Cache and enable **use-quota-cache** in this plugin's CPA config panel:

```yaml
use-quota-cache: true
# Optional if Quota Cache uses its default path:
# quota-cache-path: plugins/data/quota-cache/snapshot.json
```

With the toggle on, missing, stale, or failed observations wait for fresh cache data; there is no direct-provider fallback. Set `use-quota-cache: false` to restore standalone polling. A path-only configuration from preview 1 still opts in until you explicitly set the toggle false.

Auto Baseline and Token Usage do not use Quota Cache. See [shared quota setup](../../docs/quota-cache-preview.md).
