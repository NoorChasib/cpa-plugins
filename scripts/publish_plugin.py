#!/usr/bin/env python3
"""Publish verified assets, then advance only that plugin in both catalog aliases."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import time

from plugin_release import REPO, URL, digest, download_artifact, load_catalog, merge_entry, parse_tag, require


def run(*args, cwd=None):
    return subprocess.check_output(args, cwd=cwd, text=True).strip()


def verify_bundle(bundle, tag):
    plugin, version = parse_tag(tag)
    entry = json.loads((bundle / 'catalog-entry.json').read_text())
    require((entry['id'], entry['version']) == (plugin, version), 'bundle/tag mismatch')
    name = f'{plugin}_{version}_linux_amd64.zip'
    expected = {name, 'catalog-entry.json', 'verification.json', 'checksums.txt'}
    require({p.name for p in bundle.iterdir()} == expected, 'unexpected bundle files')
    artifact = entry['install']['artifacts'][0]
    raw = (bundle / name).read_bytes()
    require(entry['install'] == {'type': 'direct', 'artifacts': [{
        'goos': 'linux', 'goarch': 'amd64', 'url': f'{URL}/releases/download/{tag}/{name}',
        'sha256': digest(raw), 'size': len(raw),
    }]}, 'unexpected artifact metadata')
    sums = ''.join(f'{digest((bundle / n).read_bytes())}  {n}\n' for n in sorted(expected - {'checksums.txt'}))
    require((bundle / 'checksums.txt').read_text() == sums, 'bundle checksum mismatch')
    return entry, artifact


def publish_assets(bundle, tag):
    args = ['gh', 'release', 'view', tag, '--repo', REPO, '--json', 'isDraft,assets']
    existing = subprocess.run(args, text=True, capture_output=True)
    if existing.returncode:
        run('gh', 'release', 'create', tag, '--repo', REPO, '--verify-tag', '--draft', '--latest=false',
            '--title', tag, '--notes', 'Independent Linux amd64 release. Native bytes verified in pinned CPA v7.2.155; see verification.json and checksums.txt.')
        existing = subprocess.run(args, text=True, capture_output=True, check=True)
    release = json.loads(existing.stdout)
    names = {p.name for p in bundle.iterdir()}
    uploaded = {a['name'] for a in release['assets']}
    require(uploaded <= names, 'release has unexpected assets; refusing to overwrite')
    missing = names - uploaded
    require(release['isDraft'] or not missing, 'published release is incomplete; refusing to modify it')
    for name in sorted(missing):
        run('gh', 'release', 'upload', tag, str(bundle / name), '--repo', REPO)
    # Read every uploaded byte back, including existing assets on a rerun.
    with tempfile.TemporaryDirectory(prefix='cpa-release-readback-') as tmp:
        run('gh', 'release', 'download', tag, '--repo', REPO, '--dir', tmp)
        require({p.name for p in Path(tmp).iterdir()} == names, 'asset list differs after upload')
        for name in names:
            require((Path(tmp) / name).read_bytes() == (bundle / name).read_bytes(), 'immutable release asset mismatch: ' + name)
    if release['isDraft']:
        run('gh', 'release', 'edit', tag, '--repo', REPO, '--draft=false', '--latest=false')


def advance_catalog(checkout, entry):
    # Each attempt starts from current main. Concurrent publishers can only fast-forward;
    # a losing push recomputes its one-entry change instead of losing the winner's change.
    for attempt in range(6):
        run('git', 'fetch', 'origin', 'main', cwd=checkout)
        run('git', 'reset', '--hard', 'origin/main', cwd=checkout)
        current = load_catalog(checkout)
        merged = merge_entry(current, entry)
        if merged == current:
            print('Catalog already contains this exact release')
            return
        for name in ('registry.json', 'preview/registry.json'):
            (checkout / name).write_text(json.dumps(merged, indent=2) + '\n')
        run('git', 'add', 'registry.json', 'preview/registry.json', cwd=checkout)
        run('git', '-c', 'user.name=github-actions[bot]', '-c', 'user.email=41898282+github-actions[bot]@users.noreply.github.com',
            'commit', '-m', f"Release {entry['id']} {entry['version']}", cwd=checkout)
        pushed = subprocess.run(['git', '-c', 'credential.helper=', '-c', 'credential.helper=!gh auth git-credential',
                                 'push', 'origin', 'HEAD:main'], cwd=checkout)
        if pushed.returncode == 0:
            return
        if attempt < 5:
            time.sleep(2)
    raise RuntimeError('Catalog push failed; check branch permissions and rerun the failed publish job. Published assets are preserved.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--bundle', type=Path, required=True)
    args = parser.parse_args()
    entry, artifact = verify_bundle(args.bundle, args.tag)
    with tempfile.TemporaryDirectory(prefix='cpa-release-catalog-') as tmp:
        checkout = Path(tmp) / 'repo'
        run('git', 'clone', '--depth=1', '--branch', 'main', URL + '.git', str(checkout))
        # Reject obsolete/same-version conflicting tags before publishing any assets.
        merge_entry(load_catalog(checkout), entry)
        publish_assets(args.bundle, args.tag)
        for attempt in range(6):
            try:
                download_artifact(artifact)  # Anonymous public URL must work before catalog publication.
                break
            except Exception:
                if attempt == 5:
                    raise
                time.sleep(3)
        advance_catalog(checkout, entry)
    print('Published ' + args.tag + ' and updated both catalog aliases')


if __name__ == '__main__':
    main()
