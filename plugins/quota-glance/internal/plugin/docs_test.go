package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
)

// operatorDocs are the files an operator follows to set quota-glance up, keyed
// by their path from the module root.
func operatorDocs(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	paths, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no docs/*.md")
	}
	paths = append(paths, filepath.Join(root, "README.md"), filepath.Join(root, "config.example.yaml"))
	docs := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		docs[filepath.ToSlash(rel)] = string(raw)
	}
	return docs
}

// prose is a document's text with YAML comment markers dropped and every run
// of whitespace made one space, so a sentence reads the same however it wraps.
func prose(name, text string) string {
	if strings.HasSuffix(name, ".yaml") {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimLeft(strings.TrimSpace(line), "#")
		}
		text = strings.Join(lines, "\n")
	}
	return strings.Join(strings.Fields(text), " ")
}

// The docs name CPA's form by the console's own labels, as Reset Priority's
// and Quota Cache's do: the plugin's Edit config button on CPA's Plugins page.
// Every mention of the button is that one instruction, so a menu path or a
// panel the console does not show cannot come back, and the README and the
// example configuration both give it. The label is matched as the console
// writes it, so "edit config.yaml" is not a mention.
func TestDocsNameThePluginsFormAsTheConsoleDoes(t *testing.T) {
	for name, text := range operatorDocs(t) {
		open := "on CPA's Plugins page, open quota-glance's **Edit config**"
		if strings.HasSuffix(name, ".yaml") {
			open = strings.ReplaceAll(open, "**", "")
		}
		text := prose(name, text)
		lower := strings.ToLower(text)
		instructions := strings.Count(lower, strings.ToLower(open))
		if buttons := strings.Count(text, "Edit config"); buttons != instructions {
			t.Errorf("%s names Edit config %d times, %d of them as %q", name, buttons, instructions, open)
		}
		if (name == "README.md" || name == "config.example.yaml") && instructions == 0 {
			t.Errorf("%s does not say %q", name, open)
		}
		for _, stale := range []string{"plugins →", "cpa panel", "configuration panel", "plugin's settings", "(the console's"} {
			if strings.Contains(lower, stale) {
				t.Errorf("%s names CPA's form as %q, which the console does not show", name, stale)
			}
		}
	}
}

// Routing on API credits is Reset Priority's to explain. These docs point to
// its README and say nothing of CPA's failover rule, which the console sets on
// each Claude API key entry and Reset Priority documents.
func TestDocsLeaveAPICreditRoutingToResetPriority(t *testing.T) {
	docs := operatorDocs(t)
	for name, text := range docs {
		text := strings.ToLower(prose(name, text))
		for _, routing := range []string{"request-scoped-errors", "request error rules", "credit balance is too low", "force-model-prefix"} {
			if strings.Contains(text, routing) {
				t.Errorf("%s explains API credit routing (%q); point to Reset Priority's README instead", name, routing)
			}
		}
	}
	if !strings.Contains(docs["README.md"], "](../reset-priority/README.md)") {
		t.Error("README.md does not point to Reset Priority's README for API credit routing")
	}
	if !strings.Contains(docs["docs/summary-contract.md"], "](../../reset-priority/README.md)") {
		t.Error("docs/summary-contract.md does not point to Reset Priority's README for API credit routing")
	}
}

// Quota Cache 0.2.0 lists only the organizations added on its page: at most
// 16, each with an Organization ID no other has. The degraded golden still
// carries the API credit values only Quota Cache 0.1.x's claude-api-credits
// gave rise to, so that it covers the whole contract. The contract names each
// of them where it says what 0.2.0 never publishes, so a client does not take
// them for states Quota Cache sends.
func TestContractSaysWhichDegradedCreditValuesQuotaCacheNeverPublishes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "summary-degraded.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc aggregate.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.APICredits == nil {
		t.Fatal("summary-degraded.json has no API credit card")
	}
	never := map[string]bool{}
	for _, account := range doc.APICredits.Accounts {
		switch {
		case strings.HasPrefix(account.ID, "item-"):
			never["`item-<n>`"] = true
		case strings.HasPrefix(account.ID, "label-"):
			never["Quota Cache 0.1.13"] = true
		}
		if account.State == qc.CreditStateMisconfigured || account.State == qc.CreditStateCacheTooOld {
			never["`"+account.State+"`"] = true
		}
		if reason := account.Settings.NotEditableReason; reason != qc.CreditEditingQuotaCache && reason != qc.CreditEditingSettingsUnreadable {
			never["`"+reason+"`"] = true
		}
	}
	for _, unlinked := range doc.APICredits.Unlinked {
		if unlinked.Reason != qc.CreditUnlinkedNotConfigured {
			never["`reason` is `"+unlinked.Reason+"`"] = true
		}
	}
	if len(never) == 0 {
		t.Fatal("summary-degraded.json carries no API credit value Quota Cache 0.2.0 never publishes; it no longer covers the contract")
	}
	const opening = "The organizations are the ones added on Quota Cache's page."
	said := ""
	for _, paragraph := range strings.Split(operatorDocs(t)["docs/summary-contract.md"], "\n\n") {
		if paragraph := prose("docs/summary-contract.md", paragraph); strings.HasPrefix(paragraph, opening) {
			said = paragraph
		}
	}
	if said == "" {
		t.Fatalf("docs/summary-contract.md has no paragraph beginning %q", opening)
	}
	for value := range never {
		if !strings.Contains(said, value) {
			t.Errorf("summary-degraded.json carries %s, which the contract does not name among what Quota Cache 0.2.0 never publishes", value)
		}
	}
	if !strings.Contains(said, "0.2.0 never publishes an item it could not link") {
		t.Error("docs/summary-contract.md does not say Quota Cache 0.2.0 never publishes an item it could not link")
	}
}

// Only Quota Cache 0.2.0's published state makes an account pending, and 0.2.0
// takes its organizations from its page alone. So the contract's pending rule
// explains a dormant organization as Quota Cache does, by when it was linked,
// and never sends the operator to a configuration it ignores.
func TestContractExplainsPendingByTheLinkNotTheConfiguration(t *testing.T) {
	const heading = "### `accounts[].state`"
	_, section, found := strings.Cut(operatorDocs(t)["docs/summary-contract.md"], "\n"+heading)
	if !found {
		t.Fatalf("docs/summary-contract.md has no %q section", heading)
	}
	section, _, _ = strings.Cut(section, "\n#")
	var rows []string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "| ") && strings.Contains(line, "| `"+qc.CreditStatePending+"` |") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("%s in docs/summary-contract.md has %d rows for %s, want 1", heading, len(rows), qc.CreditStatePending)
	}
	if row := strings.ToLower(rows[0]); strings.Contains(row, "config") || !strings.Contains(row, "linked") {
		t.Errorf("the contract's %s rule should say the meter has not saved since the organization was linked, not tie it to configuration: %s", qc.CreditStatePending, rows[0])
	}
}

// Quota Cache does not read what Quota Glance 0.7 saved, so the operator
// enters it again on Quota Cache's page. The README's paragraph on updating
// from 0.7 links to Quota Cache's own steps, whose heading is still there.
func TestReadmeLinksToQuotaCachesStepsForUpdatingFrom07(t *testing.T) {
	const section = "updating-from-quota-glance-07"
	found := false
	for _, paragraph := range strings.Split(operatorDocs(t)["README.md"], "\n\n") {
		if strings.HasPrefix(paragraph, "**Updating from Quota Glance 0.7.**") {
			found = true
			if !strings.Contains(prose("README.md", paragraph), "](../quota-cache/README.md#"+section+")") {
				t.Errorf("README.md's update from Quota Glance 0.7 does not link to Quota Cache's #%s", section)
			}
		}
	}
	if !found {
		t.Fatal("README.md has no paragraph on updating from Quota Glance 0.7")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		// The anchor GitHub gives a heading: lower case, spaces as hyphens,
		// and every other character that is not a letter or digit dropped.
		slug := strings.Map(func(r rune) rune {
			switch {
			case r == ' ':
				return '-'
			case r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r):
				return unicode.ToLower(r)
			}
			return -1
		}, strings.TrimSpace(strings.TrimLeft(line, "#")))
		if slug == section {
			return
		}
	}
	t.Errorf("Quota Cache's README.md has no heading for #%s", section)
}
