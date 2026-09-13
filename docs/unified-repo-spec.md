# CPA Plugins: repository consolidation

Status: draft. This proposes the initial repository move; no repositories have been migrated or published.

## Goal

Create one repository, `cpa-plugins`, with all four existing CPA plugins under `plugins/` and a top-level README explaining the collection. Make it possible to find and work on every plugin from one checkout.

This first step is organizational. Preserve each plugin's code, Go module, dependencies, configuration, tests, build commands, and documentation. Shared libraries, deduplication, a common framework, and unified versioning are future decisions.

## Repository layout

```text
cpa-plugins/
├── README.md
├── AGENTS.md
├── .gitignore
├── .github/
│   └── workflows/                  # Existing workflows adapted to their new paths
├── docs/
│   └── migration.md                # Source repositories, import commits, release locations
└── plugins/
    ├── account-health-pushover/
    │   ├── README.md
    │   ├── LICENSE
    │   ├── go.mod
    │   ├── go.sum
    │   ├── Makefile
    │   ├── registry.json
    │   ├── config.example.yaml
    │   ├── main.go
    │   ├── abi.go
    │   ├── internal/
    │   ├── docs/
    │   └── scripts/
    ├── auto-baseline/              # Existing repository contents
    ├── reset-priority/             # Existing repository contents
    └── token-usage/                # Existing repository contents
```

The expanded plugin directory is illustrative; preserve each plugin's actual files rather than forcing identical contents. Keep tests beside their existing implementation and retain all license notices.

Each directory stays self-contained. Existing module paths can remain during this move because Go module identity does not have to match the checkout directory. Builds and tests run from the individual plugin directory. There is no root Go module or required Go workspace in this phase.

For example, existing commands remain usable from the root with `make -C plugins/account-health-pushover ci` or by changing into the plugin directory. Do not standardize differing Makefile targets as part of the move.

## Top-level README

The README should introduce the collection, show this catalog, explain where to start, and link to each plugin's existing documentation:

| Plugin | What it does | Changes CPA/account settings? |
| --- | --- | --- |
| [Account Health Pushover](../plugins/account-health-pushover/README.md) | Sends credential-health notifications and optional weekly quota alerts | No; sends notifications and saves its own state |
| [Auto Baseline](../plugins/auto-baseline/README.md) | Learns client fingerprints and updates CPA's configured baselines | Yes; updates selected CPA configuration keys |
| [Reset Priority](../plugins/reset-priority/README.md) | Prioritizes credentials by weekly quota reset time | Yes; updates credential priorities |
| [Token Usage](../plugins/token-usage/README.md) | Persists CPA-reported token usage and displays a sidebar page | No; writes its own SQLite database |

These links illustrate the future catalog from this draft's location; in the new root README, use `plugins/<id>/README.md`.

After the catalog, include:

- **Installation:** each plugin is installed and enabled separately; link to its instructions and current release location.
- **Compatibility and known issues:** a short per-plugin summary linking to its existing compatibility/verification documents and relevant issues. Distinguish documented validation from untested combinations; the repository move itself does not establish that a plugin works.
- **Development:** work inside the chosen `plugins/<id>` directory and use its existing commands.
- **Adding a plugin:** create another directory under `plugins/` and add a catalog entry.

Keep detailed configuration, audits, and troubleshooting in the plugin READMEs/docs. The root README is the entry point for understanding and navigating the collection.

## GitHub workflows and existing releases

GitHub discovers workflows only at the repository root. Move each plugin's workflows into root `.github/workflows/` with distinct filenames, such as `account-health-ci.yml` and `token-usage-ci.yml`. Adapt working directories, path filters, artifact paths, script references, and concurrency names. Preserve existing checks.

Retain separate plugin versions for now. Existing hosted releases and registry URLs can continue serving installations from their original repositories while source development moves. Link to those locations explicitly so users know where installation artifacts live.

Do not turn on imported release workflows unchanged: existing generic `v*` triggers and repository guards assume separate repositories. Keep old release workflow definitions as reference under each plugin's docs during the import, outside root `.github/workflows/`. Choose and verify the next publication workflow before the next release; that decision is not required to consolidate source.

A combined store source is also optional later. Simply pointing all four registry entries to the new repository would change release resolution: the inspected local CPA checkout uses the repository's latest GitHub release for `github-release` installation. Leave working distribution metadata intact in this phase.

## Migration steps

1. Inventory the four source repositories, including branches, uncommitted work, licenses, and current release locations. Record the commits selected for import.
2. Create `cpa-plugins` and import each repository's tracked contents under its plugin directory, retaining Git history through a history-preserving subtree import. Do not copy nested `.git` directories, local secrets, databases, or build outputs. Carry uncommitted work over deliberately rather than silently dropping it.
3. Add the root README, repository instructions, and migration record. Preserve each plugin's README, docs, and license notices. Adjust navigation links where the move requires it; keep historical release references intact.
4. Adapt existing CI workflows to the root layout. Preserve plugin-local script execution contexts. Move release workflow definitions to reference documentation until their publication behavior is deliberately adapted.
5. Run the existing build/test checks from each plugin directory and validate root workflow paths. Record pre-existing failures or unavailable integration prerequisites separately from move-induced failures.
6. Confirm that one checkout contains all four plugins and the README explains how to navigate, build, and install each one.

Publishing the new repository, changing store sources, or archiving old repositories can follow once the imported repository is ready. They do not need to be bundled with the local consolidation.

## Done means

- All four plugins live in one repository, each in its own directory.
- The top-level README clearly explains what each plugin does and links to its docs.
- Existing plugin code, configuration, data locations, and independent installation behavior are preserved.
- Existing development commands work from each plugin directory, and CI uses the new paths.
- Git history and source provenance are retained.
- Existing release locations remain documented and usable; publication workflows cannot accidentally run with obsolete assumptions.

No shared-code extraction or dependency/version unification is required to complete this phase.
