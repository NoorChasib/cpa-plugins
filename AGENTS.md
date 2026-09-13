# CPA Plugins

Each plugin is an independent Go module under `plugins/<id>`. Run its existing Makefile targets from that directory. Keep plugin IDs, configuration keys, routes, and persistent-data defaults compatible during consolidation.

Root `.github/workflows/*-ci.yml` files are the active CI workflows. Plugin-local `.github/workflows/` files are historical release/CI references retained for their existing contract checks; GitHub does not execute them here. When adjusting CI gates, keep reference checks meaningful and verify active root workflows as well.

Keep READMEs focused on installation and first use. Put detailed behavior in `REFERENCE.md` and plugin-specific `docs/`. Do not remove existing test gates or change provider behavior as incidental cleanup.
