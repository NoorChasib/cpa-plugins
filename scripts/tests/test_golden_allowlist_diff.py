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


# The summary as it was while Quota Glance still published editing.migration.
WITH_MIGRATION = copy.deepcopy(DOCUMENT)
WITH_MIGRATION['apiCredits']['editing']['migration'] = ''


def configured_account(id, monthly_usd, renews):
    """An API credit account whose credit and refill date the configuration
    gave, with a flag and an issue only a configured value raises."""
    return {
        'id': id, 'monthlyCreditSource': 'config', 'renewsSource': 'config',
        'cycleStartEpoch': 1788825600, 'renewsAtEpoch': 1791417600, 'renewsInSeconds': 2404800,
        'leftText': '$37.50', 'dataIssues': ['stale', 'configRenewsInvalid'],
        'settings': {'revision': '', 'monthlyUsd': '', 'configMonthlyUsd': monthly_usd, 'configMonthlyUsdInvalid': False,
                     'renews': '', 'configRenews': renews, 'configRenewsInvalid': True, 'updatedAtEpoch': None},
    }


# Two accounts as the configuration gave them: one whose values the page now
# gives, and one the page cannot edit, which now has none.
CONFIGURED = {'apiCredits': {'accounts': [configured_account('org-1', '200', '2026-09-29'),
                                          configured_account('label-1', '100', '2026-09-08')]}}


def from_the_page(document):
    paged, unedited = document['apiCredits']['accounts']
    paged.update(monthlyCreditSource='dashboard', renewsSource='dashboard', dataIssues=['stale'])
    paged['settings'].update(revision='3', monthlyUsd='200', configMonthlyUsd='', renews='2026-09-29',
                             configRenews='', configRenewsInvalid=False, updatedAtEpoch=1788220800)
    unedited.update(monthlyCreditSource='none', renewsSource='none', dataIssues=['stale'],
                    cycleStartEpoch=None, renewsAtEpoch=None, renewsInSeconds=None)
    unedited['settings'].update(configMonthlyUsd='', configRenews='', configRenewsInvalid=False)


def text(document):
    return json.dumps(document, indent=2)


class GoldenAllowlistTests(unittest.TestCase):
    def changed(self, edit):
        document = copy.deepcopy(DOCUMENT)
        edit(document)
        return allowlist.compare(text(DOCUMENT), text(document))

    def test_only_the_allowlisted_fields_may_change(self):
        def read_only(document):
            document['apiCredits']['editing'] = {'available': False, 'reason': 'quotaCache'}
            document['apiCredits']['accounts'][0]['settings'].update(editable=False, notEditableReason='quotaCache')
            document['credentials'][0]['renewalEditable'] = False
        outside, inside = self.changed(read_only)
        self.assertEqual(outside, [])
        self.assertEqual([path for path, _, _ in inside], [
            'credentials[0].renewalEditable', 'apiCredits.editing.available', 'apiCredits.editing.reason',
            'apiCredits.accounts[0].settings.editable', 'apiCredits.accounts[0].settings.notEditableReason'])

    def test_the_migration_field_may_only_go(self):
        outside, inside = allowlist.compare(text(WITH_MIGRATION), text(DOCUMENT))
        self.assertEqual(outside, [])
        self.assertEqual([(path, allowlist.plain(old), allowlist.plain(new)) for path, old, new in inside],
                         [('apiCredits.editing.migration', '""', '(removed)')])
        with self.subTest('still there'):
            self.assertNotEqual(allowlist.compare(text(WITH_MIGRATION), text(WITH_MIGRATION))[0], [])
        with self.subTest('added'):
            self.assertNotEqual(allowlist.compare(text(DOCUMENT), text(WITH_MIGRATION))[0], [])

    def test_a_configured_value_may_come_from_the_page_instead(self):
        def page(edit=lambda d: None):
            document = copy.deepcopy(CONFIGURED)
            from_the_page(document)
            edit(document)
            return allowlist.compare(text(CONFIGURED), text(document))

        outside, inside = page()
        self.assertEqual(outside, [])
        paged = [f'apiCredits.accounts[0].{key}' for key in (
            'monthlyCreditSource', 'settings.monthlyUsd', 'settings.configMonthlyUsd',
            'renewsSource', 'settings.renews', 'settings.configRenews', 'settings.configRenewsInvalid',
            'settings.revision', 'settings.updatedAtEpoch', 'dataIssues')]
        unedited = [f'apiCredits.accounts[1].{key}' for key in (
            'monthlyCreditSource', 'settings.configMonthlyUsd', 'renewsSource',
            'cycleStartEpoch', 'renewsAtEpoch', 'renewsInSeconds',
            'settings.configRenews', 'settings.configRenewsInvalid', 'dataIssues')]
        self.assertEqual([path for path, _, _ in inside], paged + unedited)
        cases = {
            'a page value other than the configured one':
                lambda d: d['apiCredits']['accounts'][0]['settings'].update(monthlyUsd='250'),
            'a figure moved beside it': lambda d: d['apiCredits']['accounts'][0].update(leftText='$37.51'),
            'a source other than the page or none':
                lambda d: d['apiCredits']['accounts'][1].update(renewsSource='dashboard'),
            'a cycle gone while its refill date stays':
                lambda d: d['apiCredits']['accounts'][0].update(cycleStartEpoch=None),
            'another issue gone': lambda d: d['apiCredits']['accounts'][0].update(dataIssues=[]),
            'an issue added': lambda d: d['apiCredits']['accounts'][0]['dataIssues'].append('meterGap'),
            'a configured value changed': lambda d: d['apiCredits']['accounts'][1]['settings'].update(configRenews='2026-09-09'),
            'a flag raised': lambda d: d['apiCredits']['accounts'][0]['settings'].update(configMonthlyUsdInvalid=True),
            'a revision with no page value':
                lambda d: d['apiCredits']['accounts'][1]['settings'].update(revision='3'),
            'the accounts swapped': lambda d: d['apiCredits']['accounts'].reverse(),
        }
        for name, edit in cases.items():
            with self.subTest(name):
                self.assertNotEqual(page(edit)[0], [], name)
        with self.subTest('back to the configuration'):
            document = copy.deepcopy(CONFIGURED)
            from_the_page(document)
            self.assertNotEqual(allowlist.compare(text(document), text(CONFIGURED))[0], [])

    def test_every_other_change_fails(self):
        cases = {
            'an amount': lambda d: d['apiCredits']['accounts'][0].update(leftText='$37.51'),
            'a number turned into a string': lambda d: d['credentials'][0].update(renewalAtEpoch='1791590400'),
            'a revision': lambda d: d['apiCredits']['accounts'][0]['settings'].update(revision='4'),
            'a key order': lambda d: d.update(schemaVersion=d.pop('schemaVersion')),
            'a masked key removed': lambda d: d['credentials'][0].pop('renewalEditable'),
            'a key added beside a masked one': lambda d: d['apiCredits']['editing'].update(note=''),
            'the masked keys reordered': lambda d: d['apiCredits']['editing'].update(
                available=d['apiCredits']['editing'].pop('available')),
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
