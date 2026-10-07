# Auto Baseline 0.1.6

Auto Baseline now understands the `upstream` section that CPA v8.0.11 and later write, so Claude promotions take effect again after a Management Center save. Plugin ID, routes, settings, and the state and backup paths are unchanged.

**Upgrading from 0.1.5 on CPA v8.0.11+:** update, check that Claude's status sources read `upstream`, then press **Clear pending** once to lift the pause 0.1.5 left. Press it only after updating: under 0.1.5 it just repeats the failure. See [the upgrade steps](install-docker-compose.md#upgrading-from-015-to-016-cpa-v8011).

## Why

CPA v8.0.11 made `upstream.claude.header-defaults` the canonical Claude block and `upstream.codex.disable-codex-cloaking` the canonical Codex cloaking switch; `oauth.providers.claude.header-defaults` became a historical alias. The first `/v8/management` save after such an upgrade (any panel save, including a plugin install or uninstall) moves the Claude block under `upstream`. 0.1.5 knew only the alias and legacy paths. It reported CPA's compiled default, wrote its promotion to the alias path, and CPA ignored that write and deleted it on the reload it triggered (a second `config.yaml` write about 150 ms later). The plugin then paused Claude with `promotion_not_effective`, and **Clear pending** only repeated it. CPA kept serving the last value under `upstream` throughout.

## Changes

- **Three layers, leaf by leaf.** Each Claude baseline value and `disable-codex-cloaking` resolve the way CPA v8.0.11+ does: `upstream`, then the `oauth.providers` name, then the legacy name, then CPA's compiled default. A present key wins even when it is `false`, blank, or null. Status reports the new source `upstream`.
- **Claude writes land in the canonical block.** When the file has an `upstream` section, which only CPA v8.0.11+ writes, the Claude promotion goes to `upstream.claude.header-defaults`. The write also removes the keys it supersedes from the alias and legacy blocks, plus any mapping that leaves empty, so CPA's load has nothing to clean and never rewrites the file. Files without an `upstream` section are read and written exactly as by 0.1.5. Codex header defaults stay under `oauth.providers.codex`, where CPA still reads them.
- **The loop guard sees the shadowing.** Because the pre-write check now applies CPA's upstream precedence, a write that an `upstream` key would shadow is refused before it happens instead of being written and then paused.
- **Safer cleanup.** The plugin no longer removes a superseded key from a mapping that has a YAML merge key (which could expose a merged value, an issue 0.1.5 also had for the legacy block), and never removes an anchored leaf; CPA's own load cleans such shapes without changing the written values. A non-mapping `upstream` or `upstream.codex`, which makes CPA refuse the whole file, now marks the affected provider unsupported instead of being read as cloaking off.
- **Codex cloaking warning fixed.** The status page no longer warns about cloaking when `upstream.codex.disable-codex-cloaking: true` is set, and names that key when it is not.
- **Audited against CPA v8.0.13** (`d7914af`) for the `upstream` rules. CPA's compiled defaults are unchanged from v8.0.4 through v8.0.16, so `assumed_cpa_version` and the default floors stay as they were.

Install or update Auto Baseline from the [combined catalog](../../../README.md#get-started). This release targets Linux amd64.
