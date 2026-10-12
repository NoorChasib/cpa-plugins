from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
GUIDE = ROOT/'docs'/'migration.md'
QUOTA_CACHE = ROOT/'plugins'/'quota-cache'
QUOTA_GLANCE = ROOT/'plugins'/'quota-glance'
RESET_PRIORITY = ROOT/'plugins'/'reset-priority'

# Where Quota Glance 0.7 saved the API credit settings, in its data-dir. Quota
# Glance 0.8.0 no longer names the file, so it is given as 0.7.2 named it.
QUOTA_GLANCE_07_SETTINGS = 'settings.json'

# How Quota Cache names a file beside its snapshot: cache-path with a final
# ".json" replaced by a suffix of its own (client.MeterPath and the like).
SIBLING = re.compile(r'strings\.TrimSuffix\(\w+, "\.json"\) \+ "(\.[a-z.-]+\.json)"')

LINK = re.compile(r'\]\(([^)\s]+)\)')

# Where one sentence of a Markdown line ends and the next begins.
SENTENCE_BREAK = re.compile(r'(?<=[.!?])\s+(?=[A-Z*\[`(])')

# Where one clause of a Markdown line ends and the next begins.
CLAUSE_BREAK = re.compile(r'[.!?;:,\u2014]\s+|\s+\u2014\s+')

# A sentence that says to keep a copy of Quota Glance's settings file.
KEEP_GLANCE_SETTINGS = re.compile(
    r"\b(?:keep|make|take)\s+a\s+(?:copy|backup)\s+of\s+Quota Glance['’]s\s+(?:file|`settings\.json`)",
    re.IGNORECASE)

# A word that moves something, and one that denies it.
MOVE = re.compile(r'\b(?:mov(?:e|es|ed|ing)|migrat\w*)\b', re.IGNORECASE)
NEGATION = re.compile(r"\b(?:no|not|nothing|never|none|neither|nor)\b|n['\u2019]t\b", re.IGNORECASE)

# A word that names the update, and one that names a gap.
ABOUT_UPDATE = re.compile(r'\bupdat(?:e|es|ed|ing)\b', re.IGNORECASE)
ABOUT_GAP = re.compile(r'\bgaps?\b', re.IGNORECASE)

# Wording that ends a gap when the first organization is added.
FIRST_ADD = re.compile(
    r'\bfirst (?:organization|one)\b[^.]*\badd(?:s|ed|ing)?\b|\badd(?:s|ed|ing)?\s+the first (?:organization|one)\b|\bfirst add\b',
    re.IGNORECASE)

# The heading both the guide and Quota Cache's README put the update steps under.
UPDATE_HEADING = '### Updating from Quota Glance 0.7\n'

# Wording that puts the updates of Quota Cache, Quota Glance and Reset
# Priority in order, in numbered steps or in a sentence of prose.
PLUGIN = r'(?:Quota (?:Cache|Glance)|Reset Priority)'
ORDERS_UPDATES = re.compile(
    rf'\bupdat(?:e|ing)\s+{PLUGIN}\s+(?:first|last)\b'
    rf'|\bthen\s+updat(?:e|ing)\s+{PLUGIN}'
    rf'|\b(?:before|after)\s+(?:updating\s+{PLUGIN}|{PLUGIN}\s+is\s+updated)\b',
    re.IGNORECASE)


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


def operator_docs():
    """The guide, the repository's README and release guide, and the
    documents Quota Cache, Quota Glance and Reset Priority ship."""
    paths = [GUIDE, ROOT/'README.md', ROOT/'docs'/'releases.md']
    for plugin in (QUOTA_CACHE, QUOTA_GLANCE, RESET_PRIORITY):
        paths.extend(path for path in (plugin/'README.md', plugin/'REFERENCE.md') if path.is_file())
        paths.extend(sorted((plugin/'docs').glob('*.md')))
    return paths


def sentences(path):
    """Each sentence of a Markdown file, none spanning two lines."""
    for line in path.read_text(encoding='utf-8').split('\n'):
        yield from SENTENCE_BREAK.split(line)


def clauses(text):
    """Each clause of Markdown text, none spanning two lines."""
    for line in text.split('\n'):
        yield from CLAUSE_BREAK.split(line)


def describes_copy_back(sentence):
    """Whether sentence describes copying Quota Cache's settings over
    Quota Glance 0.7's."""
    return bool(re.search(r'\bcopy\b', sentence, re.IGNORECASE)
                and '`snapshot.settings.json`' in sentence and f'`{QUOTA_GLANCE_07_SETTINGS}`' in sentence)


def says_organizations_move_from_configuration(clause):
    """Whether clause says that the organizations, or claude-api-credits,
    move from the configuration, without denying it."""
    about = '`claude-api-credits`' in clause or (
        re.search(r'organi[sz]ation', clause, re.IGNORECASE) and re.search(r'\bconfig', clause, re.IGNORECASE))
    return bool(about and MOVE.search(clause) and not NEGATION.search(clause))


def says_to_save_nothing_in_quota_glance_07(sentence):
    """Whether sentence says to save nothing in Quota Glance's editor until
    Quota Glance is updated."""
    return bool(re.search(r'\bsave nothing\b', sentence, re.IGNORECASE)
                and re.search(r'\beditor\b', sentence, re.IGNORECASE)
                and re.search(r'\buntil Quota Glance is updated\b', sentence, re.IGNORECASE))


def describes_the_gap_across_the_update(paragraph):
    """Whether paragraph describes a gap in counting across the update."""
    return bool(ABOUT_UPDATE.search(paragraph) and ABOUT_GAP.search(paragraph))


def update_section(path):
    """What a document says under its update heading, up to the next heading."""
    section = path.read_text(encoding='utf-8').partition(UPDATE_HEADING)[2]
    return re.split(r'\n#+ ', section, maxsplit=1)[0]


def update_steps(path):
    """The numbered steps under a document's update heading."""
    return re.findall(r'^[0-9]+\. .*$', update_section(path), re.MULTILINE)


def sections(text):
    """Each section of Markdown text, from one heading to the next, as its
    paragraphs with every run of whitespace made one space, so a sentence
    reads the same however the file wraps it."""
    for section in re.split(r'^#+ .*$', text, flags=re.MULTILINE):
        yield [' '.join(paragraph.split()) for paragraph in re.split(r'\n\s*\n', section) if paragraph.strip()]


def orders_the_updates(paragraph):
    """Whether paragraph puts the updates of the three plugins in order."""
    return bool(ORDERS_UPDATES.search(paragraph))


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

    # Nor does it name a file Quota Cache does not keep, which an operator
    # would look for in vain, or keep for nothing.
    def test_names_no_file_quota_cache_does_not_keep(self):
        named = set(re.findall(r'`snapshot(\.[a-z.-]+\.json)`', GUIDE.read_text(encoding='utf-8')))
        self.assertEqual(sorted('snapshot' + suffix for suffix in named - siblings()), [])

    # Quota Glance 0.7 kept the credits, refill dates, readings and renewal
    # dates in settings.json in its data-dir. Nothing reads it since, but until
    # they are entered again on Quota Cache's page it is their only copy, and
    # a rollback to Quota Glance 0.7.2 shows them from it again. An operator
    # who backs up or persists only the paths in the table loses it.
    def test_names_quota_glance_data_dir_and_the_files_it_keeps(self):
        plugin = QUOTA_GLANCE/'internal'/'plugin'/'plugin.go'
        data_dir = go_string(plugin, 'defaultDataDir')
        files = [
            go_string(QUOTA_GLANCE/'internal'/'store'/'store.go', 'fileName'),
            go_string(plugin, 'tokenFile'),
        ]
        self.assertTrue(data_dir and all(files), 'the patterns no longer match how Quota Glance names its files')
        files.append(QUOTA_GLANCE_07_SETTINGS)
        row = preserve_rows().get('Quota Glance', '')
        expected = ['`data-dir`', f'`<CPA cwd>/{data_dir}`'] + [f'`{name}`' for name in files]
        self.assertEqual([text for text in expected if text not in row], [])

    # Copying Quota Cache's settings over Quota Glance's settings.json carries
    # the values entered on Quota Cache's page back to a rolled-back Quota
    # Glance 0.7.2. But 0.7.2 reads that file only when it starts, and writes
    # out what it holds at each save. A copy made while it runs is never
    # shown, and its next save overwrites the copy. Stopping CPA stops it on
    # every CPA version. So wherever the copy is described, it is made while
    # CPA is stopped.
    def test_copying_settings_back_to_quota_glance_07_waits_for_cpa_to_stop(self):
        instructions = [
            (path, sentence) for path in operator_docs() for sentence in sentences(path) if describes_copy_back(sentence)
        ]
        self.assertTrue(instructions, 'no document describes copying the settings back any more, or the pattern no longer matches it')
        untimed = [f'{path.relative_to(ROOT)}: {sentence}' for path, sentence in instructions if 'CPA is stopped' not in sentence]
        self.assertEqual(untimed, [])

    # Copying Quota Cache's settings over Quota Glance's settings.json
    # replaces the whole file. Until every value saved in Quota Glance 0.7 is
    # entered on Quota Cache's page, that file is their only copy, so a copy
    # made while some are still to be entered loses them for good. So
    # wherever the copy is described, the same paragraph says to keep a copy
    # of Quota Glance's file first.
    def test_copying_settings_back_says_to_keep_quota_glance_07_settings_first(self):
        paragraphs = [
            (path, line) for path in operator_docs() for line in path.read_text(encoding='utf-8').split('\n')
            if any(map(describes_copy_back, SENTENCE_BREAK.split(line)))
        ]
        self.assertTrue(paragraphs, 'no document describes copying the settings back any more, or the pattern no longer matches it')
        unwarned = [
            f'{path.relative_to(ROOT)}: {line[:80]}…' for path, line in paragraphs if not KEEP_GLANCE_SETTINGS.search(line)
        ]
        self.assertEqual(unwarned, [])

    def test_recognises_keeping_quota_glance_settings(self):
        named = [
            "Keep a copy of Quota Glance's file first if you may want its old values.",
            "Make a backup of Quota Glance's `settings.json` before the copy.",
        ]
        unnamed = [
            "Copy `snapshot.settings.json` over Quota Glance's `settings.json` while CPA is stopped.",
            'Keep a copy of `snapshot.settings.json` with the snapshot.',
            "Quota Glance's `settings.json` stays where it is, untouched and unread.",
        ]
        self.assertEqual([text for text in named if not KEEP_GLANCE_SETTINGS.search(text)], [])
        self.assertEqual([text for text in unnamed if KEEP_GLANCE_SETTINGS.search(text)], [])

    # Quota Cache 0.1.13 and 0.1.14 read the organizations from
    # claude-api-credits in their configuration. 0.2.0 lists only the ones
    # added on its page, and nothing puts them there for the operator: told
    # they move over, an operator updates, finds none listed, and loses the
    # metering of every organization until they are added. So no document
    # says that the configuration's organizations move, unless it denies it.
    def test_no_document_says_the_organizations_move_from_the_configuration(self):
        claims = [
            f'{path.relative_to(ROOT)}: {clause}' for path in operator_docs()
            for clause in clauses(path.read_text(encoding='utf-8'))
            if says_organizations_move_from_configuration(clause)
        ]
        self.assertEqual(claims, [])

    def test_recognises_a_move_and_its_denial(self):
        moved = [
            'The organizations are already listed, moved from `claude-api-credits` in the configuration.',
            "Quota Cache moves the configuration's `claude-api-credits` list again.",
            'Organizations in the configuration are migrated to the page once.',
        ]
        denied = [
            'It lists only the organizations added on its page, and nothing moves there from the configuration.',
            '0.2.0 reads none of `claude-api-credits` and moves nothing.',
            'Moving `cache-path` still requires a native restart.',
            "The rule that makes a refused request move to the next key is CPA's configuration.",
        ]
        self.assertEqual([text for text in moved if not any(map(says_organizations_move_from_configuration, clauses(text)))], [])
        self.assertEqual([text for text in denied if any(map(says_organizations_move_from_configuration, clauses(text)))], [])

    # The guide repeats the update steps from Quota Cache's README, for an
    # operator who starts from either. A step changed in one alone, as when
    # the organizations stopped moving over from the configuration, sends
    # that operator down a path that no longer exists.
    def test_gives_the_same_update_steps_as_quota_cache(self):
        steps = update_steps(GUIDE)
        self.assertTrue(steps, 'the guide lists no update steps any more, or the pattern no longer finds them')
        self.assertEqual(steps, update_steps(QUOTA_CACHE/'README.md'))

    # Until it is updated, Quota Glance 0.7.2 keeps its own editor. Quota
    # Cache never reads what it saves. And while only some organizations are
    # added on Quota Cache's page, 0.7.2 offers Remove for the saved values of
    # the others: saving that deletes their only copy before they are entered
    # again. So wherever the update steps are given, they say to save nothing
    # in Quota Glance's editor until it is updated.
    def test_says_to_save_nothing_in_quota_glance_07_until_it_is_updated(self):
        documents = [path for path in operator_docs() if update_steps(path)]
        self.assertTrue(documents, 'no document gives the update steps any more, or the pattern no longer finds them')
        unwarned = [
            str(path.relative_to(ROOT)) for path in documents
            if not any(
                says_to_save_nothing_in_quota_glance_07(sentence)
                for line in update_section(path).split('\n') for sentence in SENTENCE_BREAK.split(line))
        ]
        self.assertEqual(unwarned, [])

    def test_recognises_saving_nothing_in_quota_glance_07(self):
        named = [
            "Until Quota Glance is updated, save nothing in its editor: Quota Cache doesn't read those saves.",
            "Save nothing in Quota Glance's editor until Quota Glance is updated.",
        ]
        unnamed = [
            "Quota Glance 0.7.2 offers **Remove** for the others' saved values.",
            'Until Quota Glance is updated, its editor stays open.',
            "Save nothing in Quota Cache's editor while CPA is stopped.",
        ]
        self.assertEqual([text for text in named if not says_to_save_nothing_in_quota_glance_07(text)], [])
        self.assertEqual([text for text in unnamed if says_to_save_nothing_in_quota_glance_07(text)], [])

    # The test above finds the steps by their numbers. Quota Glance's README
    # once gave the same order in a sentence of prose, without the caution,
    # and nothing saw it. An operator who starts there adds a first
    # organization, finds Remove beside the others' saved values in Quota
    # Glance 0.7.2's editor, and saves away the only copy of them. So
    # wherever a document puts the updates in order, numbered or not, the
    # same section says to save nothing in Quota Glance's editor until it is
    # updated.
    def test_says_to_save_nothing_in_quota_glance_07_wherever_the_updates_are_ordered(self):
        ordered = [
            (path, section) for path in operator_docs() for section in sections(path.read_text(encoding='utf-8'))
            if any(map(orders_the_updates, section))
        ]
        self.assertTrue(ordered, 'no document puts the updates in order any more, or the pattern no longer finds it')
        unwarned = [
            f'{path.relative_to(ROOT)}: {next(filter(orders_the_updates, section))[:80]}…' for path, section in ordered
            if not any(
                says_to_save_nothing_in_quota_glance_07(sentence)
                for paragraph in section for sentence in SENTENCE_BREAK.split(paragraph))
        ]
        self.assertEqual(unwarned, [])

    def test_recognises_the_updates_put_in_order(self):
        ordered = [
            '1. Update Quota Cache first.',
            '4. Then update Quota Glance, then Reset Priority.',
            "So add them once on Quota Cache's page: update Quota Cache first, add the organizations and their "
            'values there, then update Quota Glance.',
            "So they are entered again on Quota Cache's page before Quota Glance is updated.",
            'Add the organizations after updating Quota Cache.',
        ]
        unordered = [
            'If `route-api-credits` is on, turn it off before you update, disable or uninstall the plugin.',
            "Update Quota Cache to 0.1.14 and add this organization's organization-id.",
            '3. **Quota Cache** to 0.1.14 last.',
            'Quota Glance 0.8.0 and Reset Priority 0.2.0 read what Quota Cache 0.2.0 publishes.',
            'Claude API spend is not counted from the update until the first organization is added.',
            "After updating, add the organizations and enter the values again on this page, as the README lists.",
        ]
        self.assertEqual([text for text in ordered if not orders_the_updates(text)], [])
        self.assertEqual([text for text in unordered if orders_the_updates(text)], [])

    def test_reads_a_wrapped_sentence_whole(self):
        found = list(sections(
            '## Updating\n\nUpdate Quota Cache first. Until Quota\nGlance is updated, save nothing in its\neditor.\n\n'
            '## Next\n\nThen update Quota Glance.\n'))
        self.assertEqual(found[1], ['Update Quota Cache first. Until Quota Glance is updated, save nothing in its editor.'])
        self.assertEqual(found[2], ['Then update Quota Glance.'])
        self.assertTrue(says_to_save_nothing_in_quota_glance_07(SENTENCE_BREAK.split(found[1][0])[1]))

    # Quota Cache 0.2.0 runs no meter until the first organization is added
    # on its page. From then on its meter keeps the traffic of every
    # organization its file knows, added yet or not, so one added later loses
    # nothing more. Quota Cache's meter tests pin this in
    # TestOrganizationsAddedAfterTheFirstKeepTheirTrafficSinceIt. The guide
    # once said each organization's gap lasted until it was added, while
    # Quota Cache's own docs said otherwise. So wherever a paragraph describes
    # the gap across the update, it says the gap ends when the first
    # organization is added.
    def test_says_the_gap_across_the_update_ends_when_the_first_organization_is_added(self):
        paragraphs = [
            (path, line) for path in operator_docs() for line in path.read_text(encoding='utf-8').split('\n')
            if describes_the_gap_across_the_update(line)
        ]
        self.assertTrue(paragraphs, 'no document describes the gap across the update any more, or the pattern no longer finds it')
        unended = [f'{path.relative_to(ROOT)}: {line[:80]}…' for path, line in paragraphs if not FIRST_ADD.search(line)]
        self.assertEqual(unended, [])

    def test_recognises_the_gap_across_the_update_and_its_end(self):
        ended = [
            'Claude API spend is not counted from the update until the first organization is added. '
            'If the gap matters, take a Console reading.',
            'So after an update from 0.1.14 the gap ends when the first organization is added.',
            'Adding the first one starts counting again from the file, with a gap from the update until that add.',
        ]
        unended = [
            'Add the organizations right after updating. An organization added with the same Organization ID is '
            'the same account: counting carries on from the file, with a gap for the minutes in between.',
        ]
        other = [
            'A CPA restart therefore leaves a brief gap of a few seconds.',
            'A list left as it was before the update lacks an organization added on the page. '
            'An organization the list lacks is not counted by 0.1.14.',
        ]
        self.assertEqual([text for text in ended + unended if not describes_the_gap_across_the_update(text)], [])
        self.assertEqual([text for text in other if describes_the_gap_across_the_update(text)], [])
        self.assertEqual([text for text in ended if not FIRST_ADD.search(text)], [])
        self.assertEqual([text for text in unended if FIRST_ADD.search(text)], [])

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
