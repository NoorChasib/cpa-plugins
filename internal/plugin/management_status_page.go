package plugin

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/config"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/engine"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/statefile"
)

// This file renders the AUTHENTICATED management HTML status view. It is
// reachable only through the CPA Management API (GET
// /v0/management/plugins/auto-baseline/status/html) and is separate from the
// static resource shell in status_page.go.
//
// Data rules: only fields from the already-sanitized engine.Snapshot are
// rendered, through html/template's contextual auto-escaping. Session IDs are
// never rendered (only counts). Timestamps use the configured
// display-timezone with the exact RFC3339 UTC instant on each <time>.

const (
	displayDateLayout  = "Mon Jan 2 2006"
	displayClockLayout = "3:04:05 PM MST"
)

type statusPageData struct {
	GeneratedAt statusPageTime
	Timezone    string
	DryRun      bool
	Enabled     bool
	Stopped     bool
	Faulted     bool
	Warnings    []string
	LastError   string
	LastErrorAt statusPageTime
	Config      engine.ConfigStatus
	Backup      engine.BackupStatus
	ConfigRead  statusPageTime
	State       engine.StateStatus
	StateSaved  statusPageTime
	Rules       engine.RulesStatus
	Entrypoints string
	Counters    []statusPageCounter
	Providers   []statusPageProvider
	History     []statusPageHistory
	CPAVersion  string
}

type statusPageCounter struct {
	Label string
	Value uint64
}

type statusPageTime struct {
	Set      bool
	Date     string
	Clock    string
	Exact    string
	Relative string
}

type statusPageProvider struct {
	Provider       string
	Managed        bool
	MinVersion     string
	Effective      engine.EffectiveStatus
	Warnings       []string
	Pending        []statusPageCandidate
	LastPromotion  *statusPageHistory
	NextWriteAfter statusPageTime
	AwaitingReload bool
}

type statusPageCandidate struct {
	Version   string
	Tuple     string
	Obs       int
	Sessions  int
	FirstSeen statusPageTime
	LastSeen  statusPageTime
	QuorumMet bool
}

type statusPageHistory struct {
	At        statusPageTime
	Confirmed bool
	Provider  string
	From      string
	To        string
	Tuple     string
	Mode      string
	Source    string
	Evidence  string
}

var managementStatusTemplate = template.Must(template.New("management-status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Auto Baseline — Management Status</title>
<style>
:root{
  --bg:#f8fafc;--surface:#ffffff;--border:#e2e8f0;--text:#0f172a;--muted:#64748b;--faint:#94a3b8;
  --ok-bg:#ecfdf5;--ok-fg:#047857;--ok-bd:#a7f3d0;--warn-bg:#fffbeb;--warn-fg:#b45309;--warn-bd:#fde68a;
  --err-bg:#fef2f2;--err-fg:#b91c1c;--err-bd:#fecaca;--info-bg:#eff6ff;--info-fg:#1d4ed8;--info-bd:#bfdbfe;
  --neutral-bg:#f1f5f9;--neutral-fg:#334155;--neutral-bd:#e2e8f0;--accent:#2563eb;--accent-fg:#fff;--accent-hover:#1d4ed8;
  --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
}
@media (prefers-color-scheme:dark){
  :root{--bg:#0b1220;--surface:#111a2b;--border:#1f2a3d;--text:#e5eaf3;--muted:#98a4b8;--faint:#6b778c;
  --ok-bg:#062f26;--ok-fg:#6ee7b7;--ok-bd:#0f5c47;--warn-bg:#3a2a06;--warn-fg:#fcd34d;--warn-bd:#6b4d0b;
  --err-bg:#3b0f0f;--err-fg:#fca5a5;--err-bd:#7f1d1d;--info-bg:#0f2148;--info-fg:#93c5fd;--info-bd:#1e3a8a;
  --neutral-bg:#1a2436;--neutral-fg:#cbd5e1;--neutral-bd:#2c3a52;--accent:#3b82f6;--accent-hover:#60a5fa}
}
*{box-sizing:border-box}
html,body{margin:0;background:var(--bg);color:var(--text)}
body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
.page{max-width:1200px;margin:0 auto;padding:28px 24px 48px}
.header{display:flex;flex-wrap:wrap;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:20px}
h1{font-size:24px;font-weight:650;letter-spacing:-.01em;margin:0 0 4px}
.subtitle{margin:0;color:var(--muted)}
.pills{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
.pill{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--neutral-bd);background:var(--neutral-bg);color:var(--neutral-fg);border-radius:999px;padding:2px 10px;font-size:12px;font-weight:500;line-height:18px}
.pill::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor;opacity:.8}
.pill.ok{background:var(--ok-bg);color:var(--ok-fg);border-color:var(--ok-bd)}
.pill.warn{background:var(--warn-bg);color:var(--warn-fg);border-color:var(--warn-bd)}
.pill.err{background:var(--err-bg);color:var(--err-fg);border-color:var(--err-bd)}
.pill.info{background:var(--info-bg);color:var(--info-fg);border-color:var(--info-bd)}
.actions{display:flex;flex-direction:column;align-items:flex-end;gap:6px}
button{font:inherit;font-weight:500;padding:7px 14px;border-radius:8px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);cursor:pointer}
button:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
button:disabled{opacity:.55;cursor:default}
#action-result{font-size:12px;color:var(--muted);min-height:18px}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px;margin-bottom:24px}
.stat{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:12px 14px}
.stat .label{font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);margin-bottom:4px}
.stat .value{font-size:14px;font-weight:550;overflow-wrap:anywhere}
.stat .hint{font-size:12px;color:var(--muted);margin-top:2px}
.alert{border-radius:10px;padding:10px 14px;margin-bottom:12px;border:1px solid var(--err-bd);background:var(--err-bg);color:var(--err-fg)}
.alert.warn{border-color:var(--warn-bd);background:var(--warn-bg);color:var(--warn-fg)}
.alert ul{margin:4px 0 0;padding-left:18px}
.section{margin-top:28px}
.section-head{display:flex;align-items:baseline;gap:10px;margin-bottom:10px}
h2{font-size:16px;font-weight:650;margin:0;text-transform:capitalize}
.count{color:var(--muted);font-size:13px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:10px;overflow:hidden}
.card .body{padding:12px 14px}
.kv{display:grid;grid-template-columns:max-content 1fr;gap:4px 16px;font-size:13px}
.kv dt{color:var(--muted)}
.kv dd{margin:0;overflow-wrap:anywhere}
.table-wrap{overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{padding:10px 12px;text-align:left;vertical-align:top;border-bottom:1px solid var(--border)}
th{font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--muted);background:var(--neutral-bg);white-space:nowrap}
tbody tr:last-child td{border-bottom:0}
td.num{font-variant-numeric:tabular-nums;white-space:nowrap}
code{font-family:var(--mono);font-size:12px;background:var(--neutral-bg);border:1px solid var(--neutral-bd);padding:1px 6px;border-radius:5px;overflow-wrap:anywhere}
time{font-variant-numeric:tabular-nums}
.rel{display:block;font-size:12px;color:var(--muted)}
.dash{color:var(--faint)}
.empty{padding:18px 14px;color:var(--muted);font-style:italic}
.footnote{margin-top:28px;padding-top:14px;border-top:1px solid var(--border);font-size:12px;color:var(--muted);line-height:1.55}
</style>
</head>
<body>
<div class="page">
<div class="header">
  <div>
    <h1>Auto Baseline</h1>
    <p class="subtitle">Authenticated management status view</p>
    <div class="pills">
      {{if .DryRun}}<span class="pill warn">dry-run</span>{{else}}<span class="pill info">live writes</span>{{end}}
      {{if .Enabled}}<span class="pill ok">enabled</span>{{else}}<span class="pill warn">disabled</span>{{end}}
      {{if .Stopped}}<span class="pill warn">stopped</span>{{else}}<span class="pill ok">learning</span>{{end}}
      {{if .Faulted}}<span class="pill err">faulted</span>{{end}}
      {{if .Config.ModeUnsupported}}<span class="pill err">{{.Config.Mode}} mode: writes disabled</span>{{end}}
      {{if .Config.Writable}}<span class="pill ok">config writable</span>{{else}}<span class="pill err">config not writable</span>{{end}}
      {{if .Backup.Writable}}<span class="pill ok">backup dir writable</span>{{else}}<span class="pill err">backup dir not writable</span>{{end}}
    </div>
  </div>
  <div class="actions">
    <button id="reset-pending" type="button">Clear pending candidates</button>
    <span id="action-result"></span>
  </div>
</div>

<div class="stats">
  <div class="stat"><div class="label">Config file</div><div class="value"><code>{{.Config.Path}}</code></div><div class="hint">via {{.Config.Source}}{{if .Config.SHA256}} · sha256 {{printf "%.12s" .Config.SHA256}}…{{end}}</div></div>
  <div class="stat"><div class="label">Config last read</div><div class="value">{{template "when" .ConfigRead}}</div></div>
  <div class="stat"><div class="label">Backup dir</div><div class="value"><code>{{.Backup.Dir}}</code></div><div class="hint">{{if .Backup.Writable}}writable{{else}}not writable{{end}}</div></div>
  <div class="stat"><div class="label">Assumed CPA build</div><div class="value">{{.CPAVersion}}</div><div class="hint">compiled defaults assumed when config.yaml omits a field; raise the floors after a CPA upgrade</div></div>
  <div class="stat"><div class="label">State file</div><div class="value"><code>{{.State.Dir}}/state.json</code></div><div class="hint">{{if .State.Loaded}}loaded{{else}}not loaded{{end}}{{if .StateSaved.Set}} · saved {{.StateSaved.Relative}}{{end}}</div></div>
  <div class="stat"><div class="label">Quorum</div><div class="value">{{.Rules.MinObservations}} observations / {{.Rules.MinDistinctSessions}} sessions</div><div class="hint">window {{.Rules.ObservationWindow}} · cooldown {{.Rules.PromotionCooldown}}</div></div>
  <div class="stat"><div class="label">Claude entrypoints</div><div class="value">{{.Entrypoints}}</div><div class="hint">claude-code beta required: {{.Rules.RequireClaudeCodeBeta}}</div></div>
  <div class="stat"><div class="label">Generated</div><div class="value">{{template "when" .GeneratedAt}}</div><div class="hint">Times shown in {{.Timezone}}</div></div>
</div>

{{if .Config.Error}}<div class="alert"><strong>Config file:</strong> {{.Config.Error}}</div>{{end}}
{{if .Backup.Error}}<div class="alert"><strong>Backup dir:</strong> {{.Backup.Error}}</div>{{end}}
{{if .State.Error}}<div class="alert warn"><strong>State file:</strong> {{.State.Error}}</div>{{end}}
{{if .LastError}}<div class="alert"><strong>Last error{{if .LastErrorAt.Set}} ({{.LastErrorAt.Relative}}){{end}}:</strong> {{.LastError}}</div>{{end}}
{{if .Warnings}}<div class="alert warn"><strong>Warnings</strong><ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul></div>{{end}}

{{range .Providers}}
<div class="section">
  <div class="section-head">
    <h2>{{.Provider}}</h2>
    <span class="count">{{if .Managed}}managed · <strong>floor {{.MinVersion}}</strong> (never written below){{else}}not managed{{end}}</span>
  </div>
  <div class="card">
    <div class="body">
      <dl class="kv">
        <dt>Effective baseline</dt><dd><strong>{{.Effective.Version}}</strong> {{if .Effective.Explicit}}<span class="pill info">explicit in config.yaml</span>{{else}}<span class="pill">compiled default</span>{{end}}{{if .Effective.Unsupported}} <span class="pill err">{{.Effective.Unsupported}}</span>{{else if .Effective.Malformed}} <span class="pill err">malformed user-agent</span>{{end}}</dd>
        <dt>user-agent</dt><dd><code>{{.Effective.UserAgent}}</code></dd>
        {{if .Effective.PackageVersion}}<dt>package-version</dt><dd><code>{{.Effective.PackageVersion}}</code></dd>{{end}}
        {{if .Effective.RuntimeVersion}}<dt>runtime-version</dt><dd><code>{{.Effective.RuntimeVersion}}</code></dd>{{end}}
        {{if .LastPromotion}}<dt>Last promotion</dt><dd>{{.LastPromotion.From}} → {{.LastPromotion.To}} ({{.LastPromotion.Mode}}, {{.LastPromotion.Source}}) {{template "when" .LastPromotion.At}}{{if .AwaitingReload}} <span class="pill warn">written, awaiting CPA reload</span>{{else if .LastPromotion.Confirmed}} <span class="pill ok">reload confirmed</span>{{end}}</dd>{{end}}
        {{if .NextWriteAfter.Set}}<dt>Next write allowed</dt><dd>{{template "when" .NextWriteAfter}}</dd>{{end}}
      </dl>
      {{if .Warnings}}<div class="alert warn" style="margin-top:12px"><ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
    </div>
    {{if .Pending}}
    <div class="table-wrap"><table>
      <thead><tr><th>Candidate</th><th>Tuple</th><th>Observations</th><th>Sessions</th><th>First seen</th><th>Last seen</th><th>Quorum</th></tr></thead>
      <tbody>
      {{range .Pending}}<tr>
        <td class="num"><strong>{{.Version}}</strong></td>
        <td><code>{{.Tuple}}</code></td>
        <td class="num">{{.Obs}}</td>
        <td class="num">{{.Sessions}}</td>
        <td>{{template "when" .FirstSeen}}</td>
        <td>{{template "when" .LastSeen}}</td>
        <td>{{if .QuorumMet}}<span class="pill ok">met</span>{{else}}<span class="pill">pending</span>{{end}}</td>
      </tr>{{end}}
      </tbody></table></div>
    {{else}}<p class="empty">No newer candidate observed inside the window.</p>{{end}}
  </div>
</div>
{{end}}

<div class="section">
  <div class="section-head"><h2>Counters</h2></div>
  <div class="card"><div class="table-wrap"><table><tbody>
  {{range .Counters}}<tr><td>{{.Label}}</td><td class="num">{{.Value}}</td></tr>{{end}}
  </tbody></table></div></div>
</div>

<div class="section">
  <div class="section-head"><h2>Promotion history</h2><span class="count">{{len .History}} entr{{if eq (len .History) 1}}y{{else}}ies{{end}}</span></div>
  <div class="card">
  {{if .History}}
  <div class="table-wrap"><table>
    <thead><tr><th>When</th><th>Provider</th><th>From</th><th>To</th><th>Tuple</th><th>Mode</th><th>Source</th><th>Evidence</th></tr></thead>
    <tbody>{{range .History}}<tr><td>{{template "when" .At}}</td><td>{{.Provider}}</td><td class="num">{{.From}}</td><td class="num">{{.To}}</td><td><code>{{.Tuple}}</code></td><td>{{.Mode}}</td><td>{{.Source}}</td><td>{{.Evidence}}</td></tr>{{end}}</tbody>
  </table></div>
  {{else}}<p class="empty">No promotions yet.</p>{{end}}
  </div>
</div>

<p class="footnote">Clear pending candidates issues <code>POST /v0/management/plugins/auto-baseline/reset</code> on the same origin with the <code>X-Auto-Baseline-Request: 1</code> header and reloads. Baselines and history are kept. Session identifiers are counted but never displayed. Hover any timestamp for the exact UTC instant.</p>
</div>
<script>` + browserAuthScript + `
(function () {
	var button = document.getElementById("reset-pending");
	var result = document.getElementById("action-result");
	if (!button || !result) {
		return;
	}
	button.addEventListener("click", function () {
		button.disabled = true;
		result.textContent = "Clearing…";
		var auth = window.autoBaselineAuth;
		fetch(auth.managementPath("/reset") + location.search, {
			method: "POST",
			credentials: "same-origin",
			headers: auth.authHeaders({ "X-Auto-Baseline-Request": "1" })
		})
			.then(function (resp) {
				if (resp.ok) {
					result.textContent = "Cleared; reloading…";
					location.reload();
					return;
				}
				result.textContent = "Reset failed: HTTP " + resp.status;
				button.disabled = false;
			})
			.catch(function () {
				result.textContent = "Reset failed: network error";
				button.disabled = false;
			});
	});
})();
</script>
</body>
</html>
{{define "when"}}{{if .Set}}<time datetime="{{.Exact}}" title="{{.Exact}}"><span class="d">{{.Date}}</span>{{if .Clock}} - <span class="t">{{.Clock}}</span>{{end}}</time>{{if .Relative}}<span class="rel">{{.Relative}}</span>{{end}}{{else}}<span class="dash">—</span>{{end}}{{end}}
`))

// renderManagementStatusPage renders the authenticated HTML status view from
// the published snapshot.
func renderManagementStatusPage(snap engine.Snapshot) ([]byte, error) {
	loc := config.LoadDisplayLocation(snap.DisplayTimezone)
	now := snap.GeneratedAt
	when := func(t time.Time) statusPageTime { return newStatusPageTime(t, now, loc) }

	data := statusPageData{
		GeneratedAt: when(snap.GeneratedAt),
		Timezone:    displayZoneName(snap.DisplayTimezone, loc),
		DryRun:      snap.DryRun,
		Enabled:     snap.Enabled,
		Stopped:     snap.Stopped,
		Faulted:     snap.Faulted,
		Warnings:    snap.Warnings,
		LastError:   snap.LastError,
		LastErrorAt: when(snap.LastErrorAt),
		Config:      snap.Config,
		Backup:      snap.Backup,
		ConfigRead:  when(snap.Config.ReadAt),
		State:       snap.State,
		StateSaved:  when(snap.State.SavedAt),
		Rules:       snap.Rules,
		Entrypoints: strings.Join(snap.Rules.ClaudeEntrypoints, ", "),
		CPAVersion:  snap.CPAVersion,
	}
	data.GeneratedAt.Relative = ""
	data.Counters = []statusPageCounter{
		{"Requests seen", snap.Counters.Requests},
		{"Ignored (not a managed client)", snap.Counters.Ignored},
		{"Accepted observations", snap.Counters.Accepted},
		{"Rejected (client-shaped but failed validation)", snap.Counters.Rejected},
	}
	for _, key := range sortedKeys(snap.Counters.RejectReasons) {
		data.Counters = append(data.Counters, statusPageCounter{"rejected: " + key, snap.Counters.RejectReasons[key]})
	}
	for _, key := range sortedKeys(snap.Counters.Decisions) {
		data.Counters = append(data.Counters, statusPageCounter{"decision: " + key, snap.Counters.Decisions[key]})
	}
	for _, p := range snap.Baselines {
		prov := statusPageProvider{
			Provider:       string(p.Provider),
			Managed:        p.Managed,
			MinVersion:     p.MinVersion.String(),
			Effective:      p.Effective,
			Warnings:       p.Warnings,
			NextWriteAfter: when(p.NextWriteAfter),
			AwaitingReload: p.AwaitingReload,
		}
		for _, ev := range p.Pending {
			prov.Pending = append(prov.Pending, statusPageCandidate{
				Version:   ev.Candidate.Version.String(),
				Tuple:     tupleLabel(ev.Candidate),
				Obs:       ev.Observations,
				Sessions:  ev.DistinctSessions,
				FirstSeen: when(ev.FirstSeen),
				LastSeen:  when(ev.LastSeen),
				QuorumMet: ev.QuorumMet,
			})
		}
		if p.LastPromotion != nil {
			h := historyRow(*p.LastPromotion, when)
			prov.LastPromotion = &h
		}
		data.Providers = append(data.Providers, prov)
	}
	for _, h := range snap.History {
		data.History = append(data.History, historyRow(h, when))
	}

	var buf bytes.Buffer
	if err := managementStatusTemplate.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func historyRow(p statefile.Promotion, when func(time.Time) statusPageTime) statusPageHistory {
	mode := "written"
	if p.DryRun {
		mode = "dry-run"
	}
	if p.Forced {
		mode += ", forced"
	}
	return statusPageHistory{
		At:        when(p.At),
		Confirmed: !p.DryRun && !p.AwaitingReload && !p.ConfirmedAt.IsZero(),
		Provider:  string(p.Provider),
		From:      p.From.String(),
		To:        p.Candidate.Version.String(),
		Tuple:     tupleLabel(p.Candidate),
		Mode:      mode,
		Source:    p.Source,
		Evidence:  fmt.Sprintf("%d obs / %d sessions", p.Observations, p.DistinctSessions),
	}
}

func tupleLabel(c fingerprint.Candidate) string {
	if c.Provider == fingerprint.ProviderClaude {
		return c.UserAgent + " · pkg " + c.PackageVersion + " · rt " + c.RuntimeVersion
	}
	return c.UserAgent
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newStatusPageTime(t, now time.Time, loc *time.Location) statusPageTime {
	if t.IsZero() {
		return statusPageTime{}
	}
	local := t.In(loc)
	return statusPageTime{
		Set:      true,
		Date:     local.Format(displayDateLayout),
		Clock:    local.Format(displayClockLayout),
		Exact:    t.UTC().Format(time.RFC3339Nano),
		Relative: relativeTime(t, now),
	}
}

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
