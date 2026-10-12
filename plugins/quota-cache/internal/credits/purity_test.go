package credits

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Compute must take the instant as an input and never read the clock itself.
// A single time.Now() in here would make the golden states unreproducible and
// every countdown in them untestable, and it would do so quietly.
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
				t.Errorf("%s calls time.%s; Compute must take now as an input", fset.Position(call.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
}

// Injecting a different clock must move every derived countdown with it.
func TestClockIsInjectedNotAmbient(t *testing.T) {
	in := inputOf(t, fixtureNamed(t, "seven-credentials"))
	first := Compute(in)
	in.Now = in.Now.Add(900 * time.Second)
	later := Compute(in)

	if later.ComputedAt.Sub(first.ComputedAt) != 900*time.Second {
		t.Fatalf("computed_at did not follow the injected clock")
	}
	a, b := first.APICredits.Pool, later.APICredits.Pool
	if a.NextRefill.RefillInSeconds-b.NextRefill.RefillInSeconds != 900 || *a.FullInSeconds-*b.FullInSeconds != 900 {
		t.Fatalf("countdown did not follow the injected clock: %d then %d", a.NextRefill.RefillInSeconds, b.NextRefill.RefillInSeconds)
	}
	if a.NextRefill.RefillAtEpoch != b.NextRefill.RefillAtEpoch || *a.FullAtEpoch != *b.FullAtEpoch {
		t.Fatal("the refill instant itself must not move with the clock")
	}
	if !first.ValidUntil.Equal(*later.ValidUntil) {
		t.Fatalf("valid_until moved with the clock: %s then %s", first.ValidUntil, later.ValidUntil)
	}
}
