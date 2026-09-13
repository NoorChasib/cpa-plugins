# Install reset-priority from the Plugin Store

Add this source once in CPA's Plugin Store:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
```

Select **reset-priority** and install it. Configure the options in [the quick start](../README.md), then follow any restart prompt. If you already use the identical `preview/registry.json` alias, keep that source URL so CPA's installed source identity stays the same.

The catalog contains the version and verified download for each plugin independently. When a release is published, its catalog entry advances and CPA can offer that plugin's update. Generated `store.install.artifacts` in CPA config records the installed package; CPA replaces it on update. Do not edit the URL or checksum by hand.

The plugin-local `registry.json` is a format-validation fixture. The combined root catalog is the supported source. See [releases](../../../docs/releases.md) and [settings/data preservation](../../../docs/migration.md).
