package plugin

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/agy"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/claudelist"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

type stateModel struct {
	catalog.Entry
	// Enabled is whether the model stays in Codex's catalog as CPA lists it.
	Enabled bool `json:"enabled"`
	// Switched is whether Enabled comes from an explicit switch.
	Switched bool `json:"switched"`
}

// stateResponse keeps every 0.1.1 field (the Codex client, at the top level)
// and adds one section per other client.
type stateResponse struct {
	CatalogPath string          `json:"catalog_path"`
	SeenAt      *time.Time      `json:"seen_at"`
	Saved       bool            `json:"saved"`
	NewModels   string          `json:"new_models"`
	Action      string          `json:"action"`
	Models      []stateModel    `json:"models"`
	Switches    map[string]bool `json:"switches"`

	Version     string           `json:"version"`
	Antigravity antigravityState `json:"antigravity"`
	ClaudeCode  claudeCodeState  `json:"claude_code"`
}

// clientState is common to the client sections.
type clientState struct {
	// ConfigKey is the top-level configuration key the page PATCHes.
	ConfigKey string `json:"config_key"`
	// Configured is whether that key is present in the configuration.
	Configured bool `json:"configured"`
	// Error is why the key's value was rejected, or "".
	Error string `json:"error"`
	// Using says which settings apply: config, last-good, or defaults.
	Using     string          `json:"using"`
	NewModels string          `json:"new_models"`
	Switches  map[string]bool `json:"switches"`
	// ModelsSource says where Models comes from: the client's own last list
	// ("client"), or, before the client has fetched, another client's
	// ("codex", "antigravity", "claude_code"); "" when no list is known.
	ModelsSource string `json:"models_source"`
	// Saved is whether the client lists are saved to disk.
	Saved bool `json:"saved"`
}

type antigravityState struct {
	clientState
	Path         string `json:"path"`
	DefaultModel string `json:"default_model"`
	TitleModel   string `json:"title_model"`
	// FetchedAt is when CPA last returned its list to the agy route.
	FetchedAt *time.Time `json:"fetched_at"`
	// ServedAt and ServedFrom ("cpa" or "last-good") describe the last answer.
	ServedAt    *time.Time `json:"served_at"`
	ServedFrom  string     `json:"served_from"`
	LastError   string     `json:"last_error"`
	LastErrorAt *time.Time `json:"last_error_at"`
	Models      []agyModel `json:"models"`
	// Preview is the list the route serves with the current settings, built
	// from CPA's last list; null before agy has fetched.
	Preview *agy.Preview `json:"preview"`
}

type agyModel struct {
	ID               string `json:"id"`
	DisplayName      string `json:"display_name"`
	Label            string `json:"label"`
	Description      string `json:"description,omitempty"`
	InputTokenLimit  int    `json:"input_token_limit,omitempty"`
	OutputTokenLimit int    `json:"output_token_limit,omitempty"`
	Enabled          bool   `json:"enabled"`
	Switched         bool   `json:"switched"`
	NonText          bool   `json:"non_text"`
	HiddenByAgy      bool   `json:"hidden_by_agy"`
	Builtin          bool   `json:"builtin"`
	BuiltinEnum      int    `json:"builtin_enum,omitempty"`
	Tokens           string `json:"tokens"`
	MaxTokens        int    `json:"max_tokens,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	ThinkingBudget   int    `json:"thinking_budget,omitempty"`
	InList           bool   `json:"in_list"`
	Position         int    `json:"position"`
	Slot             string `json:"slot"`
	Placeholder      int    `json:"placeholder,omitempty"`
	Skipped          bool   `json:"skipped"`
}

type claudeCodeState struct {
	clientState
	Filter bool `json:"filter"`
	// SeenAt is when Claude Code last fetched its list through CPA.
	SeenAt          *time.Time    `json:"seen_at"`
	Lists           uint64        `json:"lists"`
	Rewritten       uint64        `json:"rewritten"`
	InterceptErrors uint64        `json:"intercept_errors"`
	Models          []claudeModel `json:"models"`
}

type claudeModel struct {
	ID          string `json:"id"`
	CloakedID   string `json:"cloaked_id,omitempty"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	Switched    bool   `json:"switched"`
}

// serveState reports, per client, the last list CPA returned and how the
// current switches apply to it. The page saves switches through CPA's own
// PATCH /v0/management/plugins/codex-catalog-filter/config.
func (p *Plugin) serveState() protocol.ManagementResponse {
	settings := p.settings.Load()
	if settings == nil {
		return failure(http.StatusServiceUnavailable, "not_configured")
	}
	rules := settings.Rules
	state := stateResponse{
		CatalogPath: CatalogPath,
		Saved:       true,
		NewModels:   newModels(rules),
		Action:      string(rules.Action()),
		Models:      []stateModel{},
		Switches:    rules.Switches(),
		Version:     Version,
	}
	seen, seenSaved := p.seen.current()
	if seen != nil {
		at := seen.SeenAt.UTC()
		state.SeenAt, state.Saved = &at, seenSaved
		for _, entry := range seen.Models {
			_, switched := rules.Switched(entry.Slug)
			state.Models = append(state.Models, stateModel{Entry: entry, Enabled: rules.Allows(entry.Slug), Switched: switched})
		}
	}
	view := p.clients.view()
	state.Antigravity = antigravitySection(settings.Antigravity, view, seen)
	state.ClaudeCode = claudeCodeSection(settings.ClaudeCode, view, seen)
	state.ClaudeCode.InterceptErrors = p.interceptErrors.Load()
	body, err := json.Marshal(state)
	if err != nil {
		return failure(http.StatusInternalServerError, "encoding_failed")
	}
	return jsonResponse(http.StatusOK, body)
}

func newModels(rules *catalog.Rules) string {
	if rules.EnableNew() {
		return "enabled"
	}
	return "disabled"
}

func timePtr(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

func clientSection(key string, configured bool, err string, source config.Source, rules *catalog.Rules, saved bool) clientState {
	return clientState{
		ConfigKey: key, Configured: configured, Error: err, Using: string(source),
		NewModels: newModels(rules), Switches: rules.Switches(), Saved: saved,
	}
}

func antigravitySection(client config.Antigravity, view clientsView, seen *seenCatalog) antigravityState {
	out := antigravityState{
		clientState:  clientSection(config.AntigravityKey, client.Configured, client.Err, client.Source, client.Rules, view.saved),
		Path:         AgyPath,
		DefaultModel: client.DefaultModel,
		TitleModel:   client.TitleModel,
		ServedAt:     timePtr(view.agyServedAt),
		ServedFrom:   view.agyServedFrom,
		LastError:    view.agyError,
		LastErrorAt:  timePtr(view.agyErrorAt),
		Models:       []agyModel{},
	}
	sel := agySelection(client)
	var models []agy.Model
	switch {
	case view.gemini != nil:
		out.FetchedAt = timePtr(view.gemini.FetchedAt)
		out.ModelsSource, models = "client", view.gemini.Models
		result := agy.Build(models, sel)
		out.Preview = &result.Preview
	case seen != nil:
		out.ModelsSource = "codex"
		for _, entry := range seen.Models {
			models = append(models, agy.Model{ID: entry.Slug, DisplayName: entry.DisplayName})
		}
	case view.claude != nil:
		out.ModelsSource = "claude_code"
		for _, entry := range view.claude.Models {
			models = append(models, agy.Model{ID: entry.ID, DisplayName: entry.DisplayName})
		}
	}
	// By id, as the list agy receives is ordered; CPA's own order changes on
	// every configuration reload.
	models = slices.SortedFunc(slices.Values(models), func(a, b agy.Model) int { return strings.Compare(a.ID, b.ID) })
	positions := map[string]int{}
	entries := map[string]agy.Entry{}
	skipped := map[string]bool{}
	if out.Preview != nil {
		for i, entry := range out.Preview.Entries {
			positions[entry.ID], entries[entry.ID] = i+1, entry
		}
		for _, id := range out.Preview.Skipped {
			skipped[id] = true
		}
	}
	for _, m := range models {
		_, switched := client.Rules.Switched(m.ID)
		enum, builtin := agy.BuiltinEnum(m.ID)
		item := agyModel{
			ID: m.ID, DisplayName: m.DisplayName, Label: agy.Label(m), Description: m.Description,
			InputTokenLimit: m.InputTokenLimit, OutputTokenLimit: m.OutputTokenLimit,
			Enabled: sel.Selected(m), Switched: switched, NonText: agy.NonText(m), HiddenByAgy: agy.HiddenAsImage(m.ID),
			Builtin: builtin, BuiltinEnum: enum, Skipped: skipped[m.ID],
		}
		item.MaxTokens, item.MaxOutputTokens, item.Tokens = agy.Tokens(m)
		item.ThinkingBudget, _ = agy.ThinkingBudget(m.ID)
		if entry, ok := entries[m.ID]; ok {
			item.InList, item.Position, item.Slot, item.Placeholder, item.Label = true, positions[m.ID], entry.Slot, entry.Placeholder, entry.DisplayName
		}
		out.Models = append(out.Models, item)
	}
	return out
}

func claudeCodeSection(client config.ClaudeCode, view clientsView, seen *seenCatalog) claudeCodeState {
	out := claudeCodeState{
		clientState: clientSection(config.ClaudeCodeKey, client.Configured, client.Err, client.Source, client.Rules, view.saved),
		Filter:      client.Filter,
		Lists:       view.claudeLists,
		Rewritten:   view.claudeRewrite,
		Models:      []claudeModel{},
	}
	var entries []claudelist.Entry
	switch {
	case view.claude != nil:
		out.SeenAt = timePtr(view.claude.SeenAt)
		out.ModelsSource, entries = "client", view.claude.Models
	case seen != nil:
		out.ModelsSource = "codex"
		for _, entry := range seen.Models {
			entries = append(entries, claudelist.Entry{ID: entry.Slug, DisplayName: entry.DisplayName})
		}
	case view.gemini != nil:
		out.ModelsSource = "antigravity"
		for _, m := range view.gemini.Models {
			entries = append(entries, claudelist.Entry{ID: m.ID, DisplayName: m.DisplayName})
		}
	}
	for _, entry := range entries {
		_, switched := client.Rules.Switched(entry.ID)
		cloaked := entry.CloakedID
		if cloaked == "" && claudelist.Cloak(entry.ID) != entry.ID {
			cloaked = claudelist.Cloak(entry.ID)
		}
		out.Models = append(out.Models, claudeModel{
			ID: entry.ID, CloakedID: cloaked, DisplayName: entry.DisplayName,
			Enabled: client.Rules.Allows(entry.ID), Switched: switched,
		})
	}
	return out
}
