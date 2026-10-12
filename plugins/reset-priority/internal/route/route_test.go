// The tests live in package route so they can reach the test hooks, the
// exploration history and the decision over a decoded request, which the
// allocation budget is stated for.
package route

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

var update = flag.Bool("update", false, "rewrite testdata/status.json")

// fixture is Quota Cache's credit state and the meter beside it, as
// testdata/worked.json holds them for the worked example most tests here
// start from: six organizations, one of them out, and their keys.
type fixture struct {
	Now     time.Time      `json:"now"`
	Credits qc.CreditState `json:"credits"`
	Meter   qc.APIMeter    `json:"meter"`
	// Accounts is Credits.APICredits.Accounts, for a test to change before
	// it takes an input.
	Accounts []qc.CreditAccount `json:"-"`
}

// worked loads the worked example afresh, so a test may change it freely.
func worked(t testing.TB) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "worked.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Credits.Schema != qc.CreditsSchema || f.Credits.APICredits == nil {
		t.Fatalf("testdata/worked.json holds no credit state: %+v", f.Credits)
	}
	f.Accounts = f.Credits.APICredits.Accounts
	return f
}

func (f *fixture) input() Input { return f.inputAt(f.Now) }

// inputAt is the input of a rebuild at now, as the router loop makes it: the
// fixture's accounts as Quota Cache published them shortly before now (as
// long before as the fixture's state was before its build), seen through
// qc.CreditsAt.
func (f *fixture) inputAt(now time.Time) Input {
	state := f.Credits
	doc := *state.APICredits
	doc.Accounts = f.Accounts
	state.APICredits = &doc
	state.ComputedAt = now.Add(f.Credits.ComputedAt.Sub(f.Now))
	return Input{Now: now, Credits: qc.CreditsAt(&state, now, MaxMeterAge), Meter: &f.Meter}
}

// credits is a credit state listing accounts, published at now with the
// operator's settings readable.
func credits(now time.Time, accounts ...qc.CreditAccount) *qc.CreditState {
	return &qc.CreditState{
		Schema: qc.CreditsSchema, ComputedAt: now, Settings: qc.CreditSettingsState{State: qc.CreditSettingsOK, Editable: true},
		APICredits: &qc.CreditDocument{Accounts: accounts}, Renewals: map[string]qc.RenewalSetting{},
	}
}

// section is the text of a Markdown section titled title, at level 2 or 3, up
// to the next heading of the same or a higher level, and false when doc has
// no such heading. Reset Priority's docs may hold the router's sections at
// either level.
func section(doc, title string) (string, bool) {
	levels := []string{"\n## ", "\n### "}
	for depth, level := range levels {
		_, text, found := strings.Cut(doc, level+title+"\n")
		if !found {
			continue
		}
		end := len(text)
		for _, next := range levels[:depth+1] {
			if i := strings.Index(text, next); i >= 0 && i < end {
				end = i
			}
		}
		return text[:end], true
	}
	return "", false
}

func at(t testing.TB, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func ptr[T any](v T) *T { return &v }

// id is the worked example's auth ID for a key, by its last two hex digits.
func id(suffix string) string { return "claude:apikey:0000000000" + suffix }

// configIndex is each worked-example key's 0-based position in CPA's Claude
// API key list.
var configIndex = map[string]string{
	"0a": "0", "0b": "1", "0c": "2", "0d": "3", "0f": "4", "10": "5", "11": "6", "12": "7", "13": "8",
}

// all is every worked-example key, in the order CPA offers them: by ID.
var all = []string{"0a", "0b", "0c", "0d", "0f", "10", "11", "12", "13"}

// cpaRequest mirrors pluginapi.SchedulerPickRequest at CPA v8.0.23
// (sdk/pluginapi/types.go): no JSON tags, so json.Marshal writes the Go field
// names exactly as CPA's host does.
type cpaRequest struct {
	Plugin     map[string]any
	Provider   string
	Providers  []string
	Model      string
	Stream     bool
	Options    cpaOptions
	Candidates []cpaCandidate
}

type cpaOptions struct {
	Headers  map[string][]string
	Metadata map[string]any
}

type cpaCandidate struct {
	ID         string
	Provider   string
	Priority   int
	Status     string
	Attributes map[string]string
	Metadata   map[string]any
}

// configKey is a Claude API key from CPA's configuration, as CPA offers it,
// with the sensitive attributes already stripped.
func configKey(suffix string) cpaCandidate {
	return cpaCandidate{
		ID: id(suffix), Provider: "claude", Status: "active",
		Attributes: map[string]string{
			"auth_kind": "apikey", "source": "config:claude[0000000000" + suffix + "]", "config_index": configIndex[suffix],
		},
	}
}

// newRequest is a pick offering candidates. Its metadata carries what CPA
// writes into every messages pick that reaches a plugin scheduler: the
// requested model, service_tier ("auto" by default) and generate (a JSON
// bool, true by default), and the session-affinity keys pickNextMixedLegacy
// sets ("" for the model under a built-in selector).
func newRequest(sel string, candidates ...cpaCandidate) cpaRequest {
	req := cpaRequest{
		Plugin:   map[string]any{"Name": "reset-priority", "Version": "0.2.0"},
		Provider: "", Providers: []string{"claude"}, Model: "credits/claude-haiku-5-5", Stream: true,
		Options: cpaOptions{
			Headers: map[string][]string{"Content-Type": {"application/json"}},
			Metadata: map[string]any{
				"generate":                  true,
				"requested_model":           "credits/claude-haiku-5-5",
				"service_tier":              "auto",
				"session_affinity_provider": "mixed",
				"session_affinity_model":    "",
			},
		},
		Candidates: candidates,
	}
	if sel != "" {
		req.Options.Metadata["selected_auth_id"] = sel
		req.Options.Metadata["selected_auth_index"] = "0000000000000000"
	}
	return req
}

func (req cpaRequest) bytes(t testing.TB) []byte {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// request offers the worked-example keys named, after a failed attempt on
// sel ("" on a round's first pick).
func request(t testing.TB, sel string, suffixes ...string) []byte {
	t.Helper()
	candidates := make([]cpaCandidate, len(suffixes))
	for i, suffix := range suffixes {
		candidates[i] = configKey(suffix)
	}
	return newRequest(sel, candidates...).bytes(t)
}

// without is all but the keys named.
func without(drop ...string) []string {
	out := []string{}
	for _, suffix := range all {
		keep := true
		for _, d := range drop {
			keep = keep && d != suffix
		}
		if keep {
			out = append(out, suffix)
		}
	}
	return out
}

// routing is an enabled router publishing in's table.
func routing(t testing.TB, in Input) *Router {
	t.Helper()
	r := New()
	r.SetEnabled(true)
	r.Publish(Build(in), in.Now)
	return r
}

// expect picks once and holds the answer to want.
func expect(t testing.TB, r *Router, raw []byte, now time.Time, want Decision) {
	t.Helper()
	if got := r.Pick(raw, now); got != want {
		t.Fatalf("pick = {%q %s}; want {%q %s}", got.AuthID, got.Reason, want.AuthID, want.Reason)
	}
}

func routed(suffix string) Decision    { return Decision{AuthID: id(suffix), Reason: ReasonRouted} }
func explored(suffix string) Decision  { return Decision{AuthID: id(suffix), Reason: ReasonExplored} }
func rechecked(suffix string) Decision { return Decision{AuthID: id(suffix), Reason: ReasonRechecked} }
func decline(reason Reason) Decision   { return Decision{Reason: reason} }

// entryOf is a copy of the exploration entry for a worked-example key, and
// false when there is none.
func entryOf(r *Router, suffix string) (entry, bool) {
	r.explore.mu.Lock()
	defer r.explore.mu.Unlock()
	e, ok := r.explore.m[id(suffix)]
	if !ok {
		return entry{}, false
	}
	return *e, true
}
