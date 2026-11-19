package aggregate

import (
	"slices"
	"strings"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// antigravitySnapshot is what quota-cache writes for two Antigravity accounts
// beside a Claude and a Grok one. "families" reports the default Gemini
// family as session/weekly and the Claude & GPT pool and Gemini Argon as
// per-model rows, with Argon's weekly untouched and so without a reset.
// "shared-only" reports the Claude & GPT pool alone: quota-cache publishes its
// windows with no entry-level observation, as for a Codex short-window-only
// account.
func antigravitySnapshot(now time.Time) qc.Snapshot {
	observed := now.Add(-5 * time.Minute)
	window := func(key, model, title string, used float64, reset time.Time) qc.EntryWindow {
		return qc.EntryWindow{Key: key, Model: model, Title: title, UsedPercent: used, ResetAt: reset, ObservedAt: observed}
	}
	return qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		qc.Key("claude", "claude-a@example.com.json"): {
			Provider: "claude", AuthIndex: "claude-a@example.com.json", ObservedAt: observed,
			Windows: []qc.EntryWindow{
				window(qc.WindowSession, "", "Session", 20, now.Add(time.Hour)),
				window(qc.WindowWeekly, "", "Weekly", 30, now.Add(72*time.Hour)),
			},
		},
		qc.Key("xai", "xai-a@example.com.json"): {
			Provider: "xai", AuthIndex: "xai-a@example.com.json", ObservedAt: observed,
			Windows: []qc.EntryWindow{window(qc.WindowCredits, "", "Credits", 10, time.Time{})},
		},
		qc.Key(qc.ProviderAntigravity, "antigravity-families@example.com.json"): {
			Provider: qc.ProviderAntigravity, AuthIndex: "antigravity-families@example.com.json",
			Percent: 35, ResetAt: now.Add(100 * time.Hour), ObservedAt: observed, Plan: "Google AI Pro",
			Windows: []qc.EntryWindow{
				window(qc.WindowSession, "", "Session (Gemini)", 40, now.Add(2*time.Hour)),
				window(qc.WindowWeekly, "", "Weekly (Gemini)", 35, now.Add(100*time.Hour)),
				window(qc.WindowModelSession, qc.AntigravityModelShared, "Session (Claude & GPT)", 100, now.Add(45*time.Minute)),
				window(qc.WindowModelWeekly, qc.AntigravityModelArgon, "Weekly (Gemini Argon)", 0, time.Time{}),
				window(qc.WindowModelWeekly, qc.AntigravityModelShared, "Weekly (Claude & GPT)", 72, now.Add(80*time.Hour)),
			},
		},
		qc.Key(qc.ProviderAntigravity, "antigravity-shared-only@example.com.json"): {
			Provider: qc.ProviderAntigravity, AuthIndex: "antigravity-shared-only@example.com.json",
			// No Gemini weekly, so no entry-level observation.
			Windows: []qc.EntryWindow{
				window(qc.WindowModelSession, qc.AntigravityModelShared, "Session (Claude & GPT)", 10, now.Add(3*time.Hour)),
				window(qc.WindowModelWeekly, qc.AntigravityModelShared, "Weekly (Claude & GPT)", 50, now.Add(90*time.Hour)),
			},
		},
	}}
}

func antigravityRoster() []Identity {
	return []Identity{
		{AuthIndex: "antigravity-shared-only@example.com.json", Provider: qc.ProviderAntigravity},
		{AuthIndex: "xai-a@example.com.json", Provider: "xai"},
		{AuthIndex: "antigravity-families@example.com.json", Provider: qc.ProviderAntigravity},
		{AuthIndex: "claude-a@example.com.json", Provider: "claude"},
	}
}

func providerNamed(t *testing.T, doc Document, id string) Provider {
	t.Helper()
	for _, p := range doc.Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no provider %s", id)
	return Provider{}
}

// Antigravity gets its own section after xAI, titled by product, and each of
// its quota families is a row of its own, titled as quota-cache titled it.
// Its Gemini session and weekly share the canonical keys Claude's use, but
// rows are built per provider, so no Claude credential ever lands in them.
func TestAntigravityRendersEachQuotaFamilyAsItsOwnRow(t *testing.T) {
	now := at(t, 0)
	doc := Build(Input{Snapshot: antigravitySnapshot(now), Identities: antigravityRoster(), StaleAfter: time.Hour}, now)

	var order []string
	for _, p := range doc.Providers {
		order = append(order, p.ID)
	}
	if want := []string{"claude", "xai", "antigravity"}; !slices.Equal(order, want) {
		t.Fatalf("providers = %v; want %v", order, want)
	}
	section := providerNamed(t, doc, qc.ProviderAntigravity)
	if section.Title != "Antigravity" || section.Order != 40 || section.CredentialCount != 2 {
		t.Fatalf("section = %q order %d with %d credentials", section.Title, section.Order, section.CredentialCount)
	}

	type shape struct{ rowID, title, model string }
	var rows []shape
	for _, row := range section.Rows {
		model := ""
		for _, entry := range row.Entries {
			if entry.SourceModel != nil {
				model = *entry.SourceModel
			}
			if !strings.HasPrefix(entry.CredentialID, "antigravity-") {
				t.Errorf("%s lists %s", row.RowID, entry.CredentialID)
			}
		}
		if !row.Matched {
			t.Errorf("%s is unmatched", row.RowID)
		}
		// The menu bar splits "Provider · Row" on its separator.
		if strings.Contains(row.Title, " · ") {
			t.Errorf("row title %q carries the menu bar's separator", row.Title)
		}
		rows = append(rows, shape{row.RowID, row.Title, model})
	}
	want := []shape{
		{"session", "Session (Gemini)", ""},
		{"weekly", "Weekly (Gemini)", ""},
		{"model_session:shared", "Session (Claude & GPT)", "shared"},
		{"model_weekly:argon", "Weekly (Gemini Argon)", "argon"},
		{"model_weekly:shared", "Weekly (Claude & GPT)", "shared"},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows = %+v\nwant %+v", rows, want)
	}
	for _, row := range providerNamed(t, doc, "claude").Rows {
		for _, entry := range row.Entries {
			if entry.CredentialID != "claude-a@example.com.json" {
				t.Errorf("Claude's %s lists %s", row.RowID, entry.CredentialID)
			}
		}
	}

	// The account with a Gemini weekly sorts on its reset; the one without
	// has nothing to sort on and goes last.
	var credentials []string
	for _, c := range doc.Credentials {
		if c.Provider == qc.ProviderAntigravity {
			credentials = append(credentials, c.ID)
		}
	}
	if want := []string{"antigravity-families@example.com.json", "antigravity-shared-only@example.com.json"}; !slices.Equal(credentials, want) {
		t.Fatalf("credentials = %v; want %v", credentials, want)
	}
	for _, c := range doc.Credentials {
		switch c.ID {
		case "antigravity-families@example.com.json":
			if c.Plan != "Google AI Pro" || c.Email != "families@example.com" || c.Status != StatusOK || c.Credits != nil || c.ResetCredits != nil {
				t.Errorf("families = %+v", c)
			}
		case "antigravity-shared-only@example.com.json":
			// Polled, not pending: its windows carry their own observation.
			if c.Status != StatusOK || c.LastObservedEpoch != now.Add(-5*time.Minute).Unix() {
				t.Errorf("shared-only = %+v", c)
			}
		}
	}
	if doc.Counters != (Counters{Credentials: 4, ObservedOK: 4}) {
		t.Fatalf("counters = %+v", doc.Counters)
	}
}

// Each family's window counts in its own row only, and the shared-only
// account, which reports no Gemini window, is listed there with no reading
// rather than counted as a full one.
func TestAntigravityFamiliesPoolSeparately(t *testing.T) {
	now := at(t, 0)
	doc := Build(Input{Snapshot: antigravitySnapshot(now), Identities: antigravityRoster(), StaleAfter: time.Hour}, now)

	weekly := rowOf(t, doc, qc.ProviderAntigravity, qc.WindowWeekly)
	if weekly.Aggregate.MemberCount != 1 || weekly.Aggregate.RemainingPercent != 65 {
		t.Fatalf("Gemini weekly = %+v", weekly.Aggregate)
	}
	for _, entry := range weekly.Entries {
		if entry.CredentialID == "antigravity-shared-only@example.com.json" && entry.HasReading {
			t.Fatalf("an account with no Gemini weekly has a reading on it: %+v", entry)
		}
	}
	shared := rowOf(t, doc, qc.ProviderAntigravity, "model_weekly:shared")
	if shared.Aggregate.MemberCount != 2 || shared.Aggregate.RemainingPercent != 39 {
		t.Fatalf("Claude & GPT weekly = %+v; want both accounts, (28+50)/2 = 39%%", shared.Aggregate)
	}
	session := rowOf(t, doc, qc.ProviderAntigravity, "model_session:shared")
	if session.Aggregate.MemberCount != 2 || session.Aggregate.RemainingPercent != 45 {
		t.Fatalf("Claude & GPT session = %+v; want (0+90)/2 = 45%%", session.Aggregate)
	}

	// An untouched bucket records no reset: the entry says none rather than a
	// countdown that would never arrive.
	argon := rowOf(t, doc, qc.ProviderAntigravity, "model_weekly:argon")
	if argon.Aggregate.SoonestResetAtEpoch != nil || len(argon.Entries) != 2 {
		t.Fatalf("Gemini Argon weekly = %+v with %d entries", argon.Aggregate, len(argon.Entries))
	}
	for _, entry := range argon.Entries {
		if entry.HasReading && (entry.ResetAtEpoch != nil || entry.ResetDisplayHint != HintNone || entry.RemainingPercent != 100) {
			t.Fatalf("untouched Argon entry = %+v", entry)
		}
	}
}

// The weekly gate is Claude's: Antigravity's spent Gemini weekly holds
// nothing out of its session row, and its session row gets no forecast.
func TestTheClaudeWeeklyGateDoesNotApplyToAntigravity(t *testing.T) {
	now := at(t, 0)
	observed := now.Add(-time.Minute)
	snapshot := qc.Snapshot{Schema: 1, ProviderCooldown: map[string]time.Time{}, Entries: map[string]qc.Entry{
		qc.Key(qc.ProviderAntigravity, "antigravity-spent@example.com.json"): {
			Provider: qc.ProviderAntigravity, AuthIndex: "antigravity-spent@example.com.json",
			Percent: 100, ResetAt: now.Add(48 * time.Hour), ObservedAt: observed,
			Windows: []qc.EntryWindow{
				{Key: qc.WindowSession, Title: "Session (Gemini)", UsedPercent: 10, ResetAt: now.Add(time.Hour), ObservedAt: observed},
				{Key: qc.WindowWeekly, Title: "Weekly (Gemini)", UsedPercent: 100, ResetAt: now.Add(48 * time.Hour), ObservedAt: observed},
			},
		},
	}}
	doc := Build(Input{Snapshot: snapshot,
		Identities: []Identity{{AuthIndex: "antigravity-spent@example.com.json", Provider: qc.ProviderAntigravity}},
		StaleAfter: time.Hour}, now)
	session := rowOf(t, doc, qc.ProviderAntigravity, qc.WindowSession)
	if session.Aggregate.MemberCount != 1 || session.Aggregate.HeldOutCount != 0 || session.Entries[0].HeldOut {
		t.Fatalf("session = %+v", session.Aggregate)
	}
	if session.Aggregate.RemainingPercent != 90 {
		t.Fatalf("session reads %d%%; want its own 90%%", session.Aggregate.RemainingPercent)
	}
}
