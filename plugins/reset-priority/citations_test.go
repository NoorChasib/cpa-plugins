package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// uncommittedCitation matches a citation of the design notes kept beside the
// repository but never committed: a section sign, their file names and their
// test IDs. No reader of the module can open them, so comments and docs
// describe the behavior instead.
var uncommittedCitation = regexp.MustCompile(`\x{00A7}|api-credits-[a-z-]*spec\.md|\b(?:QC-[A-Z]?|RP-?)[0-9]+\b`)

func TestNoCitationsOfUncommittedNotes(t *testing.T) {
	eachTextLine(t, func(path string, n int, line string) {
		if match := uncommittedCitation.FindString(line); match != "" {
			t.Errorf("%s:%d cites %q, which no reader can look up", path, n, match)
		}
	})
}

// quotaCacheVersion matches a Quota Cache version named in prose or a
// comment, with or without bold and after "older than", "newer than",
// "since" or "before".
var quotaCacheVersion = regexp.MustCompile(`(?i)\bquota[- ]cache (?:(?:older|newer) than |since |before )?(?:\*\*)?v?([0-9]+\.[0-9]+\.[0-9]+)\b`)

// Every Quota Cache version the module names is one an operator can find:
// one that shipped, at or below the repository catalog's, or the one Quota
// Cache builds now. A working version that never shipped, named as a release,
// sends them looking for it.
func TestQuotaCacheVersionsNamedShipped(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Plugins []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	released := ""
	for _, plugin := range catalog.Plugins {
		if plugin.ID == "quota-cache" {
			released = plugin.Version
		}
	}
	raw, err = os.ReadFile(filepath.Join("..", "quota-cache", "internal", "plugin", "plugin.go"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^var Version = "([^"]+)"$`).FindSubmatch(raw)
	if released == "" || match == nil {
		t.Fatalf("Quota Cache's released version %q or the version it builds could not be read", released)
	}
	newest, building := semver(t, released), semver(t, string(match[1]))
	named := 0
	eachTextLine(t, func(path string, n int, line string) {
		for _, found := range quotaCacheVersion.FindAllStringSubmatch(line, -1) {
			named++
			if v := semver(t, found[1]); v != building && !v.atMost(newest) {
				t.Errorf("%s:%d names Quota Cache %s, which neither shipped (the catalog has %s) nor is the version it builds (%s)", path, n, found[1], released, match[1])
			}
		}
	})
	if named == 0 {
		t.Fatal("the module names no Quota Cache version, so this test checks nothing")
	}
}

type version [3]int

func semver(t *testing.T, text string) version {
	t.Helper()
	var v version
	parts := strings.Split(text, ".")
	if len(parts) != len(v) {
		t.Fatalf("version %q is not major.minor.patch", text)
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			t.Fatalf("version %q: %v", text, err)
		}
		v[i] = n
	}
	return v
}

func (v version) atMost(w version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return true
}

// eachTextLine calls fn with every line of the module's text files, numbered
// from 1.
func eachTextLine(t *testing.T, fn func(path string, n int, line string)) {
	t.Helper()
	text := map[string]bool{".go": true, ".md": true, ".yaml": true, ".yml": true, ".json": true, ".sh": true, ".h": true}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			// dist and other dot directories hold build output.
			if path != "." && (name == "dist" || strings.HasPrefix(name, ".") && name != ".github") {
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
			fn(path, i+1, line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
