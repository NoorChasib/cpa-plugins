#!/usr/bin/env python3
"""Require the current install catalogs to be self-contained in this repository."""
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
REPOSITORY = 'https://github.com/NoorChasib/cpa-plugins'
catalog = json.loads((ROOT / 'registry.json').read_text())
assert catalog == json.loads((ROOT / 'preview/registry.json').read_text()), 'catalog aliases diverged'
assert catalog['schema_version'] == 2
ids = [p['id'] for p in catalog['plugins']]
assert sorted(ids) == ['account-health-pushover', 'auto-baseline', 'quota-cache', 'reset-priority', 'token-usage']
for plugin in catalog['plugins']:
    assert plugin['repository'] == REPOSITORY, plugin['id'] + ': legacy repository'
    assert plugin['homepage'] == REPOSITORY + '/tree/main/plugins/' + plugin['id']
    assert plugin['install']['type'] == 'direct', plugin['id'] + ': unpinned release discovery'
    assert plugin['install']['artifacts']
    for artifact in plugin['install']['artifacts']:
        assert artifact['url'].startswith(REPOSITORY + '/releases/download/'), plugin['id'] + ': external download'
        assert re.fullmatch(r'[a-f0-9]{64}', artifact['sha256'])
        assert artifact['size'] > 0
        assert artifact['url'].endswith('/' + plugin['id'] + '_' + plugin['version'] + '_' + artifact['goos'] + '_' + artifact['goarch'] + '.zip')
print('PASS: both catalogs list all five plugins with pinned downloads exclusively from NoorChasib/cpa-plugins')

# Guard source identity in modules, imports, documentation, and packaging too.
# Construct the retired naming pattern so the check does not introduce a
# forbidden reference into the very source tree it audits.
pattern = re.compile(rb"cpa-" + rb"plugin-(?:account-health-pushover|auto-baseline|reset-priority|token-usage)")
for name in subprocess.check_output(['git', 'ls-files', '-z'], cwd=ROOT).split(b'\0'):
    if not name:
        continue
    path = ROOT / name.decode()
    if path.is_file():
        assert not pattern.search(path.read_bytes()), str(path.relative_to(ROOT)) + ': repository reference outside the unified naming'
for plugin_id in ids:
    module = (ROOT / 'plugins' / plugin_id / 'go.mod').read_text().splitlines()[0]
    assert module == 'module github.com/NoorChasib/cpa-plugins/plugins/' + plugin_id, plugin_id + ': module identity'
print('PASS: current tracked files and all Go modules use unified repository identity')
