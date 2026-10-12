package main

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// uncommittedCitation matches a citation of the design notes kept beside the
// repository but never committed: a section sign, their file names, their
// test IDs, their sections (a capital letter, a dot and a number), and the
// panels of a design board. No reader of the module can open them, so
// comments, test names and docs describe the behaviour instead. It is the
// pattern scripts/tests/test_citations.py checks the whole repository with;
// keep the two the same, so this module's own gate fails where that one would.
var uncommittedCitation = regexp.MustCompile(`\x{00A7}|api-credits-[a-z0-9-]*\.md|` +
	`\b(?:QC-[A-Z]?|RP-?|G-T)[0-9]+\b|\bQC-[a-z]\b|\b[A-N]\.[0-9]+\b|\bboard [A-Z][0-9]+\b|\(L[0-9]+\)`)

// rfcSection is a section of an RFC, which anyone can look up.
var rfcSection = regexp.MustCompile(`RFC [0-9]+ \x{00A7}[0-9.]+`)

// notesParts are the lettered parts the notes had when the section pattern was
// last widened. Each amendment to them adds a part, headed by its capital
// letter and a dot.
const notesParts = "ABCDEFGHIJKLMN"

var partHeading = regexp.MustCompile(`(?m)^#+ ([A-Z])\. `)

// A part the section pattern stops short of can be cited unnoticed. Where the
// notes are present beside the repository this also checks the parts added
// since.
func TestCatchesASectionOfEveryPartOfTheNotes(t *testing.T) {
	parts := map[string]bool{}
	for _, part := range notesParts {
		parts[string(part)] = true
	}
	notes, err := filepath.Glob(filepath.Join("..", "..", "docs", "api-credits-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range notes {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, heading := range partHeading.FindAllStringSubmatch(string(raw), -1) {
			parts[heading[1]] = true
		}
	}
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		if example := "// " + part + ".4.1: as decided there"; !uncommittedCitation.MatchString(example) {
			t.Errorf("the section pattern misses part %s of the notes: %q passes", part, example)
		}
	}
}

func TestNoCitationsOfUncommittedNotes(t *testing.T) {
	text := map[string]bool{
		".go": true, ".md": true, ".yaml": true, ".yml": true, ".json": true, ".sh": true, ".py": true,
		".ts": true, ".tsx": true, ".js": true, ".mjs": true, ".css": true, ".html": true,
	}
	// Build output, installed packages, and web/design, which holds mockups
	// rather than code or docs.
	skip := map[string]bool{"dist": true, "node_modules": true, filepath.Join("web", "design"): true}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != "." && (skip[name] || skip[path] || strings.HasPrefix(name, ".") && name != ".github") {
				return filepath.SkipDir
			}
			return nil
		}
		if !text[filepath.Ext(name)] && name != "Makefile" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if match := uncommittedCitation.FindString(rfcSection.ReplaceAllString(line, "")); match != "" {
				t.Errorf("%s:%d cites %q, which no reader can look up", path, i+1, match)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
