import copy
import importlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
release = importlib.import_module('plugin_release')
publisher = importlib.import_module('publish_plugin')


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.catalog = release.load_catalog()
        # Fixed fixture versions keep these regression scenarios independent
        # of catalog advancement after each real publication.
        next(p for p in self.catalog['plugins'] if p['id'] == 'quota-cache')['version'] = '0.1.0'
        next(p for p in self.catalog['plugins'] if p['id'] == 'auto-baseline')['version'] = '0.1.3'
        self.entry = copy.deepcopy(next(p for p in self.catalog['plugins'] if p['id'] == 'quota-cache'))
        self.entry['version'] = '0.1.1'

    def test_strict_tag_selection(self):
        for plugin in release.LIBRARIES:
            self.assertEqual(release.parse_tag(plugin + '/v1.2.3'), (plugin, '1.2.3'))
            release.source_version(plugin)
        for tag in ('v1.2.3', 'extra/quota-cache/v1.2.3', '../v1.2.3', 'quota-cache/v01.2.3',
                    'quota-cache/v1.2', 'quota-cache/v1.2.3-rc1', 'quota-cache/v1.2.3\n', 'unknown/v1.2.3'):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release.parse_tag(tag)

    def test_update_preserves_other_plugins_and_is_idempotent(self):
        before = copy.deepcopy(self.catalog)
        updated = release.merge_entry(self.catalog, self.entry)
        self.assertEqual(self.catalog, before)
        for old, new in zip(before['plugins'], updated['plugins']):
            self.assertEqual(new, self.entry if old['id'] == 'quota-cache' else old)
        self.assertEqual(release.merge_entry(updated, self.entry), updated)

    def test_version_order_and_conflicts(self):
        updated = release.merge_entry(self.catalog, self.entry)
        for version in ('0.1.0', '0.0.9'):
            entry = dict(self.entry, version=version)
            with self.assertRaisesRegex(ValueError, 'downgrade'):
                release.merge_entry(updated, entry)
        with self.assertRaisesRegex(ValueError, 'same version'):
            release.merge_entry(updated, dict(self.entry, description='changed bytes'))
        release.merge_entry(updated, dict(self.entry, version='0.1.10'))

    def test_catalog_alias_divergence_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'preview').mkdir()
            (root / 'registry.json').write_text(json.dumps(self.catalog))
            (root / 'preview/registry.json').write_text('{}')
            with self.assertRaisesRegex(ValueError, 'aliases diverged'):
                release.load_catalog(root)

    def package_fixture(self, root, plugin='quota-cache'):
        directory = root / 'plugins' / plugin
        library = directory / release.LIBRARIES[plugin]
        library.parent.mkdir(parents=True)
        raw = b'\x7fELF\x02\x01' + b'\0' * 10 + b'\x03\x00\x3e\x00' + b'synthetic-library'
        library.write_bytes(raw)
        (directory / 'LICENSE').write_text('license')
        if plugin == 'token-usage':
            (directory / 'docs').mkdir()
            (directory / 'docs/third-party-notices.txt').write_text('notices')
        (root / 'dist').mkdir()
        evidence = {'image': release.IMAGE, 'code_commit': 'commit', 'candidate': plugin,
                    'optional_cache_modes': ['missing-cache-wait', 'standalone'],
                    'libraries': {plugin: {'sha256': release.digest(raw), 'version': '0.1.1'}}}
        evidence_path = root / 'dist/quota-preview-evidence.json'
        evidence_path.write_text(json.dumps(evidence))
        return library, evidence_path, evidence

    def test_exact_tested_package_and_notices(self):
        for plugin in ('quota-cache', 'token-usage'):
            with self.subTest(plugin=plugin), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                library, _, _ = self.package_fixture(root, plugin)
                with patch.object(release, 'ROOT', root), patch.object(release, 'source_version', return_value='0.1.1'), \
                     patch.object(release, 'load_catalog', return_value=self.catalog), \
                     patch.object(release.subprocess, 'check_output', return_value='commit'):
                    bundle = root / 'bundle'
                    tag = plugin + '/v0.1.1'
                    release.package(tag, bundle)
                    publisher.verify_bundle(bundle, tag)
                    archive_path = next(bundle.glob('*.zip'))
                    original = archive_path.read_bytes()
                    with zipfile.ZipFile(archive_path) as archive:
                        self.assertEqual(archive.read(plugin + '.so'), library.read_bytes())
                        self.assertIn('LICENSE', archive.namelist())
                        if plugin == 'token-usage':
                            self.assertEqual(archive.read('THIRD-PARTY-NOTICES.txt'), b'notices')
                    release.package(tag, bundle)
                    self.assertEqual(archive_path.read_bytes(), original)
                    archive_path.write_bytes(b'corrupt')
                    with self.assertRaises(ValueError):
                        publisher.verify_bundle(bundle, tag)

    def test_reject_stale_evidence_wrong_version_and_architecture(self):
        for failure in ('sha', 'version', 'commit', 'image', 'candidate', 'cache_modes', 'architecture'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                library, evidence_path, evidence = self.package_fixture(root)
                if failure == 'sha':
                    library.write_bytes(library.read_bytes() + b'changed')
                elif failure == 'version':
                    evidence['libraries']['quota-cache']['version'] = '0.1.0'
                elif failure in ('commit', 'image', 'candidate'):
                    evidence['code_commit' if failure == 'commit' else failure] = 'wrong'
                elif failure == 'cache_modes':
                    evidence['optional_cache_modes'] = []
                else:
                    raw = bytearray(library.read_bytes())
                    raw[18] = 183  # AArch64, even when the evidence hash agrees.
                    library.write_bytes(raw)
                    evidence['libraries']['quota-cache']['sha256'] = release.digest(raw)
                evidence_path.write_text(json.dumps(evidence))
                with patch.object(release, 'ROOT', root), patch.object(release, 'source_version', return_value='0.1.1'), \
                     patch.object(release.subprocess, 'check_output', return_value='commit'), self.assertRaises(ValueError):
                    release.package('quota-cache/v0.1.1', root / 'bundle')

    def test_concurrent_catalog_push_reapplies_to_new_main(self):
        with tempfile.TemporaryDirectory() as tmp:
            checkout = Path(tmp)
            (checkout / 'preview').mkdir()
            remote = copy.deepcopy(self.catalog)
            pushes = []

            def run(*args, cwd=None):
                if args[:3] == ('git', 'reset', '--hard'):
                    for name in ('registry.json', 'preview/registry.json'):
                        (checkout / name).write_text(json.dumps(remote))
                return ''

            def push(args, cwd=None):
                pushes.append(release.load_catalog(checkout))
                if len(pushes) == 1:
                    next(p for p in remote['plugins'] if p['id'] == 'auto-baseline')['version'] = '0.1.4'
                    return subprocess.CompletedProcess(args, 1)
                return subprocess.CompletedProcess(args, 0)

            with patch.object(publisher, 'run', side_effect=run), patch.object(publisher.subprocess, 'run', side_effect=push), \
                 patch.object(publisher.time, 'sleep'):
                publisher.advance_catalog(checkout, self.entry)
            self.assertEqual(len(pushes), 2)
            final = {p['id']: p for p in pushes[-1]['plugins']}
            self.assertEqual(final['auto-baseline']['version'], '0.1.4')
            self.assertEqual(final['quota-cache']['version'], '0.1.1')

    def test_draft_resume_readback_and_immutable_published_assets(self):
        for draft, mismatch in ((True, False), (False, False), (True, True), (False, True)):
            with self.subTest(draft=draft, mismatch=mismatch), tempfile.TemporaryDirectory() as tmp:
                bundle = Path(tmp)
                (bundle / 'asset.zip').write_bytes(b'bytes')
                commands = []

                def run(*args, cwd=None):
                    commands.append(args)
                    if args[:3] == ('gh', 'release', 'download'):
                        destination = Path(args[args.index('--dir') + 1])
                        (destination / 'asset.zip').write_bytes(b'wrong' if mismatch else b'bytes')
                    return ''

                view = subprocess.CompletedProcess([], 0, json.dumps({'isDraft': draft, 'assets': [{'name': 'asset.zip'}]}))
                with patch.object(publisher, 'run', side_effect=run), patch.object(publisher.subprocess, 'run', return_value=view):
                    if mismatch:
                        with self.assertRaisesRegex(ValueError, 'immutable'):
                            publisher.publish_assets(bundle, 'quota-cache/v0.1.1')
                    else:
                        publisher.publish_assets(bundle, 'quota-cache/v0.1.1')
                self.assertFalse(any(c[2] == 'upload' for c in commands))
                self.assertEqual(any(c[2] == 'edit' for c in commands), draft and not mismatch)

    def test_partial_draft_uploads_only_missing_assets(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle = Path(tmp)
            for name in ('existing.zip', 'missing.txt'):
                (bundle / name).write_text(name)
            commands = []

            def run(*args, cwd=None):
                commands.append(args)
                if args[:3] == ('gh', 'release', 'download'):
                    destination = Path(args[args.index('--dir') + 1])
                    for file in bundle.iterdir():
                        (destination / file.name).write_bytes(file.read_bytes())
                return ''

            view = subprocess.CompletedProcess([], 0, json.dumps({'isDraft': True, 'assets': [{'name': 'existing.zip'}]}))
            with patch.object(publisher, 'run', side_effect=run), patch.object(publisher.subprocess, 'run', return_value=view):
                publisher.publish_assets(bundle, 'quota-cache/v0.1.1')
            uploads = [c for c in commands if c[2] == 'upload']
            self.assertEqual(len(uploads), 1)
            self.assertEqual(Path(uploads[0][4]).name, 'missing.txt')
            self.assertEqual(commands[-1][2], 'edit')


if __name__ == '__main__':
    unittest.main()
