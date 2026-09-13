# Account Health Pushover

Get a Pushover message when a Claude, Codex, or Grok credential needs reauthentication or stays unhealthy. Ordinary usage limits do not trigger credential-health alerts.

## Install

1. Ensure CPA supports this plugin's native ABI/platform and persist its plugin directory. In the standard Docker image, mount your existing plugins volume at `/CLIProxyAPI/plugins`.
2. Add the store source below to the existing `plugins.store-sources` list, then install the plugin from CPA's Plugin Store.
3. Merge the configuration below into your existing configuration, enable the plugin, and follow CPA's restart prompt. Do not create a second `plugins:` mapping.

The source still serves the existing published release during repository consolidation.

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json"
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

Weekly quota notifications are optional. Enabling `quota-alerts` currently adds provider quota polling; leave it off until the shared quota cache is available if reducing quota reads is your priority. Credential-health monitoring continues with it off.

Preserve any existing `state-file` override. By default, incident and notification state lives under the detected auth directory at `.plugin-state/account-health-pushover/state.ahp`, so persist and back up the auth volume too.

## More detail

- [Complete behavior and compatibility reference](REFERENCE.md)
- [Migration without losing settings or data](../../docs/migration.md)
- [All configuration options](config.example.yaml)
