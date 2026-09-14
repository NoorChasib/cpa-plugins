#!/usr/bin/env python3
"""Build metadata and catalog operations for independent Linux amd64 releases."""
import argparse
import copy
import hashlib
import io
import json
from pathlib import Path
import re
import stat
import subprocess
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
REPO = 'NoorChasib/cpa-plugins'
URL = 'https://github.com/' + REPO
IMAGE = 'eceasy/cli-proxy-api@sha256:3990e4de484ac5caac80164ee3a60d0ba521320dcda193a2ef71a5ad2e2c768b'
LIBRARIES = {
    'quota-cache': 'dist/quota-cache.so',
    'account-health-pushover': 'dist/account-health-pushover.so',
    'reset-priority': 'reset-priority.so',
    'auto-baseline': 'auto-baseline.so',
    'token-usage': 'dist/token-usage.so',
    'quota-glance': 'dist/quota-glance.so',
}

# Descriptive catalog metadata for a plugin that has never been released, and so
# has no catalog entry to copy it from. Kept here, in review, rather than
# invented at release time; once the first release lands the catalog entry is
# the source of truth and this is only a fallback.
FIRST_RELEASE = {
    'quota-glance': {
        'name': 'Quota Glance',
        'description': 'One dashboard for remaining quota across every credential and rate-limit window, read from Quota Cache. Signs in with your CPA console session. Preview: Linux amd64 only.',
        'author': 'NoorChasib',
        'license': 'MIT',
    },
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def version_tuple(version):
    require(isinstance(version, str) and re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', version), 'expected numeric X.Y.Z version')
    return tuple(map(int, version.split('.')))


def parse_tag(tag):
    match = re.fullmatch(r'([^/]+)/v(.+)', tag)
    require(match and match[1] in LIBRARIES, 'expected <plugin>/vX.Y.Z release tag')
    version_tuple(match[2])
    return match[1], match[2]


def source_version(plugin):
    require(plugin in LIBRARIES, 'unknown plugin')
    directory = ROOT / 'plugins' / plugin
    filename = 'runtime.go' if plugin in ('reset-priority', 'auto-baseline') else 'plugin.go'
    source = (directory / 'internal/plugin' / filename).read_text()
    matches = re.findall(r'\b(?:PluginVersion|Version)\s*=\s*"([0-9.]+)"', source)
    require(len(matches) == 1, 'expected one native version declaration')
    version_tuple(matches[0])
    # Auto Baseline and Reset Priority derive VERSION from PluginVersion with sed.
    make_versions = re.findall(r'^VERSION\s*\?=\s*([0-9.]+)\s*$', (directory / 'Makefile').read_text(), re.M)
    require(not make_versions or make_versions == matches, 'Makefile/native versions disagree')
    return matches[0]


def load_catalog(root=ROOT):
    catalog = json.loads((root / 'registry.json').read_text())
    require(catalog == json.loads((root / 'preview/registry.json').read_text()), 'catalog aliases diverged')
    require(catalog['schema_version'] == 2, 'unsupported catalog schema')
    ids = [p['id'] for p in catalog['plugins']]
    require(len(ids) == len(set(ids)), 'duplicate plugin entries')
    # A subset, not an exact match: a plugin that has never been released has no
    # catalog entry yet, and requiring one would make a first release impossible.
    # The reverse — an entry with no known library — is still a hard error.
    require(set(ids) <= set(LIBRARIES), 'catalog lists a plugin with no known library')
    return catalog


def merge_entry(catalog, entry):
    """Reapply one entry to the latest catalog without rolling back another release."""
    require(entry['id'] in LIBRARIES, 'unknown plugin')
    result = copy.deepcopy(catalog)
    old = next((p for p in result['plugins'] if p['id'] == entry['id']), None)
    if old is None:
        # First release of this plugin. Inserted in id order so the catalog stays
        # deterministic whichever release happens to land first.
        result['plugins'].append(copy.deepcopy(entry))
        result['plugins'].sort(key=lambda p: p['id'])
        return result
    before, after = version_tuple(old['version']), version_tuple(entry['version'])
    require(after >= before, 'refusing catalog downgrade')
    require(after != before or old == entry, 'same version already has different metadata/assets; bump version')
    old.clear()
    old.update(copy.deepcopy(entry))
    return result


def first_release_entry(plugin):
    """A catalog entry for a plugin releasing for the first time."""
    require(plugin in FIRST_RELEASE, 'no catalog entry and no first-release metadata for ' + plugin)
    # repository and homepage are derived, not declared: check-catalog.py asserts
    # both, so there is exactly one spelling of each and no way to typo one.
    return dict(FIRST_RELEASE[plugin], id=plugin, repository=URL,
                homepage=URL + '/tree/main/plugins/' + plugin)


def download_artifact(artifact):
    require(artifact['url'].startswith(URL + '/releases/download/'), 'unexpected artifact origin')
    with urllib.request.urlopen(artifact['url'], timeout=120) as response:
        raw = response.read()
    require(len(raw) == artifact['size'] and digest(raw) == artifact['sha256'], 'download integrity mismatch')
    return raw


def released_library(entry):
    artifacts = [a for a in entry['install']['artifacts'] if (a['goos'], a['goarch']) == ('linux', 'amd64')]
    require(len(artifacts) == 1, 'expected one Linux amd64 artifact')
    with zipfile.ZipFile(io.BytesIO(download_artifact(artifacts[0]))) as archive:
        name = entry['id'] + '.so'
        require(archive.namelist().count(name) == 1 and archive.testzip() is None, 'invalid release archive')
        return archive.read(name)


def package(tag, output):
    plugin, version = parse_tag(tag)
    require(source_version(plugin) == version, 'tag/native source version mismatch')
    evidence = json.loads((ROOT / 'dist/quota-preview-evidence.json').read_text())
    commit = subprocess.check_output(['git', '-C', str(ROOT), 'rev-parse', 'HEAD'], text=True).strip()
    require(evidence['image'] == IMAGE and evidence['code_commit'] == commit, 'wrong runtime/commit evidence')
    require(evidence.get('candidate') == plugin, 'candidate was not tested with released peers')
    require(evidence.get('optional_cache_modes') == ['missing-cache-wait', 'standalone'], 'missing optional cache checks')
    library = (ROOT / 'plugins' / plugin / LIBRARIES[plugin]).read_bytes()
    require(evidence['libraries'][plugin] == {'sha256': digest(library), 'version': version}, 'library differs from CPA-tested bytes/version')
    require(len(library) >= 20 and library[:6] == b'\x7fELF\x02\x01' and library[16:20] == b'\x03\x00\x3e\x00', 'expected ELF64 little-endian x86-64 shared library')
    members = [(plugin + '.so', library, 0o755), ('LICENSE', (ROOT / 'plugins' / plugin / 'LICENSE').read_bytes(), 0o644)]
    if plugin == 'token-usage':
        members.append(('THIRD-PARTY-NOTICES.txt', (ROOT / 'plugins/token-usage/docs/third-party-notices.txt').read_bytes(), 0o644))
    output.mkdir(parents=True, exist_ok=True)
    name = f'{plugin}_{version}_linux_amd64.zip'
    with zipfile.ZipFile(output / name, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
        for member, raw, mode in members:
            info = zipfile.ZipInfo(member, date_time=(1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | mode) << 16
            archive.writestr(info, raw, compress_type=zipfile.ZIP_DEFLATED)
    raw = (output / name).read_bytes()
    entry = copy.deepcopy(next((p for p in load_catalog()['plugins'] if p['id'] == plugin), None) or first_release_entry(plugin))
    entry['version'] = version
    entry['install'] = {'type': 'direct', 'artifacts': [{
        'goos': 'linux', 'goarch': 'amd64', 'url': f'{URL}/releases/download/{tag}/{name}',
        'sha256': digest(raw), 'size': len(raw),
    }]}
    (output / 'catalog-entry.json').write_text(json.dumps(entry, indent=2) + '\n')
    (output / 'verification.json').write_text(json.dumps(evidence, indent=2) + '\n')
    names = [name, 'catalog-entry.json', 'verification.json']
    (output / 'checksums.txt').write_text(''.join(f'{digest((output / n).read_bytes())}  {n}\n' for n in sorted(names)))
    print(f'Packaged {tag}; verified exact native bytes in CPA v7.2.155')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    select = sub.add_parser('select')
    select.add_argument('--tag')
    select.add_argument('--plugin', choices=LIBRARIES)
    pack = sub.add_parser('package')
    pack.add_argument('--tag', required=True)
    pack.add_argument('--output', type=Path, required=True)
    merge = sub.add_parser('merge')
    merge.add_argument('--entry', type=Path, required=True)
    merge.add_argument('--root', type=Path, default=ROOT)
    args = parser.parse_args()
    if args.command == 'select':
        if args.tag:
            plugin, version = parse_tag(args.tag)
            require(source_version(plugin) == version, 'tag/native source version mismatch')
        else:
            plugin = args.plugin
            version = source_version(plugin)
        print(f'plugin={plugin}\nversion={version}\ntag={plugin}/v{version}')
    elif args.command == 'package':
        package(args.tag, args.output)
    else:
        entry = json.loads(args.entry.read_text())
        download_artifact(entry['install']['artifacts'][0])
        result = merge_entry(load_catalog(args.root), entry)
        for path in ('registry.json', 'preview/registry.json'):
            (args.root / path).write_text(json.dumps(result, indent=2) + '\n')


if __name__ == '__main__':
    main()
