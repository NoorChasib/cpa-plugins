package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source"
	"gopkg.in/yaml.v3"
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

// readmeMoveFromQuotaGlance is the README's paragraph on moving from Quota
// Glance 0.7, as prose.
func readmeMoveFromQuotaGlance(t *testing.T) string {
	t.Helper()
	for _, paragraph := range strings.Split(operatorDocs(t)["README.md"], "\n\n") {
		if strings.HasPrefix(paragraph, "**Moving from Quota Glance 0.7.**") {
			return prose("README.md", paragraph)
		}
	}
	t.Fatal("README.md has no paragraph on moving from Quota Glance 0.7")
	return ""
}

// Quota Cache imports Quota Glance 0.7's settings.json only from where it
// looks, since it cannot see this plugin's data-dir. The README's move from
// 0.7 says so: Quota Cache finds the file by itself with data-dir unset or as
// the configuration it shows, and for any other data-dir the README names the
// drop-in and links to Quota Cache's own steps, whose heading is still there.
func TestReadmeMoveFromQuotaGlanceMatchesWhereQuotaCacheLooks(t *testing.T) {
	readme := operatorDocs(t)["README.md"]
	_, block, ok := strings.Cut(readme, "```yaml\n")
	block, _, closed := strings.Cut(block, "```")
	if !ok || !closed {
		t.Fatal("README.md has no yaml block")
	}
	var shown struct {
		Plugins struct {
			Configs map[string]struct {
				CachePath string `yaml:"cache-path"`
				DataDir   string `yaml:"data-dir"`
			} `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(block), &shown); err != nil {
		t.Fatal(err)
	}
	cfg := shown.Plugins.Configs["quota-glance"]
	if cfg.CachePath == "" || cfg.DataDir == "" {
		t.Fatalf("README.md's configuration = %+v", cfg)
	}
	unset, err := filepath.Abs(filepath.Join(defaultDataDir, source.LegacySettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	candidates := qc.LegacySettingsCandidates(cfg.CachePath)
	for _, legacy := range []string{filepath.Join(cfg.DataDir, source.LegacySettingsFile), unset} {
		if !slices.Contains(candidates, legacy) {
			t.Errorf("Quota Cache does not look for %s beside README.md's cache-path, only %v", legacy, candidates)
		}
	}

	move := readmeMoveFromQuotaGlance(t)
	const section = "moving-from-quota-glance"
	for _, want := range []string{"`" + qc.LegacySettingsDropIn + "`", "](../quota-cache/README.md#" + section + ")"} {
		if !strings.Contains(move, want) {
			t.Errorf("README.md's move from Quota Glance 0.7 does not say %q", want)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if strings.HasPrefix(line, "#") && strings.ReplaceAll(strings.ToLower(heading), " ", "-") == section {
			found = true
		}
	}
	if !found {
		t.Errorf("Quota Cache's README.md has no heading for #%s", section)
	}
}

// The migration note rides on the API credit card, and there is no card while
// Quota Cache lists no organization, as for an operator who saved only renewal
// dates in Quota Glance 0.7. The note is then only in /health, so the README's
// move from 0.7 says to look there rather than promising the card says so.
func TestReadmeSaysWhereTheMigrationNoteIsWithoutACard(t *testing.T) {
	p, _, cachePath, dataDir := newPublishedPlugin(t, "")
	waitForDocument(t, p, publishedDocument)
	const legacy = `{"schema":1,"revision":12,"apiCredits":{},"renewals":{}}`
	if err := os.WriteFile(filepath.Join(dataDir, source.LegacySettingsFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	editState(t, cachePath, func(state map[string]any) { state["api_credits"] = nil })
	p.Rebuild()
	res := summary(t, p)
	if _, ok := creditsOf(decode(t, res)); ok {
		t.Fatal("a state listing no organization still has a card")
	}
	if strings.Contains(string(res.Body), `"notImported"`) {
		t.Fatal("the document carries the migration note without a card; the README can point to it")
	}
	if h, _ := healthOf(t, p); h.Settings.Migration != "notImported" {
		t.Fatalf("health's migration = %q", h.Settings.Migration)
	}
	move := readmeMoveFromQuotaGlance(t)
	for _, want := range []string{"`settings.migration`", "`.../health`"} {
		if !strings.Contains(move, want) {
			t.Errorf("README.md's move from Quota Glance 0.7 does not say %q, where the note is without a card", want)
		}
	}
}
