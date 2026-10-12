import copy
import importlib.util
import json
from pathlib import Path
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'golden-allowlist-diff.py'
spec = importlib.util.spec_from_file_location('golden_allowlist_diff', SCRIPT)
allowlist = importlib.util.module_from_spec(spec)
spec.loader.exec_module(allowlist)

DOCUMENT = {
    'schemaVersion': 1,
    'credentials': [{'id': 'a', 'renewalEditable': True, 'renewalAtEpoch': 1791590400}],
    'apiCredits': {
        'editing': {'available': True, 'reason': ''},
        'accounts': [{'id': 'org-1', 'leftText': '$37.50', 'left': 37.5,
                      'settings': {'editable': True, 'notEditableReason': '', 'revision': '3'}}],
    },
}


def text(document):
    return json.dumps(document, indent=2)


class GoldenAllowlistTests(unittest.TestCase):
    def changed(self, edit):
        document = copy.deepcopy(DOCUMENT)
        edit(document)
        return allowlist.compare(text(DOCUMENT), text(document))

    def test_only_the_allowlisted_fields_may_change(self):
        def read_only(document):
            document['apiCredits']['editing'] = {'available': False, 'reason': 'quotaCache', 'migration': ''}
            document['apiCredits']['accounts'][0]['settings'].update(editable=False, notEditableReason='quotaCache')
            document['credentials'][0]['renewalEditable'] = False
        outside, inside = self.changed(read_only)
        self.assertEqual(outside, [])
        self.assertEqual([path for path, _, _ in inside], [
            'credentials[0].renewalEditable', 'apiCredits.editing',
            'apiCredits.accounts[0].settings.editable', 'apiCredits.accounts[0].settings.notEditableReason'])

    def test_every_other_change_fails(self):
        cases = {
            'an amount': lambda d: d['apiCredits']['accounts'][0].update(leftText='$37.51'),
            'a number turned into a string': lambda d: d['credentials'][0].update(renewalAtEpoch='1791590400'),
            'a revision': lambda d: d['apiCredits']['accounts'][0]['settings'].update(revision='4'),
            'a key order': lambda d: d.update(schemaVersion=d.pop('schemaVersion')),
            'a masked key removed': lambda d: d['credentials'][0].pop('renewalEditable'),
            'an added account': lambda d: d['apiCredits']['accounts'].append({'id': 'org-2'}),
        }
        for name, edit in cases.items():
            with self.subTest(name):
                outside, _ = self.changed(edit)
                self.assertNotEqual(outside, [], name)

    def test_numbers_keep_their_text(self):
        old = '{"left": 1, "used": 1.0}'
        self.assertEqual(allowlist.compare(old, old), ([], []))
        self.assertNotEqual(allowlist.compare(old, '{"left": 1.0, "used": 1.0}')[0], [])

    def test_committed_goldens_match_themselves(self):
        for relative in allowlist.GOLDENS:
            with self.subTest(relative):
                golden = (allowlist.ROOT / relative).read_text()
                self.assertEqual(allowlist.compare(golden, golden), ([], []))


if __name__ == '__main__':
    unittest.main()
