from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
GUIDE = ROOT/'docs'/'migration.md'
QUOTA_CACHE = ROOT/'plugins'/'quota-cache'
QUOTA_GLANCE = ROOT/'plugins'/'quota-glance'

# How Quota Cache names a file beside its snapshot: cache-path with a final
# ".json" replaced by a suffix of its own (client.MeterPath and the like).
SIBLING = re.compile(r'strings\.TrimSuffix\(\w+, "\.json"\) \+ "(\.[a-z.-]+\.json)"')

LINK = re.compile(r'\]\(([^)\s]+)\)')


def siblings():
    """The suffixes of every file Quota Cache names beside its snapshot."""
    found = set()
    for path in sorted(QUOTA_CACHE.rglob('*.go')):
        if path.name.endswith('_test.go') or 'dist' in path.relative_to(QUOTA_CACHE).parts:
            continue
        found.update(SIBLING.findall(path.read_text(encoding='utf-8')))
    return found


def go_string(path, name):
    """The value of the Go string constant name declared in path, or None."""
    match = re.search(rf'\b{name}\s*=\s*"([^"\\]*)"', path.read_text(encoding='utf-8'))
    return match.group(1) if match else None


def preserve_rows():
    """The guide's "Data paths to preserve" table: what to keep, by plugin."""
    section = GUIDE.read_text(encoding='utf-8').partition('\n## Data paths to preserve\n')[2]
    rows = {}
    for line in section.partition('\n## ')[0].split('\n'):
        cells = [cell.strip() for cell in line.strip().strip('|').split('|')]
        if line.startswith('|') and len(cells) == 2 and cells[0] not in ('Plugin', '---'):
            rows[cells[0]] = cells[1]
    return rows


def anchors(path):
    """GitHub's anchors for a Markdown file's headings."""
    found = set()
    for line in path.read_text(encoding='utf-8').split('\n'):
        heading = re.match(r'#+ (.*)', line)
        if heading:
            found.add(re.sub(r'[^\w\- ]', '', heading.group(1).lower()).replace(' ', '-'))
    return found


class MigrationGuideTests(unittest.TestCase):
    # The guide is where operators look before they move or restore CPA's
    # storage. Quota Cache keeps the organizations and every value entered on
    # its page only in these files, so a file the guide does not name is lost
    # at the move. A file with nothing to keep, such as the refresh request
    # Quota Glance leaves there, is named too, so that an operator who finds
    # it knows it may be left behind.
    def test_names_every_file_quota_cache_keeps_beside_its_snapshot(self):
        suffixes = siblings()
        self.assertTrue(suffixes, 'the pattern no longer matches how Quota Cache names its files')
        guide = GUIDE.read_text(encoding='utf-8')
        missing = sorted('snapshot' + suffix for suffix in suffixes if f'`snapshot{suffix}`' not in guide)
        self.assertEqual(missing, [])

    # Quota Glance 0.7 kept the credits, refill dates, readings and renewal
    # dates in settings.json in its data-dir. Until Quota Cache 0.2.0 has
    # imported it, that file is their only copy, and a rollback copies Quota
    # Cache's settings back into it. An operator who backs up or persists only
    # the paths in the table loses it, and Quota Cache then has nothing to
    # import and starts empty.
    def test_names_quota_glance_data_dir_and_the_files_it_keeps(self):
        plugin = QUOTA_GLANCE/'internal'/'plugin'/'plugin.go'
        data_dir = go_string(plugin, 'defaultDataDir')
        files = [
            go_string(QUOTA_GLANCE/'internal'/'source'/'legacy.go', 'LegacySettingsFile'),
            go_string(QUOTA_GLANCE/'internal'/'store'/'store.go', 'fileName'),
            go_string(plugin, 'tokenFile'),
        ]
        self.assertTrue(data_dir and all(files), 'the patterns no longer match how Quota Glance names its files')
        row = preserve_rows().get('Quota Glance', '')
        expected = ['`data-dir`', f'`<CPA cwd>/{data_dir}`'] + [f'`{name}`' for name in files]
        self.assertEqual([text for text in expected if text not in row], [])

    def test_relative_links_resolve(self):
        broken = []
        for target in LINK.findall(GUIDE.read_text(encoding='utf-8')):
            if re.match(r'[a-z]+:', target):
                continue
            file, _, anchor = target.partition('#')
            path = (GUIDE.parent/file).resolve() if file else GUIDE
            if not path.is_file():
                broken.append(target)
            elif anchor and anchor not in anchors(path):
                broken.append(target)
        self.assertEqual(broken, [])


if __name__ == '__main__':
    unittest.main()
