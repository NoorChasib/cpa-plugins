package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWarnBelowIsANumberInTheConfigurationPanel(t *testing.T) {
	for _, field := range registration().Metadata.ConfigFields {
		if field.Name == "openrouter-warn-below" {
			if field.Type != "number" {
				t.Fatalf("type = %q; the panel should offer a number field", field.Type)
			}
			return
		}
	}
	t.Fatal("openrouter-warn-below is not offered in the configuration panel")
}

func warnBelowConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(cachePath, fixtureSnapshot(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return "cache-path: " + cachePath + "\ndata-dir: " + filepath.Join(dir, "data") + "\nweb-token: test-token\n"
}

// The panel saves a number field as an int or a float depending on what was
// typed, and clears it by saving it empty; a hand-edited file may quote the
// value or keep the dollar sign. All of those are amounts. Absent or cleared is
// the default, and zero is a real choice.
func TestWarnBelowAcceptsWhatThePanelAndAFileSave(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       float64
	}{
		{"absent", "", 5},
		{"integer", "openrouter-warn-below: 10\n", 10},
		{"float", "openrouter-warn-below: 2.5\n", 2.5},
		{"zero", "openrouter-warn-below: 0\n", 0},
		{"quoted", "openrouter-warn-below: \"7\"\n", 7},
		{"dollar sign", "openrouter-warn-below: \"$3.50\"\n", 3.5},
		{"cleared", "openrouter-warn-below:\n", 5},
		{"null", "openrouter-warn-below: null\n", 5},
		{"empty string", "openrouter-warn-below: \"\"\n", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(&fakeHost{})
			t.Cleanup(p.Shutdown)
			if _, err := configure(t, p, warnBelowConfig(t)+tc.line); err != nil {
				t.Fatal(err)
			}
			p.configMu.RLock()
			got := p.settings.warnBelow
			p.configMu.RUnlock()
			if got != tc.want {
				t.Fatalf("warnBelow = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestWarnBelowRefusesWhatIsNotAnAmount(t *testing.T) {
	for _, line := range []string{
		"openrouter-warn-below: -1\n",
		"openrouter-warn-below: five\n",
		"openrouter-warn-below: .nan\n",
		"openrouter-warn-below: [5]\n",
		"openrouter-warn-below: {amount: 5}\n",
	} {
		p := New(&fakeHost{})
		t.Cleanup(p.Shutdown)
		_, err := configure(t, p, warnBelowConfig(t)+line)
		if err == nil || !strings.Contains(err.Error(), "openrouter-warn-below") {
			t.Errorf("%q: err = %v; want a refusal naming the field", strings.TrimSpace(line), err)
		}
	}
}

// End to end: the fixture's OpenRouter account holds $74.75, which is fine
// against the default and low against a threshold of $80.
func TestWarnBelowReachesTheServedDocument(t *testing.T) {
	p := New(&fakeHost{})
	t.Cleanup(p.Shutdown)
	if _, err := configure(t, p, warnBelowConfig(t)+"openrouter-warn-below: 80\n"); err != nil {
		t.Fatal(err)
	}
	doc := waitForDocument(t, p, func(doc map[string]any) bool {
		balances, _ := doc["balances"].([]any)
		return len(balances) == 1
	})
	balance := doc["balances"].([]any)[0].(map[string]any)
	if balance["level"] != "low" || balance["warnBelow"] != 80.0 || balance["remainingText"] != "$74.75" {
		t.Fatalf("balance = %v", balance)
	}
}
