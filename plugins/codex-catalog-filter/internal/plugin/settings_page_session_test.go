package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSettingsPageReadsConsoleSessions runs scripts/tests/session.test.mjs:
// the shipped app.js, loaded unchanged into a stub page, reading remembered
// console sessions as the official Management Center stores them, enc::v2::
// from v1.27.0 on and enc::v1:: before. It needs Node.js 18 or later. A
// missing node skips it on a workstation and fails it in CI.
func TestSettingsPageReadsConsoleSessions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required in CI to run the page's session tests")
		}
		t.Skip("node not found: the page's session tests need Node.js 18 or later")
	}
	out, err := exec.Command(node, "--test", filepath.Join("..", "..", "scripts", "tests", "session.test.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test scripts/tests/session.test.mjs: %v\n%s", err, out)
	}
}
