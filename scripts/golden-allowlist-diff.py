#!/usr/bin/env python3
"""Check that Quota Glance's regenerated summary goldens changed only where the
API credits restructure allows.

Quota Glance 0.8.0 shows the API credit figures Quota Cache publishes instead of
working them out itself. The figures must not move: only the fields that said
Glance could edit them change value. So, before regenerated goldens are
committed, this compares each one with the committed copy with these paths
masked, and fails on any other difference:

  apiCredits.editing.available
  apiCredits.editing.reason
  apiCredits.accounts[*].settings.editable
  apiCredits.accounts[*].settings.notEditableReason
  credentials[*].renewalEditable

One field leaves the document. The committed copy may have it, the regenerated
one must not:

  apiCredits.editing.migration

Quota Cache 0.2.0 takes no monthly credit or refill date from its
configuration, so the test data now enters on the page every value the
configuration gave. An API credit account may differ by exactly what that
changes:

  monthlyCreditSource or renewsSource "config" -> "dashboard", when
      settings.monthlyUsd or settings.renews goes from "" to the old
      settings.configMonthlyUsd or settings.configRenews; settings.revision
      "" and settings.updatedAtEpoch null then take any value
  monthlyCreditSource or renewsSource "config" -> "none"; for renewsSource,
      the cycle it gave goes too: cycleStartEpoch, renewsAtEpoch and
      renewsInSeconds -> null
  settings.configMonthlyUsd and settings.configRenews -> ""
  settings.configMonthlyUsdInvalid and settings.configRenewsInvalid -> false
  dataIssues -> the same list without configMonthlyUsdInvalid,
      configRenewsInvalid and adminKeyIgnored

Everything else must match exactly: every key in the same order, every value
with the same type and the same text, so an amount that moved by a cent, a
reordered key or a number turned into a string all fail. A masked key keeps its
presence: one that appears or disappears is a difference too, and so is a key
added beside it.

    python3 scripts/golden-allowlist-diff.py                 # HEAD against the working tree
    python3 scripts/golden-allowlist-diff.py --base origin/main
    python3 scripts/golden-allowlist-diff.py OLD.json NEW.json

CI keeps running `make golden` and `git diff --exit-code`; this is the review
step before the regenerated files are committed.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
GOLDENS = (
    'plugins/quota-glance/testdata/golden/summary.json',
    'plugins/quota-glance/testdata/golden/summary-degraded.json',
)
ALLOWED = (
    ('apiCredits', 'editing', 'available'),
    ('apiCredits', 'editing', 'reason'),
    ('apiCredits', 'accounts', '*', 'settings', 'editable'),
    ('apiCredits', 'accounts', '*', 'settings', 'notEditableReason'),
    ('credentials', '*', 'renewalEditable'),
)
REMOVED = (
    ('apiCredits', 'editing', 'migration'),
)
# Per value an account's configuration could give: where the account says it
# came from, the page's value, the configuration's value, and its flag.
CONFIGURED = (
    ('monthlyCreditSource', 'monthlyUsd', 'configMonthlyUsd', 'configMonthlyUsdInvalid'),
    ('renewsSource', 'renews', 'configRenews', 'configRenewsInvalid'),
)
# The data issues only a configured value raised.
CONFIG_ISSUES = ('configMonthlyUsdInvalid', 'configRenewsInvalid', 'adminKeyIgnored')
# What a refill date gives an account.
CYCLE = ('cycleStartEpoch', 'renewsAtEpoch', 'renewsInSeconds')
SHOWN = 50


class Number:
    """A JSON number as written, so 1, 1.0 and 1e0 stay different, and a
    number never equals the string with the same digits."""

    def __init__(self, text):
        self.text = text

    def __eq__(self, other):
        return isinstance(other, Number) and other.text == self.text

    def __repr__(self):
        return self.text


class Masked:
    """A value the allowlist lets change. It equals any other masked value and
    nothing else, so a masked key still has to be present on both sides."""

    def __init__(self, value):
        self.value = value

    def __eq__(self, other):
        return isinstance(other, Masked)


# What a removed field became, for the report.
GONE = object()


def parse(text):
    return json.loads(text, parse_int=Number, parse_float=Number, parse_constant=Number)


def plain(value):
    """value as JSON text, for a message."""
    if value is GONE:
        return '(removed)'
    if isinstance(value, Masked):
        value = value.value
    return json.dumps(value, default=lambda v: v.value if isinstance(v, Masked) else json.loads(v.text),
                      separators=(',', ':'))


def matches(document, path, prefix=()):
    """Each (object, key, location) where path, with * for every item of a
    list, names a key present in document. location is a dotted path."""
    if not path:
        return []
    head, rest = path[0], path[1:]
    if head == '*':
        if not isinstance(document, list):
            return []
        found = []
        for i, item in enumerate(document):
            found += matches(item, rest, prefix + (f'[{i}]',))
        return found
    if not isinstance(document, dict) or head not in document:
        return []
    if not rest:
        return [(document, head, where(prefix + (head,)))]
    return matches(document[head], rest, prefix + (head,))


def mask(document, path):
    """Replace the value at each match of path with Masked, keeping the key."""
    for parent, key, _ in matches(document, path):
        parent[key] = Masked(parent[key])


def drop(document, path):
    """Remove each match of path. Returns them as (location, value, GONE)."""
    return [(location, parent.pop(key), GONE) for parent, key, location in matches(document, path)]


def where(parts):
    text = ''
    for part in parts:
        text += part if part.startswith('[') else ('.' if text else '') + part
    return text or '(document)'


def differences(old, new, parts=()):
    """Every place old and new differ, as (path, old, new); masked values only
    where one side is masked and the other is not."""
    if isinstance(old, Masked) or isinstance(new, Masked):
        return [] if old == new else [(where(parts), old, new)]
    if isinstance(old, dict) and isinstance(new, dict):
        if list(old) != list(new):
            return [(where(parts) + ' (keys)', list(old), list(new))]
        found = []
        for key in old:
            found += differences(old[key], new[key], parts + (key,))
        return found
    if isinstance(old, list) and isinstance(new, list):
        if len(old) != len(new):
            return [(where(parts) + ' (length)', len(old), len(new))]
        found = []
        for i, (a, b) in enumerate(zip(old, new)):
            found += differences(a, b, parts + (f'[{i}]',))
        return found
    if type(old) is not type(new) or old != new:
        return [(where(parts), old, new)]
    return []


def allowed_changes(old, new, parts=()):
    """The masked locations whose value changed, for the report."""
    if isinstance(old, Masked) and isinstance(new, Masked):
        return [] if plain(old) == plain(new) else [(where(parts), old, new)]
    if isinstance(old, dict) and isinstance(new, dict):
        found = []
        for key in old:
            if key in new:
                found += allowed_changes(old[key], new[key], parts + (key,))
        return found
    if isinstance(old, list) and isinstance(new, list):
        found = []
        for i, (a, b) in enumerate(zip(old, new)):
            found += allowed_changes(a, b, parts + (f'[{i}]',))
        return found
    return []


def credit_accounts(document):
    """The API credit accounts in document, or None."""
    credits = document.get('apiCredits') if isinstance(document, dict) else None
    accounts = credits.get('accounts') if isinstance(credits, dict) else None
    return accounts if isinstance(accounts, list) else None


def from_the_page(old, new):
    """Take into each of old's API credit accounts what new changes by giving
    on the page the values the configuration gave, and nothing else. Accounts
    are paired by position and id. Returns the changes, as (location, old,
    new)."""
    changes = []
    olds, news = credit_accounts(old), credit_accounts(new)
    if olds is None or news is None or len(olds) != len(news):
        return changes

    def take(document, update, key, location):
        if key in document and key in update and differences(document[key], update[key]):
            changes.append((location, document[key], update[key]))
            document[key] = update[key]

    for i, (o, n) in enumerate(zip(olds, news)):
        if not (isinstance(o, dict) and isinstance(n, dict) and o.get('id') == n.get('id')):
            continue
        so, sn = o.get('settings'), n.get('settings')
        if not (isinstance(so, dict) and isinstance(sn, dict)):
            continue
        at = f'apiCredits.accounts[{i}]'
        paged = False
        for source, value, configured, invalid in CONFIGURED:
            if o.get(source) == 'config' and n.get(source) == 'dashboard' \
                    and so.get(value) == '' and sn.get(value) == so.get(configured):
                take(o, n, source, f'{at}.{source}')
                take(so, sn, value, f'{at}.settings.{value}')
                paged = True
            elif o.get(source) == 'config' and n.get(source) == 'none':
                take(o, n, source, f'{at}.{source}')
                if source == 'renewsSource':
                    for key in CYCLE:
                        if n.get(key) is None:
                            take(o, n, key, f'{at}.{key}')
            if sn.get(configured) == '':
                take(so, sn, configured, f'{at}.settings.{configured}')
            if sn.get(invalid) is False:
                take(so, sn, invalid, f'{at}.settings.{invalid}')
        if paged and so.get('revision') == '' and so.get('updatedAtEpoch') is None:
            take(so, sn, 'revision', f'{at}.settings.revision')
            take(so, sn, 'updatedAtEpoch', f'{at}.settings.updatedAtEpoch')
        issues = o.get('dataIssues')
        if isinstance(issues, list) and [issue for issue in issues if issue not in CONFIG_ISSUES] == n.get('dataIssues'):
            take(o, n, 'dataIssues', f'{at}.dataIssues')
    return changes


def compare(old_text, new_text):
    """(outside, inside): the differences the allowlist does not cover, and
    the masked values that changed, the values that moved from the
    configuration to the page, and the removed fields that went. A removed
    field is taken out of the old document only, so one still in the new
    document is a difference."""
    old, new = parse(old_text), parse(new_text)
    paged = from_the_page(old, new)
    removed = []
    for path in REMOVED:
        removed += drop(old, path)
    for path in ALLOWED:
        mask(old, path)
        mask(new, path)
    return differences(old, new), allowed_changes(old, new) + paged + removed


def committed(base, relative):
    try:
        return subprocess.run(['git', '-C', str(ROOT), 'show', f'{base}:{relative}'],
                              check=True, capture_output=True, text=True).stdout
    except subprocess.CalledProcessError as error:
        raise SystemExit(f'cannot read {relative} at {base}: {error.stderr.strip()}')


def report(name, old_text, new_text):
    outside, inside = compare(old_text, new_text)
    if outside:
        print(f'FAIL {name}: {len(outside)} difference(s) outside the allowlist')
        for path, old, new in outside[:SHOWN]:
            print(f'  {path}: {plain(old)} -> {plain(new)}')
        if len(outside) > SHOWN:
            print(f'  … and {len(outside) - SHOWN} more')
        return False
    print(f'ok   {name}: identical outside the allowlist; {len(inside)} allowlisted change(s)')
    for path, old, new in inside:
        print(f'  {path}: {plain(old)} -> {plain(new)}')
    return True


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n\n')[0],
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--base', default='HEAD', help='git revision holding the previous goldens (default HEAD)')
    parser.add_argument('files', nargs='*', metavar='OLD NEW', help='compare two files instead')
    args = parser.parse_args(argv)
    if args.files:
        if len(args.files) != 2:
            parser.error('give exactly two files: OLD NEW')
        old, new = (Path(name) for name in args.files)
        return 0 if report(str(new), old.read_text(), new.read_text()) else 1
    ok = True
    for relative in GOLDENS:
        ok = report(relative, committed(args.base, relative), (ROOT / relative).read_text()) and ok
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
