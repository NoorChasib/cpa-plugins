import os
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]

# A citation of the design notes kept beside the repository but never
# committed: a section sign, their file names, their test IDs, their sections
# (a capital letter, a dot and a number), and the panels of a design board.
# No reader can open them, so comments, test names and docs describe the
# behaviour instead. Quota Glance and Reset Priority also check their own
# modules from go test; this covers the whole repository, including the root
# scripts and workflows and the plugins without a check of their own.
UNCOMMITTED_CITATION = re.compile(
    r'\u00a7|api-credits-[a-z0-9-]*\.md|'
    r'\b(?:QC-[A-Z]?|RP-?|G-T)[0-9]+\b|\bQC-[a-z]\b|\b[A-P]\.[0-9]+\b|\bboard [A-Z][0-9]+\b|\(L[0-9]+\)')

# A section of an RFC, which anyone can look up.
RFC_SECTION = re.compile(r'RFC [0-9]+ \u00a7[0-9.]+')

TEXT = {'.go', '.md', '.yaml', '.yml', '.json', '.sh', '.py', '.h', '.swift',
        '.ts', '.tsx', '.js', '.mjs', '.cjs', '.css', '.html'}

# Build output, installed packages, and Quota Glance's web/design, which holds
# mockups rather than code or docs.
SKIP_DIRS = {'dist', 'node_modules', '__pycache__'}
SKIP_PATHS = {Path('plugins/quota-glance/web/design')}

# The lettered parts the notes had when the section pattern was last widened.
# Each amendment to them adds a part, headed by its capital letter and a dot.
NOTES_PARTS = set('ABCDEFGHIJKLMNOP')
PART_HEADING = re.compile(r'^#+ ([A-Z])\. ', re.MULTILINE)


def notes(path):
    """The uncommitted notes themselves, which sit in docs/ untracked."""
    return path.parent == Path('docs') and path.name.startswith('api-credits-') and path.suffix == '.md'


def notes_parts():
    """The lettered parts of the notes, where they are present."""
    found = set(NOTES_PARTS)
    for path in sorted((ROOT/'docs').glob('*.md')):
        if notes(path.relative_to(ROOT)):
            found.update(PART_HEADING.findall(path.read_text(encoding='utf-8', errors='replace')))
    return found


def citations():
    found = []
    for directory, subdirectories, files in os.walk(ROOT):
        relative = Path(directory).relative_to(ROOT)
        subdirectories[:] = sorted(
            name for name in subdirectories
            if not (name.startswith('.') and name != '.github')
            and name not in SKIP_DIRS and relative/name not in SKIP_PATHS)
        for name in sorted(files):
            path = relative/name
            if Path(name).suffix not in TEXT and name != 'Makefile' or notes(path):
                continue
            lines = (ROOT/path).read_text(encoding='utf-8', errors='replace').split('\n')
            for number, line in enumerate(lines, 1):
                match = UNCOMMITTED_CITATION.search(RFC_SECTION.sub('', line))
                if match:
                    found.append(f'{path}:{number} cites {match.group(0)!r}, which no reader can look up')
    return found


class CitationTests(unittest.TestCase):
    def test_no_citations_of_uncommitted_notes(self):
        self.assertEqual(citations(), [])

    # A part the section pattern stops short of can be cited unnoticed. Where
    # the notes are present this also checks the parts added since.
    def test_catches_a_section_of_every_part_of_the_notes(self):
        missed = sorted(part for part in notes_parts()
                        if not UNCOMMITTED_CITATION.search(f'// {part}.4.1: as decided there'))
        self.assertEqual(missed, [])


if __name__ == '__main__':
    unittest.main()
