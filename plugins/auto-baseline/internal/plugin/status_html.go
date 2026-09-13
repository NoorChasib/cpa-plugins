package plugin

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/engine"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/statefile"
)

// This file renders both browser views from one template:
//
//   - the UNAUTHENTICATED resource page (GET /v0/resource/plugins/<id>/status)
//     receives a redacted snapshot (see redactResourceStatus) and carries the
//     same-origin bootstrap script that swaps in the authenticated view when
//     the browser already holds a management session;
//   - the AUTHENTICATED management page (GET /v0/management/plugins/<id>/status/html)
//     receives the full sanitized snapshot plus the Promote now / dry-run
//     toggle / Clear pending / Refresh actions.
//
// Every dynamic value passes through html/template's contextual escaping.
// Timestamps are rendered in the configured display timezone as
// "Tue Sep 1 2026 - 6:25:36 PM PDT"; the exact RFC3339 UTC instant is kept
// on each <time> element and shown on hover. Session identifiers are counted
// but never rendered on either view.

const (
	displayDateLayout  = "Mon Jan 2 2006"
	displayClockLayout = "3:04:05 PM MST"
)

type statusPageData struct {
	Authenticated        bool
	Timezone             string
	GeneratedAt          pageTime
	Enabled              bool
	Stopped              bool
	Faulted              bool
	DryRun               bool
	DryRunAwaitingReload bool
	DryRunTarget         bool
	Warnings             []string
	LastError            string
	LastErrorAt          pageTime
	Config               engine.ConfigStatus
	ConfigRead           pageTime
	Backup               engine.BackupStatus
	State                engine.StateStatus
	StateSaved           pageTime
	Rules                engine.RulesStatus
	Entrypoints          string
	CPAVersion           string
	CodexCloakingWarning string
	Providers            []pageProvider
	Pending              []pageCandidate
	History              []pageHistory
	Counters             []pageCounter
}

type pageProvider struct {
	Name           string
	Provider       string
	Managed        bool
	MinVersion     string
	Effective      engine.EffectiveStatus
	Warnings       []string
	LastPromotion  *pageHistory
	NextWriteAfter pageTime
	AwaitingReload bool
	PendingCount   int
}

type pageCandidate struct {
	Provider       string
	ProviderName   string
	Version        string
	UserAgent      string
	PackageVersion string
	RuntimeVersion string
	OS             string
	Arch           string
	Observations   int
	NeedObs        int
	Sessions       int
	NeedSessions   int
	FirstSeen      pageTime
	LastSeen       pageTime
	QuorumMet      bool
	Blocked        string
}

type pageHistory struct {
	At        pageTime
	Provider  string
	From      string
	To        string
	Tuple     string
	Source    string
	DryRun    bool
	Forced    bool
	Awaiting  bool
	Confirmed bool
}

type pageCounter struct {
	Label string
	Value uint64
}

// pageTime carries one instant in the forms the template needs. A zero value
// (Set=false) renders as a placeholder dash.
type pageTime struct {
	Set      bool
	Date     string // "Tue Sep 1 2026"
	Clock    string // "6:25:36 PM PDT"
	Exact    string // RFC3339Nano UTC for datetime/title attributes
	Relative string // "in 2m" / "3h ago", relative to GeneratedAt
}

var statusTemplate = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Auto Baseline</title>
<style>
:root{
  --bg:#f6f7f9;--surface:#fff;--border:#e4e7ec;--text:#101828;--muted:#667085;--faint:#98a2b3;
  --ok-bg:#ecfdf3;--ok-fg:#067647;--ok-bd:#abefc6;
  --warn-bg:#fffaeb;--warn-fg:#b54708;--warn-bd:#fedf89;
  --err-bg:#fef3f2;--err-fg:#b42318;--err-bd:#fecdca;
  --info-bg:#eff8ff;--info-fg:#175cd3;--info-bd:#b2ddff;
  --neutral-bg:#f2f4f7;--neutral-fg:#344054;--neutral-bd:#e4e7ec;
  --accent:#2563eb;--accent-hover:#1d4ed8;--accent-fg:#fff;
  --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0c111d;--surface:#131a2b;--border:#1f2a3d;--text:#e6eaf2;--muted:#94a3b8;--faint:#64748b;
    --ok-bg:#052e21;--ok-fg:#5ee6a8;--ok-bd:#0f5c47;
    --warn-bg:#3a2a06;--warn-fg:#fcd34d;--warn-bd:#6b4d0b;
    --err-bg:#3b0f0f;--err-fg:#fca5a5;--err-bd:#7f1d1d;
    --info-bg:#0f2148;--info-fg:#93c5fd;--info-bd:#1e3a8a;
    --neutral-bg:#1a2436;--neutral-fg:#cbd5e1;--neutral-bd:#2c3a52;
    --accent:#3b82f6;--accent-hover:#60a5fa;
  }
}
*{box-sizing:border-box}
html,body{margin:0;background:var(--bg);color:var(--text)}
body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
.page{max-width:1280px;margin:0 auto;padding:28px 24px 48px}
.header{display:flex;flex-wrap:wrap;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:22px}
h1{font-size:22px;font-weight:650;letter-spacing:-.01em;margin:0 0 4px}
.subtitle{margin:0;color:var(--muted)}
.pills{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
.pill{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--neutral-bd);background:var(--neutral-bg);color:var(--neutral-fg);border-radius:999px;padding:2px 10px;font-size:12px;font-weight:500;line-height:18px;white-space:nowrap}
.pill::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor;opacity:.85}
.pill.ok{background:var(--ok-bg);color:var(--ok-fg);border-color:var(--ok-bd)}
.pill.warn{background:var(--warn-bg);color:var(--warn-fg);border-color:var(--warn-bd)}
.pill.err{background:var(--err-bg);color:var(--err-fg);border-color:var(--err-bd)}
.pill.info{background:var(--info-bg);color:var(--info-fg);border-color:var(--info-bd)}
.actions{display:flex;flex-direction:column;align-items:flex-end;gap:8px}
.buttons{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}
button{font:inherit;font-weight:500;padding:7px 14px;border-radius:8px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);cursor:pointer;transition:background .12s,border-color .12s}
button:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
button.secondary{background:var(--surface);color:var(--text);border-color:var(--border)}
button.secondary:hover{background:var(--neutral-bg)}
button:disabled{opacity:.55;cursor:default}
.result{font-size:12px;color:var(--muted);min-height:18px;text-align:right}
.result.ok{color:var(--ok-fg)}
.result.err{color:var(--err-fg)}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:12px;margin-bottom:12px}
.stat{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:12px 14px;min-width:0}
.stat .label{font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);margin-bottom:4px}
.stat .value{font-size:14px;font-weight:550;overflow-wrap:anywhere}
.stat .hint{font-size:12px;color:var(--muted);margin-top:2px}
.alert{border-radius:10px;padding:10px 14px;margin:12px 0;border:1px solid var(--err-bd);background:var(--err-bg);color:var(--err-fg)}
.alert.warn{border-color:var(--warn-bd);background:var(--warn-bg);color:var(--warn-fg)}
.alert.info{border-color:var(--info-bd);background:var(--info-bg);color:var(--info-fg)}
.alert ul{margin:4px 0 0;padding-left:18px}
.section{margin-top:26px}
.section-head{display:flex;align-items:baseline;gap:10px;margin-bottom:10px}
h2{font-size:16px;font-weight:650;margin:0}
.count{color:var(--muted);font-size:13px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:10px;overflow:hidden}
.table-wrap{overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{padding:10px 12px;text-align:left;vertical-align:top;border-bottom:1px solid var(--border)}
th{font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--muted);background:var(--neutral-bg);white-space:nowrap}
tbody tr:last-child td{border-bottom:0}
tbody tr:hover td{background:color-mix(in srgb,var(--neutral-bg) 55%,transparent)}
td.account{min-width:180px;max-width:320px}
td.account .name{font-weight:550;overflow-wrap:anywhere}
code{font-family:var(--mono);font-size:12px;background:var(--neutral-bg);border:1px solid var(--neutral-bd);padding:1px 6px;border-radius:5px;overflow-wrap:anywhere}
.sub{display:block;font-size:12px;color:var(--muted);margin-top:2px}
.flags{display:flex;flex-wrap:wrap;gap:4px}
time{font-variant-numeric:tabular-nums}
time .d,time .t{white-space:nowrap}
.rel{display:block;font-size:12px;color:var(--muted)}
.dash{color:var(--faint)}
.error-text{color:var(--err-fg)}
.empty{padding:18px 14px;color:var(--muted);font-style:italic}
.footnote{margin-top:28px;padding-top:14px;border-top:1px solid var(--border);font-size:12px;color:var(--muted);line-height:1.55}
.footnote code{font-size:11px}
td.num{font-variant-numeric:tabular-nums;white-space:nowrap}
button.row{padding:4px 10px;font-size:12px}
</style>
</head>
<body>
<div class="page">
<div class="header">
  <div>
    <h1>Auto Baseline</h1>
    <p class="subtitle">{{if .Authenticated}}Learned Claude Code and Codex CLI fingerprint baselines and what will be promoted into config.yaml.{{else}}Redacted read-only view. Baselines, candidates, and counters are shown; paths, errors, and identifiers are hidden.{{end}}</p>
    <div class="pills">
      {{if .DryRun}}<span class="pill warn">dry-run</span>{{else}}<span class="pill info">live writes</span>{{end}}
      {{if .DryRunAwaitingReload}}<span class="pill warn">dry-run change awaiting reload</span>{{end}}
      {{if .Enabled}}<span class="pill ok">enabled</span>{{else}}<span class="pill warn">disabled</span>{{end}}
      {{if .Faulted}}<span class="pill err">faulted</span>{{else if .Stopped}}<span class="pill warn">stopped</span>{{else}}<span class="pill ok">running</span>{{end}}
      {{if .Authenticated}}<span class="pill info">authenticated view</span>{{else}}<span class="pill">redacted view</span>{{end}}
    </div>
  </div>
  {{if .Authenticated}}
  <div class="actions">
    <div class="buttons">
      <button type="button" class="secondary" data-action="refresh">Refresh</button>
      <button type="button" class="secondary" data-action="reset">Clear pending</button>
      {{if .DryRun}}<button type="button" data-action="dry-run" data-enabled="false">Switch to live writes</button>{{else}}<button type="button" data-action="dry-run" data-enabled="true">Switch to dry-run</button>{{end}}
    </div>
    <span id="action-result" class="result"></span>
  </div>
  {{end}}
</div>

<div class="stats">
  {{range .Providers}}
  <div class="stat">
    <div class="label">{{.Name}} baseline</div>
    <div class="value">{{.Effective.Version}} {{if .Effective.Explicit}}<span class="pill info">explicit</span>{{else}}<span class="pill">implicit</span>{{end}}{{if .Effective.Unsupported}} <span class="pill err">{{.Effective.Unsupported}}</span>{{else if .Effective.Malformed}} <span class="pill err">malformed</span>{{end}}{{if .AwaitingReload}} <span class="pill warn">awaiting reload</span>{{end}}</div>
    <div class="hint"><code>{{.Effective.UserAgent}}</code></div>
    <div class="hint">{{if .Managed}}floor {{.MinVersion}} · {{.PendingCount}} pending{{else}}not managed{{end}}</div>
  </div>
  {{end}}
  <div class="stat">
    <div class="label">Quorum</div>
    <div class="value">{{.Rules.MinObservations}} observations · {{.Rules.MinDistinctSessions}} sessions</div>
    <div class="hint">window {{.Rules.ObservationWindow}} · cooldown {{.Rules.PromotionCooldown}}</div>
  </div>
  <div class="stat">
    <div class="label">Config file</div>
    <div class="value">{{if .Config.ModeUnsupported}}<span class="pill err">{{.Config.Mode}} mode</span>{{else if .Config.Writable}}<span class="pill ok">writable</span>{{else}}<span class="pill err">not writable</span>{{end}}</div>
    {{if .Config.Path}}<div class="hint"><code>{{.Config.Path}}</code> via {{.Config.Source}}</div>{{end}}
  </div>
  <div class="stat">
    <div class="label">Backup dir</div>
    <div class="value">{{if .Backup.Writable}}<span class="pill ok">writable</span>{{else}}<span class="pill err">not writable</span>{{end}}</div>
    {{if .Backup.Dir}}<div class="hint"><code>{{.Backup.Dir}}</code></div>{{end}}
  </div>
  <div class="stat">
    <div class="label">State file</div>
    <div class="value">{{if .State.Loaded}}<span class="pill ok">loaded</span>{{else}}<span class="pill warn">not loaded</span>{{end}}{{if .StateSaved.Set}} <span class="sub">saved {{.StateSaved.Relative}}</span>{{end}}</div>
    {{if .State.Dir}}<div class="hint"><code>{{.State.Dir}}/state.json</code></div>{{end}}
  </div>
  <div class="stat">
    <div class="label">Assumed CPA build</div>
    <div class="value">{{.CPAVersion}}</div>
    <div class="hint">compiled defaults assumed when config.yaml omits a field; raise the floors after a CPA upgrade</div>
  </div>
  <div class="stat">
    <div class="label">Generated</div>
    <div class="value">{{template "when" .GeneratedAt}}</div>
    <div class="hint">Times shown in {{.Timezone}}</div>
  </div>
</div>

{{if .Config.Error}}<div class="alert"><strong>Config file:</strong> {{.Config.Error}}</div>{{end}}
{{if .Backup.Error}}<div class="alert"><strong>Backup dir:</strong> {{.Backup.Error}}</div>{{end}}
{{if .State.Error}}<div class="alert warn"><strong>State file:</strong> {{.State.Error}}</div>{{end}}
{{if .LastError}}<div class="alert"><strong>Last error{{if .LastErrorAt.Set}} ({{.LastErrorAt.Relative}}){{end}}:</strong> {{.LastError}}</div>{{end}}
{{if .CodexCloakingWarning}}<div class="alert warn"><strong>Codex:</strong> {{.CodexCloakingWarning}}</div>{{end}}
{{if .Warnings}}<div class="alert warn"><strong>Warnings</strong><ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
{{if not .Authenticated}}<p class="footnote" id="session-note"></p>{{end}}

<div class="section">
  <div class="section-head">
    <h2>Pending candidates</h2>
    <span class="count">{{len .Pending}} candidate{{if ne (len .Pending) 1}}s{{end}}</span>
  </div>
  <div class="card">
  {{if .Pending}}
  <div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Provider</th>
        <th>Version</th>
        <th>User-Agent</th>
        <th>Package</th>
        <th>Runtime</th>
        <th>Observations</th>
        <th>Sessions</th>
        <th>First seen</th>
        <th>Last seen</th>
        <th>Status</th>
        {{if $.Authenticated}}<th></th>{{end}}
      </tr>
    </thead>
    <tbody>
    {{range .Pending}}
      <tr>
        <td>{{.ProviderName}}</td>
        <td class="num"><strong>{{.Version}}</strong></td>
        <td><code>{{.UserAgent}}</code></td>
        <td class="num">{{if .PackageVersion}}<code>{{.PackageVersion}}</code>{{else}}<span class="dash">—</span>{{end}}</td>
        <td class="num">{{if .RuntimeVersion}}<code>{{.RuntimeVersion}}</code>{{else}}<span class="dash">—</span>{{end}}</td>
        <td class="num">{{.Observations}} / {{.NeedObs}}</td>
        <td class="num">{{.Sessions}} / {{.NeedSessions}}</td>
        <td>{{template "when" .FirstSeen}}</td>
        <td>{{template "when" .LastSeen}}</td>
        <td>{{if .Blocked}}<span class="pill err">{{.Blocked}}</span>{{else if .QuorumMet}}<span class="pill ok">quorum met</span>{{else}}<span class="pill">collecting</span>{{end}}</td>
        {{if $.Authenticated}}<td><button type="button" class="row" data-action="promote" data-provider="{{.Provider}}" data-user-agent="{{.UserAgent}}" data-package-version="{{.PackageVersion}}" data-runtime-version="{{.RuntimeVersion}}" data-os="{{.OS}}" data-arch="{{.Arch}}">Promote now</button></td>{{end}}
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p class="empty">No candidate newer than the effective baseline has been observed inside the window.</p>{{end}}
  </div>
</div>

<div class="section">
  <div class="section-head">
    <h2>Promotion history</h2>
    <span class="count">{{len .History}} entr{{if eq (len .History) 1}}y{{else}}ies{{end}}</span>
  </div>
  <div class="card">
  {{if .History}}
  <div class="table-wrap">
  <table>
    <thead>
      <tr><th>When</th><th>Provider</th><th>From → to</th><th>Tuple</th><th>Source</th><th>Mode</th><th>Reload</th></tr>
    </thead>
    <tbody>
    {{range .History}}
      <tr>
        <td>{{template "when" .At}}</td>
        <td>{{.Provider}}</td>
        <td class="num">{{.From}} → <strong>{{.To}}</strong></td>
        <td><code>{{.Tuple}}</code></td>
        <td>{{.Source}}{{if .Forced}} <span class="pill warn">forced</span>{{end}}</td>
        <td>{{if .DryRun}}<span class="pill warn">dry-run</span>{{else}}<span class="pill info">written</span>{{end}}</td>
        <td>{{if .DryRun}}<span class="dash">—</span>{{else if .Awaiting}}<span class="pill warn">awaiting</span>{{else if .Confirmed}}<span class="pill ok">confirmed</span>{{else}}<span class="dash">—</span>{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p class="empty">No promotions yet.</p>{{end}}
  </div>
</div>

<div class="section">
  <div class="section-head"><h2>Counters</h2></div>
  <div class="card"><div class="table-wrap"><table><tbody>
  {{range .Counters}}<tr><td>{{.Label}}</td><td class="num">{{.Value}}</td></tr>{{end}}
  </tbody></table></div></div>
</div>

<p class="footnote">{{if .Authenticated}}Promote now queues an immediate promotion of that exact tuple, the dry-run switch edits only <code>plugins.configs.auto-baseline.dry-run</code> in CPA's config.yaml (CPA hot-reloads it), and Clear pending discards collected evidence. All actions post to same-origin management routes with the plugin action header; CPA must receive the management authentication header through the browser session or reverse proxy.{{else}}File paths, error text, and session identifiers are redacted on this unauthenticated page. When opened from the same origin as a signed-in management console, the full authenticated view loads in place.{{end}} Hover any timestamp for the exact UTC instant.</p>
</div>
<script>` + browserAuthScript + `{{if .Authenticated}}` + managementActionsScript + `{{else}}` + resourceBootstrapScript + `{{end}}</script>
</body>
</html>
{{define "when"}}{{if .Set}}<time datetime="{{.Exact}}" title="{{.Exact}}"><span class="d">{{.Date}}</span> - <span class="t">{{.Clock}}</span></time>{{if .Relative}}<span class="rel">{{.Relative}}</span>{{end}}{{else}}<span class="dash">—</span>{{end}}{{end}}
`))

// renderStatusPage renders the shared browser view. authenticated selects the
// management variant (actions, exact paths); the caller is responsible for
// passing a redacted snapshot when authenticated is false.
func renderStatusPage(snap engine.Snapshot, authenticated bool) []byte {
	var output bytes.Buffer
	if err := statusTemplate.Execute(&output, buildStatusPageData(snap, authenticated)); err != nil {
		return []byte("<!doctype html><title>Auto Baseline</title><p>Status rendering failed.</p>")
	}
	return output.Bytes()
}

func buildStatusPageData(snap engine.Snapshot, authenticated bool) statusPageData {
	loc := config.LoadDisplayLocation(snap.DisplayTimezone)
	now := snap.GeneratedAt
	when := func(t time.Time) pageTime { return newPageTime(t, now, loc) }

	data := statusPageData{
		Authenticated:        authenticated,
		Timezone:             displayZoneName(snap.DisplayTimezone, loc),
		GeneratedAt:          when(now),
		Enabled:              snap.Enabled,
		Stopped:              snap.Stopped,
		Faulted:              snap.Faulted,
		DryRun:               snap.DryRun,
		DryRunAwaitingReload: snap.DryRunAwaitingReload,
		DryRunTarget:         snap.DryRunTarget,
		Warnings:             snap.Warnings,
		LastError:            snap.LastError,
		LastErrorAt:          when(snap.LastErrorAt),
		Config:               snap.Config,
		ConfigRead:           when(snap.Config.ReadAt),
		Backup:               snap.Backup,
		State:                snap.State,
		StateSaved:           when(snap.State.SavedAt),
		Rules:                snap.Rules,
		Entrypoints:          strings.Join(snap.Rules.ClaudeEntrypoints, ", "),
		CPAVersion:           snap.CPAVersion,
	}
	data.GeneratedAt.Relative = ""

	for _, p := range snap.Baselines {
		prov := pageProvider{
			Name:           providerName(p.Provider),
			Provider:       string(p.Provider),
			Managed:        p.Managed,
			MinVersion:     p.MinVersion.String(),
			Effective:      p.Effective,
			NextWriteAfter: when(p.NextWriteAfter),
			AwaitingReload: p.AwaitingReload,
			PendingCount:   len(p.Pending),
		}
		for _, w := range p.Warnings {
			// The Codex cloaking caveat gets its own alert; other provider
			// warnings join the general list.
			if p.Provider == fingerprint.ProviderCodex && strings.Contains(w, "disable-codex-cloaking") {
				data.CodexCloakingWarning = w
				continue
			}
			prov.Warnings = append(prov.Warnings, w)
			data.Warnings = append(data.Warnings, providerName(p.Provider)+": "+w)
		}
		if p.LastPromotion != nil {
			h := historyRow(*p.LastPromotion, when)
			prov.LastPromotion = &h
		}
		for _, ev := range p.Pending {
			data.Pending = append(data.Pending, pageCandidate{
				Provider:       string(p.Provider),
				ProviderName:   providerName(p.Provider),
				Version:        ev.Candidate.Version.String(),
				UserAgent:      ev.Candidate.UserAgent,
				PackageVersion: ev.Candidate.PackageVersion,
				RuntimeVersion: ev.Candidate.RuntimeVersion,
				OS:             ev.Candidate.OS,
				Arch:           ev.Candidate.Arch,
				Observations:   ev.Observations,
				NeedObs:        snap.Rules.MinObservations,
				Sessions:       ev.DistinctSessions,
				NeedSessions:   snap.Rules.MinDistinctSessions,
				FirstSeen:      when(ev.FirstSeen),
				LastSeen:       when(ev.LastSeen),
				QuorumMet:      ev.QuorumMet,
				Blocked:        p.Effective.Unsupported,
			})
			if p.Effective.Malformed && data.Pending[len(data.Pending)-1].Blocked == "" {
				data.Pending[len(data.Pending)-1].Blocked = "baseline_malformed"
			}
		}
		data.Providers = append(data.Providers, prov)
	}
	for _, h := range snap.History {
		data.History = append(data.History, historyRow(h, when))
	}
	data.Counters = []pageCounter{
		{"Requests seen", snap.Counters.Requests},
		{"Ignored (not a managed client)", snap.Counters.Ignored},
		{"Accepted observations", snap.Counters.Accepted},
		{"Rejected (client-shaped but failed validation)", snap.Counters.Rejected},
	}
	for _, key := range sortedKeys(snap.Counters.RejectReasons) {
		data.Counters = append(data.Counters, pageCounter{"rejected: " + key, snap.Counters.RejectReasons[key]})
	}
	for _, key := range sortedKeys(snap.Counters.Decisions) {
		data.Counters = append(data.Counters, pageCounter{"decision: " + key, snap.Counters.Decisions[key]})
	}
	return data
}

func historyRow(p statefile.Promotion, when func(time.Time) pageTime) pageHistory {
	return pageHistory{
		At:        when(p.At),
		Provider:  providerName(p.Provider),
		From:      p.From.String(),
		To:        p.Candidate.Version.String(),
		Tuple:     tupleLabel(p.Candidate),
		Source:    p.Source,
		DryRun:    p.DryRun,
		Forced:    p.Forced,
		Awaiting:  p.AwaitingReload,
		Confirmed: !p.DryRun && !p.AwaitingReload && !p.ConfirmedAt.IsZero(),
	}
}

func tupleLabel(c fingerprint.Candidate) string {
	if c.Provider == fingerprint.ProviderClaude {
		return c.UserAgent + " · pkg " + c.PackageVersion + " · rt " + c.RuntimeVersion
	}
	return c.UserAgent
}

func providerName(p fingerprint.Provider) string {
	switch p {
	case fingerprint.ProviderClaude:
		return "Claude"
	case fingerprint.ProviderCodex:
		return "Codex"
	default:
		return string(p)
	}
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newPageTime(t, now time.Time, loc *time.Location) pageTime {
	if t.IsZero() {
		return pageTime{}
	}
	local := t.In(loc)
	return pageTime{
		Set:      true,
		Date:     local.Format(displayDateLayout),
		Clock:    local.Format(displayClockLayout),
		Exact:    t.UTC().Format(time.RFC3339Nano),
		Relative: relativeTime(t, now),
	}
}

// relativeTime renders a coarse human hint ("in 2m", "3h 12m ago") relative
// to the page generation time. It is intentionally approximate.
func relativeTime(t, now time.Time) string {
	if now.IsZero() {
		return ""
	}
	d := t.Sub(now)
	future := d >= 0
	if !future {
		d = -d
	}
	if d < time.Second {
		return "now"
	}
	var s string
	switch {
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%dh %dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	default:
		s = fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int((d%(24*time.Hour))/time.Hour))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

func displayZoneName(configured string, loc *time.Location) string {
	if strings.EqualFold(strings.TrimSpace(configured), "local") {
		return "host local time (" + loc.String() + ")"
	}
	if loc == nil || loc.String() == "" {
		return "UTC"
	}
	return loc.String()
}

// redactResourceStatus strips everything the unauthenticated resource page
// must not show: file paths, error and warning text, the last-error detail,
// and the deployment topology (mode / mode reason / unsupported flag). Versions, baselines, candidate tuples, quorum rules, counters, and
// the assumed CPA build stay (none is secret). Session identifiers are never
// present in the snapshot to begin with, only their counts.
func redactResourceStatus(snap engine.Snapshot) engine.Snapshot {
	snap.Config.Path = ""
	snap.Config.Source = ""
	snap.Config.SHA256 = ""
	snap.Config.Error = ""
	snap.Config.Mode = ""
	snap.Config.ModeReason = ""
	snap.Config.ModeUnsupported = false
	snap.Backup.Dir = ""
	snap.Backup.Error = ""
	snap.State.Dir = ""
	snap.State.Error = ""
	snap.LastError = ""
	snap.LastErrorAt = time.Time{}
	snap.FaultReason = ""
	snap.Warnings = nil
	providers := make([]engine.ProviderStatus, len(snap.Baselines))
	for i, p := range snap.Baselines {
		p.Warnings = nil
		providers[i] = p
	}
	snap.Baselines = providers
	return snap
}
