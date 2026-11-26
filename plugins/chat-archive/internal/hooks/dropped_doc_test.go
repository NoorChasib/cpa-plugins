package hooks

import (
	"os"
	"strings"
	"testing"
)

// TestReferenceGivesEveryDroppedNameAUnit: REFERENCE.md's table of dropped.* units names every
// counter the plugin reports, so a new counter cannot ship without its unit.
func TestReferenceGivesEveryDroppedNameAUnit(t *testing.T) {
	b, err := os.ReadFile("../../REFERENCE.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "| Unit | Counters |")
	if start < 0 {
		t.Fatal("REFERENCE.md has no table of dropped.* units")
	}
	end := strings.Index(doc[start:], "\n\n")
	table := doc[start : start+end]
	for _, n := range DroppedNames {
		if !strings.Contains(table, "`"+n+"`") {
			t.Errorf("dropped.%s has no unit in REFERENCE.md", n)
		}
	}
}
