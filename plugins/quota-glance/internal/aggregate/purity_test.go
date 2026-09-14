package aggregate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Build must take the instant as an argument and never read the clock itself.
// A single time.Now() in here would make the golden document unreproducible and
// every countdown in it untestable, and it would do so quietly.
func TestPackageNeverReadsTheClock(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "time" && (selector.Sel.Name == "Now" || selector.Sel.Name == "Since") {
				t.Errorf("%s calls time.%s; Build must take now as an argument", fset.Position(call.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
}

// Injecting a different clock must move every derived countdown with it.
func TestClockIsInjectedNotAmbient(t *testing.T) {
	first := buildFixture(t)
	later := Build(Input{
		Snapshot:   loadSnapshot(t, "seven-credentials.json"),
		Identities: fixtureRoster(),
		StaleAfter: 45 * time.Minute,
	}, at(t, 900))

	if later.GeneratedAtEpoch-first.GeneratedAtEpoch != 900 {
		t.Fatalf("generatedAt did not follow the injected clock")
	}
	a := rowOf(t, first, "claude", qc.WindowSession).Aggregate
	b := rowOf(t, later, "claude", qc.WindowSession).Aggregate
	if *a.SoonestResetInSeconds-*b.SoonestResetInSeconds != 900 {
		t.Fatalf("countdown did not follow the injected clock: %d then %d",
			*a.SoonestResetInSeconds, *b.SoonestResetInSeconds)
	}
	if *a.SoonestResetAtEpoch != *b.SoonestResetAtEpoch {
		t.Fatal("the reset instant itself must not move with the clock")
	}
	if b.Subtext != "+6% when siphorchannel resets in 1h" {
		t.Fatalf("subtext = %q", b.Subtext)
	}
}
