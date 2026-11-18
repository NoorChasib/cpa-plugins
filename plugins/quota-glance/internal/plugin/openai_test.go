package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Quota Cache 0.6.0 publishes the OpenAI API credit in a credit state of its
// own beside the snapshot. These test that this plugin finds it there, shows
// it as its own card, and reports it in health beside the Claude state.

// openAIHealth is the parts of /health about the OpenAI API credit.
type openAIHealth struct {
	OpenAIMeter struct {
		Path      string    `json:"path"`
		FlushedAt time.Time `json:"flushed_at"`
		LastError string    `json:"last_error"`
	} `json:"openai_meter"`
	OpenAICreditState struct {
		Path              string     `json:"path"`
		LastError         string     `json:"last_error"`
		ComputedAt        *time.Time `json:"computed_at"`
		ValidUntil        *time.Time `json:"valid_until"`
		SettingsState     string     `json:"settings_state"`
		QuotaCacheVersion string     `json:"quota_cache_version"`
	} `json:"openai_credit_state"`
}

func openAIHealthOf(t *testing.T, p *Plugin) openAIHealth {
	t.Helper()
	var h openAIHealth
	if err := json.Unmarshal(managementGet(t, p, "/v0/management/plugins/quota-glance/health").Body, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// commitOpenAIState writes raw where Quota Cache publishes the OpenAI API
// credit state, as its publisher does: a new file renamed over the old.
func commitOpenAIState(t *testing.T, cachePath string, raw []byte) {
	t.Helper()
	temp := qc.OpenAICreditsPath(cachePath) + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, qc.OpenAICreditsPath(cachePath)); err != nil {
		t.Fatal(err)
	}
}

func openAICreditsOf(doc map[string]any) (map[string]any, bool) {
	credits, ok := doc["openaiApiCredits"].(map[string]any)
	return credits, ok
}

// Health reports the OpenAI meter and credit state as it reports the Claude
// ones: missing on an install that lists no OpenAI organization, which is no
// fault, then Quota Cache's figures once the state appears, and unreadable
// when it cannot be read. A write to the state alone rebuilds the document,
// and the Claude state's report never moves with it.
func TestHealthReportsTheOpenAIState(t *testing.T) {
	p, _, cachePath, _ := newPublishedPlugin(t, "")
	doc := waitForDocument(t, p, publishedDocument)
	if _, ok := doc["openaiApiCredits"]; ok {
		t.Fatalf("a Claude-only install has an OpenAI card: %v", doc["openaiApiCredits"])
	}
	claude, _ := healthOf(t, p)
	h := openAIHealthOf(t, p)
	if h.OpenAICreditState.Path != qc.OpenAICreditsPath(cachePath) || h.OpenAICreditState.LastError != "missing" || h.OpenAICreditState.ComputedAt != nil ||
		h.OpenAIMeter.Path != qc.OpenAIMeterPath(cachePath) || h.OpenAIMeter.LastError != "missing" || !h.OpenAIMeter.FlushedAt.IsZero() {
		t.Fatalf("no OpenAI state: %+v", h)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "quota-cache", "internal", "credits", "testdata", "golden", "degraded-states.openai-credits.json"))
	if err != nil {
		t.Fatal(err)
	}
	commitOpenAIState(t, cachePath, raw)
	doc = waitForDocument(t, p, func(doc map[string]any) bool {
		_, ok := openAICreditsOf(doc)
		return ok
	})
	credits, _ := openAICreditsOf(doc)
	if credits["provider"] != qc.ProviderOpenAIAPI || credits["editing"].(map[string]any)["reason"] != qc.CreditEditingQuotaCache {
		t.Fatalf("openaiApiCredits = %v", credits)
	}
	h = openAIHealthOf(t, p)
	computed, until := time.Date(2026, 9, 10, 4, 0, 0, 0, time.UTC), time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	if s := h.OpenAICreditState; s.LastError != "" || s.ComputedAt == nil || !s.ComputedAt.Equal(computed) || s.ValidUntil == nil ||
		!s.ValidUntil.Equal(until) || s.SettingsState != "ok" || s.QuotaCacheVersion != "0.6.0" {
		t.Fatalf("OpenAI credit state = %+v", s)
	}
	if m := h.OpenAIMeter; m.LastError != "" || !m.FlushedAt.Equal(time.Date(2026, 9, 10, 3, 59, 0, 0, time.UTC)) {
		t.Fatalf("OpenAI meter = %+v", m)
	}
	if now, _ := healthOf(t, p); !reflect.DeepEqual(now, claude) {
		t.Fatalf("the Claude state's report moved with the OpenAI state: %+v, was %+v", now, claude)
	}

	commitOpenAIState(t, cachePath, []byte(`{"schema":1,`))
	doc = waitForDocument(t, p, func(doc map[string]any) bool {
		_, ok := openAICreditsOf(doc)
		return !ok
	})
	if !publishedDocument(doc) {
		t.Fatal("an unreadable OpenAI state took the Claude card with it")
	}
	if h = openAIHealthOf(t, p); h.OpenAICreditState.LastError != "unreadable" || h.OpenAIMeter.LastError != "unreadable" {
		t.Fatalf("unreadable OpenAI state: %+v", h)
	}
}
