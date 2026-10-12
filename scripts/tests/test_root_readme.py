from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
README = ROOT/'README.md'

# A boolean field a plugin offers in CPA's Edit config dialog, in either of the
# two forms the plugins declare them: a hostapi.ConfigField literal or the
# field helper. The description is the Go string literal that follows.
BOOLEAN_FIELD = re.compile(
    r'\{Name: "([a-z-]+)", Type: "boolean", Description: "((?:[^"\\]|\\.)*)"'
    r'|field\("([a-z-]+)", "boolean", "((?:[^"\\]|\\.)*)"')

# How a field's description says that turning it on makes the plugin read
# Quota Cache's files.
READS_QUOTA_CACHE = re.compile(r'Quota Cache|shared quota observations')


def quota_cache_switches():
    """Each plugin's boolean settings that make it read Quota Cache's files.

    Quota Cache writes the files rather than reading them, so its own settings
    are left out.
    """
    found = set()
    for path in sorted((ROOT/'plugins').glob('*/internal/**/*.go')):
        plugin = path.relative_to(ROOT/'plugins').parts[0]
        if path.name.endswith('_test.go') or plugin == 'quota-cache':
            continue
        for match in BOOLEAN_FIELD.finditer(path.read_text(encoding='utf-8')):
            name = match.group(1) or match.group(3)
            description = match.group(2) or match.group(4)
            if READS_QUOTA_CACHE.search(description):
                found.add((plugin, name))
    return found


class RootReadmeTests(unittest.TestCase):
    # The root README is where operators decide which plugins can stay
    # standalone. A setting that makes a plugin read Quota Cache but goes
    # unnamed there lets someone who never installed Quota Cache turn it on
    # and then find the plugin waiting on files nobody writes.
    def test_names_every_setting_that_reads_quota_cache(self):
        switches = quota_cache_switches()
        self.assertTrue(switches, 'the patterns no longer match how the plugins declare their settings')
        readme = README.read_text(encoding='utf-8')
        missing = sorted(f'{plugin}: {name}' for plugin, name in switches if f'`{name}' not in readme)
        self.assertEqual(missing, [])


if __name__ == '__main__':
    unittest.main()
