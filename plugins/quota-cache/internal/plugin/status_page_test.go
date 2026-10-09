package plugin

import (
	"encoding/json"
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
	script := sidebarScript
	if strings.Count(script, "fetch(") != 1 || strings.Count(script, "refresh(true)") != 1 {
		t.Fatal("expected one request site, resumed by Refresh view alone")
	}
	if !strings.Contains(script, "addEventListener('click', () => refresh(true))") ||
		!strings.Contains(script, "if ($('auto').checked && !document.hidden) refresh(false); }, 30000)") {
		t.Fatal("the timer must never resume after a refusal")
	}
	refresh := script[strings.Index(script, "async function refresh(manual)"):]
	request := strings.Index(refresh, "fetch(")
	for _, guard := range []string{
		"if (busy || (stopped && !manual)) return;",
		"if (!session) { syncButton(null); unavailable(SIGN_IN,",
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
}

// Claude API credit entries have a name, a filter, an endpoint and a reading
// of their own, and the page prints Anthropic's amounts without adding them up.
func TestStatusPageShowsClaudeAPICredits(t *testing.T) {
	script := sidebarScript
	for _, rule := range []string{
		"'anthropic-api': 'Claude API credits'",
		"'anthropic-api': 'api.anthropic.com/v1/organizations/cost_report'",
		"item.api_credit.label",
		"const noWeekly = ['openrouter', 'anthropic-api'];",
		"const label = problem ? 'Not polled' :",
		"if (entry.provider === 'anthropic-api') return creditDetails(entry, now);",
		"' (lowest units)'",
		"'Today not reported yet'",
		"claude-api-credits",
	} {
		if !strings.Contains(script, rule) {
			t.Fatalf("page script lost %q", rule)
		}
	}
	credit := script[strings.Index(script, "function creditDetails("):strings.Index(script, "function extendedQuota(")]
	for _, arithmetic := range []string{"Number(", "parseFloat", "reduce(", " + Number", "+= "} {
		if strings.Contains(credit, arithmetic) {
			t.Fatalf("the credit reading does arithmetic (%q); totals are Quota Glance's", arithmetic)
		}
	}
	if !strings.Contains(sidebarHTML, `<option value="anthropic-api">Claude API credits</option>`) || !strings.Contains(sidebarHTML, "claude-api-credits") {
		t.Fatal("the provider filter or the empty state lacks Claude API credits")
	}
}
