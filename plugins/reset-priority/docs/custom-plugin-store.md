# Install reset-priority from the Plugin Store

Add this source once in CPA's Plugin Store:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

Before you install it, put its configuration, with `dry-run: true`, into CPA's `config.yaml` as [the quick start](../README.md#install) shows. CPA can load the plugin as soon as it is installed, and its first start saves credential priorities unless `dry-run` is already on. Then select **reset-priority**, install it, and follow any restart prompt. If you already use the identical `preview/registry.json` alias, keep that source URL so CPA's installed source identity stays the same.

The catalog contains the version and verified download for each plugin independently. When a release is published, its catalog entry advances and CPA can offer that plugin's update.

If `route-api-credits` is on, turn it off before you update, disable or uninstall the plugin, even for an update that is only about OAuth priorities: set `route-api-credits: false`, wait until two consecutive requests leave the status's `router.counters.picks` unchanged, then proceed. CPA calls the scheduler on every request, and a request that reaches the plugin while CPA replaces or removes it fails. Turn routing back on after the update. See [Updating or removing Reset Priority](../REFERENCE.md#updating-or-removing-reset-priority). Generated `store.install.artifacts` in CPA config records the installed package; CPA replaces it on update. Do not edit the URL or checksum by hand.

The plugin-local `registry.json` is a format-validation fixture. The combined root catalog is the supported source. See [releases](../../../docs/releases.md) and [settings/data preservation](../../../docs/migration.md).
