package plugin

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

func TestPublicSidebarIsStaticAndDoesNotStartPolling(t *testing.T) {
	host := &testHost{}
	p := New(host)
	defer p.Shutdown()
	for _, query := range []string{"", "?refresh=true&token=private-canary"} {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/quota-cache/status", Body: []byte(query)})
		result, err := p.Handle(protocol.MethodManagementHandle, raw)
		if err != nil {
			t.Fatal(err)
		}
		response := result.(protocol.ManagementResponse)
		if response.StatusCode != 200 || string(response.Body) != sidebarDocument || strings.Contains(string(response.Body), "private-canary") {
			t.Fatal("resource response depends on request data")
		}
		if !strings.Contains(response.Headers.Get("Content-Security-Policy"), sidebarHash(sidebarScript)) || strings.Contains(response.Headers.Get("Content-Security-Policy"), "unsafe-inline") {
			t.Fatal("missing strict script hash")
		}
	}
	if p.cache != nil || host.lists.Load() != 0 {
		t.Fatal("sidebar started polling")
	}
	for _, path := range []string{"/v0/resource/plugins/quota-cache/data", "/v0/resource/plugins/quota-cache/status/html"} {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: path})
		result, _ := p.Handle(protocol.MethodManagementHandle, raw)
		if result.(protocol.ManagementResponse).StatusCode != 404 {
			t.Fatal("resource route exposed quota data")
		}
	}
}

// The page runs in a browser, where scripts/sidebar-smoke.py exercises it. This
// pins the guards that keep it from costing CPA management sign-in failures, so
// an edit that drops one fails here as well.
func TestStatusPageNeverPresentsAMissingOrRefusedKey(t *testing.T) {
	script := sidebarApp
	if strings.Count(script, "fetch(") != 1 || strings.Count(script, "refresh(true)") != 1 {
		t.Fatal("expected one request site, resumed by Refresh view alone")
	}
	// The timer also waits while a card is being edited and shown, so nothing
	// replaces the rows being edited.
	if !strings.Contains(script, "addEventListener('click', () => refresh(true))") ||
		!strings.Contains(script, "if ($('auto').checked && !document.hidden && !holding()) refresh(false); }, 30000)") ||
		!strings.Contains(script, "const holding = () => editing() && !$('content').hidden;") {
		t.Fatal("the timer must never resume after a refusal")
	}
	refresh := script[strings.Index(script, "async function refresh(manual)"):]
	request := strings.Index(refresh, "fetch(")
	for _, guard := range []string{
		"if (busy || (stopped && !manual)) return;",
		"if (!session) { unavailable(SIGN_IN,",
		"if (refused && manual && !banned(refused)) { forget(); refused = null; }",
		"if (refused) { stopped = true;",
	} {
		at := strings.Index(refresh, guard)
		if at < 0 || at > request {
			t.Fatalf("refresh lost %q before its request", guard)
		}
	}
	refusal := refresh[strings.Index(refresh, "response.status === 401 || response.status === 403"):]
	stop, latch, body := strings.Index(refusal, "stopped = true;"), strings.Index(refusal, "remember(session, response.status, null)"), strings.Index(refusal, "response.json()")
	if stop < 0 || latch < stop || body < latch {
		t.Fatal("a refusal must stop the page and be remembered before its body is read")
	}
	// A request that ends without any status may have been refused and counted
	// all the same, so it stops the timer too.
	if answered := strings.Index(refresh, "answered = true;"); answered < request || answered > strings.Index(refresh, "response.status === 401") {
		t.Fatal("a status must be marked as received as soon as fetch resolves")
	}
	unanswered := refresh[strings.Index(refresh, "} catch (error) {"):]
	if !strings.HasPrefix(strings.Join(strings.Fields(unanswered), " "), "} catch (error) { if (!answered) { // No status came back") ||
		!strings.Contains(unanswered[:strings.Index(unanswered, "} else {")], "stopped = true;") {
		t.Fatal("a request that got no answer must stop the timer")
	}
	for _, rule := range []string{
		// The strict console reader.
		"record.version !== 0", "state.rememberPassword !== true", "state.isAuthenticated === false",
		"!matchesRoot(state.apiBase)", "!validKey(state.managementKey)", "stored('isLoggedIn') !== 'true'",
		"value.length > 32768", "new TextDecoder('utf-8', {fatal: true})",
		// The persistent refusal record, and CPA's own refusal messages. The
		// record names its CPA, so a page under another prefix leaves it
		// alone, and falls back to the tab's storage when the origin's is full.
		"'quota-cache.console-refused'", "record.fp === fingerprint(session.key)",
		"{v: 1, root, fp: fingerprint(session.key),", "typeof record.root === 'string' && record.root !== root) return undefined;",
		"try { found.push(localStorage); } catch {}\n    try { found.push(sessionStorage); } catch {}",
		"try { store.setItem(REFUSED_NAME, JSON.stringify(record)); break; } catch {}",
		"'missing management key'", "'invalid management key'", "'IP banned'", "'remote management disabled'", "'remote management key not set'",
	} {
		if !strings.Contains(script, rule) {
			t.Fatalf("page script lost %q", rule)
		}
	}
	// Refresh view drops the refusal and sends the same key again, so the
	// page's own words must not promise that a refused key is never sent.
	footnote := sidebarHTML[strings.Index(sidebarHTML, `<footer class="footnote">`):]
	if strings.Contains(footnote, "never sent again") || !strings.Contains(footnote, "sends the remembered key again only when you select Refresh view") {
		t.Fatal("the footnote must say Refresh view sends a refused key again")
	}
}

// Claude API credit entries have a name, a filter, a source and a reading of
// their own: they are metered from CPA's usage records, never polled, and
// the page prints the meter's counts without adding them up.
func TestStatusPageShowsClaudeAPICredits(t *testing.T) {
	script := sidebarApp
	for _, rule := range []string{
		"'anthropic-api': 'Claude API credits'",
		"'anthropic-api': 'CPA usage meter (no requests)'",
		"item.api_credit.label",
		"const noWeekly = ['openrouter', 'anthropic-api'];",
		"const label = credit ? 'Metered' :",
		"const polled = all.filter(e => e.provider !== 'anthropic-api');",
		"addCell(row, credit ? '—' :",
		"const flushed = date(s.api_meter && s.api_meter.flushed_at);",
		"now - flushed <= 30 * 60000",
		"if (entry.provider === 'anthropic-api') return creditDetails(entry, now, s);",
		"'Organization: ' + (c.organization_id || 'not set')",
		"m.organizations[c.organization_id]",
		"' ok, ' + (u.failed || 0) + ' failed, in ' + (u.input || 0) + ', out ' + (u.output || 0) + ', cache read ' + (u.cache_read || 0) + ', cache write ' + (u.cache_write || 0) + ' tokens'",
		"function meterPanel(s, now)",
		"['Records received', String(m.received || 0)], ['Counted', String(m.counted || 0)], ['Foreign (not Anthropic)', String(m.foreign || 0)]",
		"['Rejected', String(m.rejected || 0)], ['Dropped', String(m.dropped || 0)], ['Unattributed', String(m.unattributed || 0)]",
		"$('meter-gaps')", "$('meter-unlinked')",
	} {
		if !strings.Contains(script, rule) {
			t.Fatalf("page script lost %q", rule)
		}
	}
	for _, gone := range []string{"cost_report", "key_fingerprint", "(lowest units)", "Today not reported yet"} {
		if strings.Contains(script, gone) {
			t.Fatalf("page script still reads the cost report (%q)", gone)
		}
	}
	// The entry carries the organization only: the monthly credit and refill
	// date are the page's, and nothing of claude-api-credits is read.
	for _, gone := range []string{"monthly_usd", "renews_invalid", "admin_key_ignored", ".problem", "(configured)"} {
		if strings.Contains(script, gone) {
			t.Fatalf("page script still reads a configured value (%q)", gone)
		}
	}
	credit := script[strings.Index(script, "function creditDetails("):strings.Index(script, "function render(")]
	for _, arithmetic := range []string{"Number(", "parseFloat", "reduce(", " + Number", "+= "} {
		if strings.Contains(credit, arithmetic) {
			t.Fatalf("the credit reading or the meter panel does arithmetic (%q); totals are Quota Glance's", arithmetic)
		}
	}
	for _, rule := range []string{`<option value="anthropic-api">Claude API credits</option>`, `<section id="meter-section"`, `<dl id="meter"`, `<ul id="meter-gaps"`, `<ul id="meter-unlinked"`} {
		if !strings.Contains(sidebarHTML, rule) {
			t.Fatalf("the page lost %q", rule)
		}
	}
}

// The organizations are entered on this page and nothing moves from the
// configuration, so the page names no configured value, offers none back, and
// tells of no move; it says when claude-api-credits is ignored, and what an
// unreadable organizations file means.
func TestStatusPageNamesNoConfiguredValue(t *testing.T) {
	page := sidebarHTML + sidebarScript
	for _, gone := range []string{"Use config", "from config", "'config '", "not in config", "back to config", "the config’s is not", "Moved ", "has_default", "migration"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still says %q", gone)
		}
	}
	for _, rule := range []string{
		"' under claude-api-credits. This version doesn’t read '",
		"'Quota Cache cannot read its organizations file, so no organization is counted and editing them here is unavailable. Fix the file’s owner or permissions, or move '",
		"'No Console organizations yet. Add one to start counting its API credit.'",
	} {
		if !strings.Contains(page, rule) {
			t.Errorf("the page lost %q", rule)
		}
	}
}

// Values are entered on this page while Quota Glance 0.7 is still installed,
// and 0.7 never reads them, so every line saying Quota Glance shows or reads
// them names the version that does.
func TestStatusPageNamesTheQuotaGlanceThatReadsTheFigures(t *testing.T) {
	page := sidebarHTML + sidebarScript
	if claim := regexp.MustCompile(`Quota Glance (?:shows|reads?|and)\b`).FindString(page); claim != "" {
		t.Fatalf("the page says %q without naming Quota Glance 0.8.0", claim)
	}
	for _, rule := range []string{
		"'. Quota Glance 0.8.0 or newer shows them from its next read of the cache.'",
		"'. Quota Glance 0.8.0 or newer shows these figures, and Reset Priority routes by them when its API-credit routing is on.'",
		"Quota Glance 0.8.0 or newer and Reset Priority 0.2.0 or newer read the same figures.",
	} {
		if !strings.Contains(page, rule) {
			t.Fatalf("the page lost %q", rule)
		}
	}
}

// While a refresh round after a banked reset has credentials left to poll,
// the page says how many, as text, from the status's refresh block only when
// it is a positive whole number; otherwise the line is hidden. The line is a
// live region, so it is written only when its text changes.
func TestStatusPageShowsARefreshRound(t *testing.T) {
	for _, rule := range []string{
		"object(s.refresh) && Number.isInteger(s.refresh.remaining) && s.refresh.remaining > 0 ? s.refresh.remaining : 0",
		"const roundLine = round ? 'Refreshing after a banked reset: ' + round + ' left' : '';",
		"if ($('refresh-round').textContent !== roundLine) $('refresh-round').textContent = roundLine;",
		"$('refresh-round').hidden = !round;",
	} {
		if !strings.Contains(sidebarApp, rule) {
			t.Fatalf("page script lost %q", rule)
		}
	}
	if !strings.Contains(sidebarHTML, `<div id="refresh-round" class="alert" role="status" hidden></div>`) {
		t.Fatal("the page lost the refresh line")
	}
}

// The page is one document with one script element, app.js then editor.js,
// under one CSP hash worked out from exactly those bytes; nothing in it
// depends on a request or on the plugin's state.
func TestSidebarIsOneScriptUnderOneHash(t *testing.T) {
	if strings.Count(sidebarDocument, "<script") != 1 || strings.Count(sidebarDocument, "</script>") != 1 {
		t.Fatal("the page must have exactly one script element")
	}
	if sidebarScript != sidebarApp+"\n"+sidebarEditor || !strings.Contains(sidebarDocument, "<script>"+sidebarApp+"\n"+sidebarEditor+"</script>") {
		t.Fatal("the script element must be app.js then editor.js")
	}
	csp := sidebarResponse().Headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src "+sidebarHash(sidebarApp+"\n"+sidebarEditor)+";") || strings.Contains(csp, sidebarHash(sidebarApp)+";") {
		t.Fatalf("the script hash must cover app.js and editor.js together: %s", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("the policy must stay strict: %s", csp)
	}
	// No other script source can run: no inline handler attribute in the
	// markup, and no string turned into markup or code by the scripts.
	if regexp.MustCompile(`(?i)<[^>]+\son[a-z]+\s*=`).MatchString(sidebarHTML) {
		t.Fatal("the markup has an inline event handler")
	}
	for _, name := range []string{"app.js", "editor.js"} {
		script := map[string]string{"app.js": sidebarApp, "editor.js": sidebarEditor}[name]
		for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute('on", `setAttribute("on`, "javascript:"} {
			if strings.Contains(script, banned) {
				t.Fatalf("%s uses %q", name, banned)
			}
		}
	}
	// The document is built once, from the embeds alone: a stopped plugin and
	// a running one serve the same bytes.
	running := New(&testHost{})
	config, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: true\ncache-path: " + filepath.Join(t.TempDir(), "cache", "snapshot.json") + "\n")})
	if _, err := running.Handle(protocol.MethodPluginRegister, config); err != nil {
		t.Fatal(err)
	}
	defer running.Shutdown()
	for _, p := range []*Plugin{New(&testHost{}), running} {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/quota-cache/status"})
		result, err := p.Handle(protocol.MethodManagementHandle, raw)
		if err != nil || string(result.(protocol.ManagementResponse).Body) != sidebarDocument {
			t.Fatal("the page depends on the plugin's state")
		}
	}
	for _, id := range []string{`<section id="organizations"`, `<section id="api-credits"`, `<section id="renewals"`, `aria-labelledby="accounts-heading"`} {
		if !strings.Contains(sidebarHTML, id) {
			t.Fatalf("the page lost %q", id)
		}
	}
	// The organizations card sits above the API credits card, whose rows it
	// decides, and the editor's cards come after the overview, before Accounts.
	order := []int{strings.Index(sidebarHTML, `class="stats"`), strings.Index(sidebarHTML, `id="organizations"`), strings.Index(sidebarHTML, `id="api-credits"`), strings.Index(sidebarHTML, `id="renewals"`), strings.Index(sidebarHTML, `id="accounts-heading"`)}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("the cards are out of order: %v", order)
		}
	}
}

// The editor saves through one request site that follows the status read's
// rules: never without the console's remembered session for this CPA, never
// with a key CPA refused, the refusal remembered before its answer is read,
// and nothing sent again after a request that got no answer. A save goes
// only from a button press: nothing on a timer calls it.
func TestEditorSavesOnlyFromAPressWithTheConsoleKey(t *testing.T) {
	script := sidebarEditor
	if strings.Count(script, "fetch(") != 1 || strings.Count(script, "await send(") != 3 {
		t.Fatal("expected one request site, used by the three cards' Save")
	}
	if strings.Contains(script, "setInterval") || strings.Count(script, "window.QuotaCachePage") != 1 {
		t.Fatal("the editor must not poll or read any global but the page's namespace")
	}
	send := script[strings.Index(script, "async function send(body)"):]
	send = send[:strings.Index(send, "\n  }\n")]
	request := strings.Index(send, "fetch(")
	for _, guard := range []string{
		"const session = page.readSession();",
		"if (!page.root || !session) return {kind: 'blocked', reason: 'session'};",
		"const held = page.refusal(session);",
		"const hold = refusalHold(held, page.banned);",
		"if (hold && hold.block) return {kind: 'blocked', reason: 'refused', record: held};",
		// Only a refusal that was not about the key is cleared, by this press.
		"if (held) page.forget();",
	} {
		if at := strings.Index(send, guard); at < 0 || at > request {
			t.Fatalf("send lost %q before its request", guard)
		}
	}
	// A key CPA refused, and a lockout still running, hold Save off.
	hold := script[strings.Index(script, "function refusalHold(record, active)"):]
	hold = hold[:strings.Index(hold, "\n  }\n")]
	for _, rule := range []string{
		"if (record.reason === 'refused') return {block:",
		"if (record.reason === 'banned' && active(record)) return {block:",
	} {
		if !strings.Contains(hold, rule) {
			t.Fatalf("refusalHold lost %q", rule)
		}
	}
	for _, rule := range []string{
		"method: 'POST'", "'Content-Type': 'application/json'", "Authorization: ['Bearer', session.key].join(' ')",
		"mode: 'same-origin'", "credentials: 'same-origin'", "cache: 'no-store'", "redirect: 'error'", "page.root + SETTINGS_PATH",
		"} catch {\n      page.unansweredSave();\n      return {kind: 'lost'};",
		"page.unescapeAll(await response.json())",
	} {
		if !strings.Contains(send, rule) {
			t.Fatalf("send lost %q", rule)
		}
	}
	refusal := send[strings.Index(send, "response.status === 401 || response.status === 403"):]
	latch, body, stop := strings.Index(refusal, "page.remember(session, response.status, null)"), strings.Index(refusal, "response.json()"), strings.Index(refusal, "page.refusedSave(record)")
	if latch < 0 || body < latch || stop < body {
		t.Fatal("a refused save must be remembered before its body is read, then stop the page")
	}
	if !strings.Contains(script, "const SETTINGS_PATH = '/v0/management/plugins/quota-cache/settings';") {
		t.Fatal("the editor must save through the management route")
	}
	// Save and Remove are buttons; the forms submit only to Save.
	for _, rule := range []string{
		"on: {submit: event => { event.preventDefault(); saveOrganizations(); }}",
		"on: {submit: event => { event.preventDefault(); saveCredits(); }}",
		"on: {submit: event => { event.preventDefault(); saveRenewals(); }}",
		// Save is off without a session or with a refused key.
		"if (!page.root || !session) return {block: SIGN_IN_FOOT};",
		"const blocked = session.block ||",
		// Leaving with unsaved edits asks, those still to fix included.
		"window.addEventListener('beforeunload'",
		"(credits.editing && creditsPlanNow().unsaved > 0)",
		"(renewals.editing && renewalsPlanNow().unsaved > 0)",
	} {
		if !strings.Contains(script, rule) {
			t.Fatalf("the editor lost %q", rule)
		}
	}
	// The page judges every field itself, so the browser's own checks never
	// stop a Save it offers: every form is novalidate, and no field has a
	// maxlength, which counts UTF-16 units where a label's rule counts
	// characters.
	if forms := strings.Count(script, "h('form', {"); forms != 3 || strings.Count(script, "h('form', {novalidate: true, ") != forms {
		t.Fatalf("every one of the editor's %d forms must be novalidate", forms)
	}
	if strings.Contains(script, "maxlength") {
		t.Fatal("an editor field has a maxlength")
	}
	app := sidebarApp
	for _, rule := range []string{
		// One decoding, of every management string, only on the host's word.
		"if (!escaped || typeof value !== 'string') return value;",
		".replace(/&#39;/g, \"'\").replace(/&amp;/g, '&');",
		"escaped = object(raw) && raw.strings_escaped === true;",
		"const data = decodeStatus(await response.json());",
		// The namespace is frozen and takes one editor.
		"window.QuotaCachePage = Object.freeze({",
		"if (editor !== null) return;",
	} {
		if !strings.Contains(app, rule) {
			t.Fatalf("app.js lost %q", rule)
		}
	}
}
