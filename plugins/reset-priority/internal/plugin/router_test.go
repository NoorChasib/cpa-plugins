package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/route"
)

// routingAt is the instant Quota Cache's api-credits-routing fixtures
// describe. The tests hold the fake clock there, so which organization refills
// soonest, what each has left and how fresh the state is never depend on the
// day they run.
var routingAt = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// quotaCacheFixture reads Quota Cache's routing fixtures, so the router is
// tested against what Quota Cache publishes: "credits" is its publisher's
// golden credit state for the api-credits-routing snapshot at routingAt, and
// "meter" the meter that state was computed from. Of the four linked
// organizations, bravo (refills Oct 16) and delta (Oct 26) can be routed to;
// alpha is out with $20.00 of its estimate left, and charlie is out at $0.00.
// Their keys are …0a to …0d, all proven; …0e is foreign and …10 unmapped.
func quotaCacheFixture(t *testing.T, name string) []byte {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "quota-cache", "internal", "credits", "testdata")
	path := map[string]string{
		"credits": filepath.Join(dir, "golden", "api-credits-routing.credits.json"),
		"meter":   filepath.Join(dir, "snapshots", "api-credits-routing.meter.json"),
	}[name]
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func object(v any, key string) map[string]any { return v.(map[string]any)[key].(map[string]any) }

// routingRig is a registered Runtime on a fake clock whose router reads Quota
// Cache's files from a temporary directory.
type routingRig struct {
	rt     *Runtime
	clk    *clock.Fake
	caller *fakeHostCaller
	path   string // the snapshot path; the credit state and the meter sit beside it
}

// newRoutingRig publishes the fixtures at routingAt and configures with extra
// appended. The loop's first build waits for the clock to move.
func newRoutingRig(t *testing.T, extra string) *routingRig {
	t.Helper()
	caller := newFakeHostCaller()
	clk := clock.NewFake(routingAt)
	g := &routingRig{
		rt:     NewRuntime(caller, WithClock(clk), WithRunAsync(func(f func()) { f() })),
		clk:    clk,
		caller: caller,
		path:   filepath.Join(t.TempDir(), "snapshot.json"),
	}
	t.Cleanup(g.rt.Shutdown)
	g.publish(t, routingAt, nil, nil)
	g.configure(t, extra)
	return g
}

// config keeps the OAuth engine standalone, as use-quota-cache: false does,
// while quota-cache-path points the router at the fixtures.
func (g *routingRig) config(extra string) string {
	return fmt.Sprintf("enabled: true\ndry-run: true\nuse-quota-cache: false\nquota-cache-path: %q\n", g.path) + extra
}

func (g *routingRig) configure(t *testing.T, extra string) hostapi.Registration {
	t.Helper()
	return configureRuntime(t, g.rt, g.config(extra))
}

func configureRuntime(t *testing.T, rt *Runtime, yaml string) hostapi.Registration {
	t.Helper()
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginReconfigure, lifecycleRequest(t, yaml, hostapi.SchemaVersion)))
	if !env.OK {
		t.Fatalf("configure %q: %+v", yaml, env.Error)
	}
	var reg hostapi.Registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

// publish writes the credit state and the meter the way Quota Cache does, each
// a whole file renamed into place: the fixtures as published at at, with the
// meter saved 30 seconds earlier, then changed by the edits. A nil edit keeps
// the fixture.
func (g *routingRig) publish(t *testing.T, at time.Time, credits, meter func(map[string]any)) {
	t.Helper()
	saved := at.Add(-30 * time.Second)
	state := decodeObject(t, quotaCacheFixture(t, "credits"))
	state["computed_at"] = at.Format(time.RFC3339)
	object(state, "inputs")["meter_flushed_at"] = saved.Format(time.RFC3339)
	object(object(state, "api_credits"), "meter")["updatedAtEpoch"] = saved.Unix()
	if credits != nil {
		credits(state)
	}
	g.write(t, qc.CreditsPath(g.path), state)

	m := decodeObject(t, quotaCacheFixture(t, "meter"))
	m["flushed_at"] = saved.Format(time.RFC3339)
	if meter != nil {
		meter(m)
	}
	g.write(t, qc.MeterPath(g.path), m)
}

func (g *routingRig) write(t *testing.T, path string, v any) {
	t.Helper()
	raw, ok := v.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// tick publishes at the clock's time and moves it one loop interval on, so the
// loop rebuilds from what was just published.
func (g *routingRig) tick(t *testing.T, credits, meter func(map[string]any)) {
	t.Helper()
	g.publish(t, g.clk.Now(), credits, meter)
	g.clk.Advance(routerEvery)
}

// status is the JSON status's router object. It must always be there.
func (g *routingRig) status(t *testing.T) route.Status {
	t.Helper()
	return routerStatus(t, g.rt)
}

func routerStatus(t *testing.T, rt *Runtime) route.Status {
	t.Helper()
	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, resp.Body)
	}
	var body struct {
		Router *route.Status `json:"router"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil || body.Router == nil {
		t.Fatalf("status carries no router: %s", resp.Body)
	}
	return *body.Router
}

// keyID is the routing fixture's auth ID for a key, by its last hex digits.
func keyID(suffix string) string { return "claude:apikey:0000000000" + suffix }

// pickRequest is a scheduler.pick request offering the fixture keys named, as
// CPA v8.0.23 marshals pluginapi.SchedulerPickRequest: Go field names, since
// the type has no JSON tags, and the metadata keys every messages pick carries
// (testdata/scheduler-pick-v8.0.23.json at the module root is the real thing).
// sel is the round's previous, failed attempt, "" on its first pick.
func pickRequest(t *testing.T, sel string, suffixes ...string) []byte {
	t.Helper()
	index := map[string]string{"0a": "0", "0b": "1", "0c": "2", "0d": "3", "0e": "4", "10": "5"}
	candidates := []map[string]any{}
	for _, suffix := range suffixes {
		candidates = append(candidates, map[string]any{
			"ID": keyID(suffix), "Provider": "claude", "Priority": 0, "Status": "active",
			"Attributes": map[string]string{
				"auth_kind": "apikey", "source": "config:claude[0000000000" + suffix + "]", "config_index": index[suffix],
			},
			"Metadata": nil,
		})
	}
	metadata := map[string]any{
		"generate":                  true,
		"request_path":              "/v1/messages",
		"requested_model":           "credits-claude-sonnet-5-5",
		"service_tier":              "auto",
		"session_affinity_provider": "mixed",
		"session_affinity_model":    "",
	}
	if sel != "" {
		metadata["selected_auth_id"], metadata["selected_auth_index"] = keyID(sel), "0000000000000000"
	}
	raw, err := json.Marshal(map[string]any{
		"Plugin": map[string]any{"Name": PluginName, "Version": PluginVersion}, "Provider": "", "Providers": []string{"claude"},
		"Model": "credits-claude-sonnet-5-5", "Stream": true,
		"Options": map[string]any{
			"Headers":  map[string][]string{"Authorization": {"Bearer client-access-key-canary"}},
			"Metadata": metadata,
		},
		"Candidates": candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// oauthRequest offers one Claude subscription credential, which is never the
// router's to choose.
func oauthRequest(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"Providers": []string{"claude"}, "Model": "claude-sonnet-5-5",
		"Candidates": []map[string]any{{
			"ID": "claude-someone@example.com.json", "Provider": "claude", "Status": "active",
			"Attributes": map[string]string{"auth_kind": "oauth"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// pick sends one scheduler.pick through Dispatch, which must answer ok.
func pick(t *testing.T, rt *Runtime, raw []byte) hostapi.SchedulerPickResult {
	t.Helper()
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodSchedulerPick, raw))
	if !env.OK {
		t.Fatalf("scheduler.pick answered an error, which fails the client's request: %+v", env.Error)
	}
	var result hostapi.SchedulerPickResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func routedTo(suffix string) hostapi.SchedulerPickResult {
	return hostapi.SchedulerPickResult{Handled: true, AuthID: keyID(suffix)}
}

var unhandled = hostapi.SchedulerPickResult{}

func (c *fakeHostCaller) logged() []hostapi.LogRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hostapi.LogRequest(nil), c.logs...)
}

// routerLines are the router's log lines, in order.
func routerLines(c *fakeHostCaller) []string {
	var lines []string
	for _, line := range c.logged() {
		if strings.HasPrefix(line.Message, "reset-priority API credit rout") {
			lines = append(lines, line.Level+" "+line.Message)
		}
	}
	return lines
}

// capabilitiesOf is the capabilities object as CPA receives it.
func capabilitiesOf(t *testing.T, reg hostapi.Registration) map[string]any {
	t.Helper()
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body.Capabilities
}

// Declaring the scheduler moves every CPA request onto its legacy selection
// path, so it is declared only while routing is on and the plugin enabled.
// Off, the capabilities carry no scheduler key at all, and never the
// across-priorities flag. A configure flips it both ways, the status follows,
// and the change is logged once each way. A rejected configure leaves it
// undeclared.
func TestRegistrationDeclaresTheSchedulerOnlyWhenRouting(t *testing.T) {
	g := newRoutingRig(t, "")
	declared := func(extra string, want bool) {
		t.Helper()
		capabilities := capabilitiesOf(t, g.configure(t, extra))
		if _, across := capabilities["scheduler_across_priorities"]; across || capabilities["management_api"] != true {
			t.Fatalf("%q declared %v", extra, capabilities)
		}
		scheduler, present := capabilities["scheduler"]
		if want && scheduler != true || !want && present {
			t.Fatalf("%q declared %v; want scheduler %v", extra, capabilities, want)
		}
		if s := g.status(t); s.Declared != want || s.Enabled != want {
			t.Fatalf("%q: status says declared %v, enabled %v", extra, s.Declared, s.Enabled)
		}
	}
	declared("", false)
	declared("route-api-credits: true\n", true)
	declared("route-api-credits: true\nreconcile-interval: 2h\n", true)
	declared("route-api-credits: false\n", false)
	declared("route-api-credits: true\n", true)
	// A disabled plugin declares nothing, whatever the setting says.
	reg := configureRuntime(t, g.rt, "enabled: false\nroute-api-credits: true\n")
	if _, present := capabilitiesOf(t, reg)["scheduler"]; present {
		t.Fatalf("a disabled plugin declared the scheduler: %v", capabilitiesOf(t, reg))
	}
	if s := g.status(t); s.Declared || s.Enabled {
		t.Fatalf("a disabled plugin is routing: %+v", s)
	}
	declared("route-api-credits: true\n", true)
	// A rejected configure leaves nothing declared.
	env := decodeEnvelope(t, g.rt.Dispatch(hostapi.MethodPluginReconfigure,
		lifecycleRequest(t, g.config("route-api-credits: true\npriority-step: 0\n"), hostapi.SchemaVersion)))
	if env.OK {
		t.Fatal("priority-step: 0 was accepted")
	}
	if s := g.status(t); s.Declared {
		t.Fatal("a rejected configure left the scheduler declared")
	}
	env = decodeEnvelope(t, g.rt.Dispatch(hostapi.MethodPluginReconfigure,
		lifecycleRequest(t, g.config("route-api-credits: sometimes\n"), hostapi.SchemaVersion)))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_config" {
		t.Fatalf("route-api-credits: sometimes: %+v", env)
	}

	want := []string{
		"info " + logRoutingOn, "info " + logRoutingOff, "info " + logRoutingOn,
		"info " + logRoutingOff, "info " + logRoutingOn,
	}
	if got := routerLines(g.caller); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routing lines = %q; want %q", got, want)
	}
}

// The loop rebuilds the table once a minute from Quota Cache's published state
// and meter: it goes active from the fixtures and routes, and each failure to
// read or trust the state is the router's reason until it is fixed. With
// routing off the status still says whether turning it on would route.
func TestRouterFollowsTheLoop(t *testing.T) {
	g := newRoutingRig(t, "route-api-credits: true\n")
	if s := g.status(t); s.Active || s.TableReason != "no_table" || !s.Declared {
		t.Fatalf("before the first build: %+v", s)
	}
	g.clk.Advance(0)
	s := g.status(t)
	if !s.Active || s.Reason != "" || s.TableReason != "" || s.BuiltAt == nil || !s.BuiltAt.Equal(routingAt) {
		t.Fatalf("active router: %+v", s)
	}
	type org struct {
		Label, Reason, Estimate string
		Rank                    int
		Eligible, Recheck       bool
	}
	want := []org{
		{"bravo", "", "", 1, true, false},
		{"delta", "", "", 2, true, false},
		{"alpha", "out", "$20.00", 0, false, true},
		{"charlie", "out", "$0.00", 0, false, false},
	}
	var got []org
	for _, o := range s.Orgs {
		got = append(got, org{o.Label, o.Reason, o.EstimateLeftText, o.Rank, o.Eligible, o.Recheck})
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("orgs = %+v; want %+v", got, want)
	}
	if s.MappedKeys != 4 || s.UnmappedKeys != 1 || s.ForeignKeys != 1 || s.FailingKeys != 0 || s.StaleKeys != 0 {
		t.Fatalf("keys: %+v", s)
	}

	all := []string{"0a", "0b", "0c", "0d"}
	if got := pick(t, g.rt, pickRequest(t, "", all...)); got != routedTo("0a") {
		t.Fatalf("first pick = %+v; want alpha re-checked", got)
	}
	// The re-check failed; the same round's next pick takes the soonest
	// refill with credit left.
	if got := pick(t, g.rt, pickRequest(t, "0a", "0b", "0c", "0d")); got != routedTo("0b") {
		t.Fatalf("failover pick = %+v", got)
	}
	if got := pick(t, g.rt, pickRequest(t, "", all...)); got != routedTo("0b") {
		t.Fatalf("next request = %+v", got)
	}
	s = g.status(t)
	if c := s.Counters; c.Picks != 3 || c.Routed != 2 || c.Rechecked != 1 ||
		s.LastRoutedOrg != qc.APICreditOrgAccount("00000000-0000-4000-8000-00000000000b") {
		t.Fatalf("after three picks: %+v, last routed %q", c, s.LastRoutedOrg)
	}

	inactive := func(want string) {
		t.Helper()
		s := g.status(t)
		if s.Active || s.TableReason != want || s.Reason != want {
			t.Fatalf("router = %+v; want %s", s, want)
		}
		if got := pick(t, g.rt, pickRequest(t, "", "0b", "0d")); got != unhandled {
			t.Fatalf("%s: pick = %+v", want, got)
		}
	}
	// Quota Cache older than 0.2.0 publishes no credit state.
	g.publish(t, g.clk.Now(), nil, nil)
	if err := os.Remove(qc.CreditsPath(g.path)); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(routerEvery)
	inactive("credits_missing")
	g.publish(t, g.clk.Now(), nil, nil)
	g.write(t, qc.CreditsPath(g.path), []byte(`{"schema":1,`))
	g.clk.Advance(routerEvery)
	inactive("credits_unreadable")
	// A state Quota Cache stopped keeping current: published 3 minutes before
	// this build and then not again.
	g.publish(t, g.clk.Now().Add(-3*time.Minute), nil, nil)
	g.clk.Advance(routerEvery)
	inactive("credits_stale")
	g.tick(t, func(c map[string]any) { object(c, "settings")["state"] = qc.CreditSettingsUnreadable }, nil)
	inactive("settings_unavailable")
	g.publish(t, g.clk.Now(), nil, nil)
	if err := os.Remove(qc.MeterPath(g.path)); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(routerEvery)
	inactive("meter_missing")
	g.tick(t, nil, nil)
	if s := g.status(t); !s.Active {
		t.Fatalf("the restored files did not route again: %+v", s)
	}
	if got := pick(t, g.rt, pickRequest(t, "", "0b", "0d")); got != routedTo("0b") {
		t.Fatalf("after recovering: %+v", got)
	}

	// Routing off still builds the table, so the status says it would route.
	g.configure(t, "route-api-credits: false\n")
	g.clk.Advance(0)
	if s := g.status(t); s.Enabled || s.Declared || s.Active || s.Reason != "disabled" || s.TableReason != "" {
		t.Fatalf("routing off: %+v", s)
	}
	if got := pick(t, g.rt, pickRequest(t, "", "0b")); got != unhandled {
		t.Fatalf("routing off: pick = %+v", got)
	}

	lines := []string{
		"info " + logRoutingOn,
		"info " + logRouterActive,
		"info " + fmt.Sprintf(logRouterInactive, "credits_missing"),
		"info " + logRouterActive,
		"info " + logRoutingOff,
		"info " + fmt.Sprintf(logRouterInactive, "disabled"),
	}
	if got := routerLines(g.caller); fmt.Sprint(got) != fmt.Sprint(lines) {
		t.Fatalf("router lines = %q; want %q", got, lines)
	}
}

// CPA fails the client's request on any scheduler error, so scheduler.pick
// answers handled:false, never an error, in every state the plugin can be in.
func TestSchedulerPickIsHandledInEveryState(t *testing.T) {
	request := pickRequest(t, "", "0b", "0d")
	declined := func(rt *Runtime, reason string) uint64 {
		return rt.router.Status(routingAt).Counters.Declined[reason]
	}

	// Before the first configure.
	fresh := NewRuntime(newFakeHostCaller(), WithClock(clock.NewFake(routingAt)))
	t.Cleanup(fresh.Shutdown)
	if got := pick(t, fresh, request); got != unhandled || declined(fresh, "disabled") != 1 {
		t.Fatalf("before the first configure: %+v", got)
	}

	// Routing on, before the loop's first build.
	g := newRoutingRig(t, "route-api-credits: true\n")
	if got := pick(t, g.rt, request); got != unhandled || declined(g.rt, "no_table") != 1 {
		t.Fatalf("before the first build: %+v", got)
	}
	g.clk.Advance(0)
	if got := pick(t, g.rt, request); got != routedTo("0b") {
		t.Fatalf("once built: %+v", got)
	}
	// Routing off.
	g.configure(t, "route-api-credits: false\n")
	if got := pick(t, g.rt, request); got != unhandled || declined(g.rt, "disabled") != 1 {
		t.Fatalf("routing off: %+v", got)
	}
	// The plugin disabled.
	configureRuntime(t, g.rt, "enabled: false\nroute-api-credits: true\n")
	if got := pick(t, g.rt, request); got != unhandled || declined(g.rt, "disabled") != 2 {
		t.Fatalf("plugin disabled: %+v", got)
	}
	// After quiesce: the table is withdrawn until a configure builds one.
	g.configure(t, "route-api-credits: true\n")
	g.clk.Advance(0)
	if got := pick(t, g.rt, request); got != routedTo("0b") {
		t.Fatalf("routing again: %+v", got)
	}
	if env := decodeEnvelope(t, g.rt.Dispatch(hostapi.MethodPluginQuiesce, nil)); !env.OK {
		t.Fatalf("quiesce: %+v", env.Error)
	}
	g.clk.Advance(time.Hour)
	if got := pick(t, g.rt, request); got != unhandled || declined(g.rt, "no_table") != 2 {
		t.Fatalf("after quiesce: %+v", got)
	}
	// After terminal shutdown Dispatch is closed, but the native path calls
	// SchedulerPick directly, and it still declines.
	g.rt.Shutdown()
	if d := g.rt.SchedulerPick(request); d.AuthID != "" || d.Reason != route.ReasonDisabled || declined(g.rt, "disabled") != 3 {
		t.Fatalf("after shutdown: %+v", d)
	}
}

// The JSON status always carries router beside the engine's snapshot, and
// the HTML status a "Claude API credit routing" section. Both name an
// organization by its label, escaped on the page, and neither ever carries an
// auth ID, an auth index or an organization UUID.
func TestStatusCarriesRouter(t *testing.T) {
	plain := newTestRuntime(t, newFakeHostCaller())
	registerRuntime(t, plain, "enabled: true\n")
	resp := managementCall(t, plain, "GET", "/v0/management/plugins/reset-priority/status", nil)
	fields := decodeObject(t, resp.Body)
	for _, key := range []string{"generated_at", "enabled", "providers", "router"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("status lacks %q: %s", key, resp.Body)
		}
	}
	s := routerStatus(t, plain)
	if s.Enabled || s.Declared || s.Reason != "disabled" || s.TableReason != "no_table" ||
		s.Orgs == nil || s.Exploring == nil || len(s.Counters.Declined) != 20 {
		t.Fatalf("an idle router: %+v", s)
	}
	configureRuntime(t, plain, "enabled: false\n")
	if s := routerStatus(t, plain); s.Enabled || s.TableReason != "no_table" {
		t.Fatalf("a disabled plugin's router: %+v", s)
	}

	const hostile = `<script>alert("x")</script>`
	g := newRoutingRig(t, "route-api-credits: true\n")
	g.publish(t, routingAt, func(c map[string]any) {
		accounts := object(c, "api_credits")["accounts"].([]any)
		accounts[0].(map[string]any)["label"] = hostile
	}, nil)
	g.clk.Advance(0)
	pick(t, g.rt, pickRequest(t, "", "0a", "0b", "0c", "0d"))
	pick(t, g.rt, oauthRequest(t))

	s = g.status(t)
	labels := []string{}
	for _, o := range s.Orgs {
		labels = append(labels, o.Label)
	}
	if fmt.Sprint(labels) != fmt.Sprint([]string{"bravo", "delta", hostile, "charlie"}) || len(s.Exploring) != 1 || s.Counters.Picks != 2 {
		t.Fatalf("router: labels %q, %+v", labels, s)
	}
	page := managementCall(t, g.rt, "GET", "/v0/management/plugins/reset-priority/status/html", nil)
	if page.StatusCode != 200 {
		t.Fatalf("status page: %d", page.StatusCode)
	}
	html := string(page.Body)
	for _, want := range []string{
		"Claude API credit routing", "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;", "routing on", "scheduler declared",
		"<code>recheck</code>", "<code>out</code>", "$40.00", "<code>out_of_scope</code> 1",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	if strings.Contains(html, "<script>alert") {
		t.Error("status page renders a label unescaped")
	}
	body := string(managementCall(t, g.rt, "GET", "/v0/management/plugins/reset-priority/status", nil).Body)
	for _, secret := range []string{
		"claude:apikey:", "00000000-0000-4000-8000-", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "1010101010101010",
		"client-access-key-canary", "someone@example.com",
	} {
		if strings.Contains(body, secret) || strings.Contains(html, secret) {
			t.Errorf("the status carries %q", secret)
		}
	}
}

// Picks run on CPA's request goroutines while configures toggle routing and
// restart the loop, whose builds run on their own goroutines on the real
// clock. None of them waits on another, every pick is answered, and each is
// counted once.
func TestPickDuringReconfigureAndLoop(t *testing.T) {
	caller := newFakeHostCaller()
	rt := NewRuntime(caller, WithRunAsync(func(f func()) { f() }))
	t.Cleanup(rt.Shutdown)
	g := &routingRig{rt: rt, caller: caller, path: filepath.Join(t.TempDir(), "snapshot.json")}
	fresh := func() {
		g.publish(t, time.Now().UTC().Truncate(time.Second), func(c map[string]any) { c["valid_until"] = nil }, nil)
	}
	fresh()
	g.configure(t, "route-api-credits: true\n")
	requests := [][]byte{
		pickRequest(t, "", "0a", "0b", "0c", "0d", "10"),
		pickRequest(t, "0b", "0c", "0d"),
		oauthRequest(t),
		[]byte(`{"Candidates":`),
	}

	stop := make(chan struct{})
	var picks atomic.Uint64
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := i; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodSchedulerPick, requests[n%len(requests)]))
				if !env.OK {
					t.Errorf("pick answered an error: %+v", env.Error)
					return
				}
				picks.Add(1)
				if n%16 == 0 {
					rt.router.Status(time.Now())
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 20 {
			fresh()
			env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginReconfigure,
				lifecycleRequest(t, g.config(fmt.Sprintf("route-api-credits: %t\n", i%2 == 0)), hostapi.SchemaVersion)))
			if !env.OK {
				t.Errorf("configure %d: %+v", i, env.Error)
				return
			}
			if i%5 == 4 {
				rt.Quiesce()
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("configures and loop runs deadlocked against picks")
	}
	close(stop)
	finished := make(chan struct{})
	go func() { workers.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a pick never returned")
	}
	c := rt.router.Status(time.Now()).Counters
	sum := c.Routed + c.Explored + c.Rechecked
	for _, n := range c.Declined {
		sum += n
	}
	if picks.Load() == 0 || c.Picks != picks.Load() || sum != c.Picks {
		t.Fatalf("%d picks returned; counters %+v", picks.Load(), c)
	}
	t.Logf("%d picks: %d routed, %d explored, %d re-checked, declined %v", c.Picks, c.Routed, c.Explored, c.Rechecked, c.Declined)
}

// A panic while building the table must not escape the loop's goroutine,
// which would abort CPA. The router steps aside with build_failed, the loop
// keeps its cadence, and the next good build brings it back. The warning is
// logged once per configure, a good build in between notwithstanding, and
// never with the panic value.
func TestLoopSurvivesARouterPanic(t *testing.T) {
	g := newRoutingRig(t, "route-api-credits: true\n")
	g.clk.Advance(0)
	const canary = "PANIC-VALUE-CANARY claude:apikey:00000000000b"
	hook := func() { panic(canary) }
	warnings := func() int {
		n := 0
		for _, line := range g.caller.logged() {
			if strings.Contains(line.Message, "CANARY") || strings.Contains(line.Message, "claude:apikey:") {
				t.Fatalf("a log line carries the panic value: %+v", line)
			}
			if line.Message == logRouterNotBuilt {
				if line.Level != "warn" {
					t.Fatalf("build failure logged at %q", line.Level)
				}
				n++
			}
		}
		return n
	}
	g.rt.loop.beforeBuild.Store(&hook)
	g.tick(t, nil, nil)
	if s := g.status(t); s.Active || s.TableReason != "build_failed" || s.Counters.BuildPanics != 1 {
		t.Fatalf("after the panic: %+v", s)
	}
	if got := pick(t, g.rt, pickRequest(t, "", "0b")); got != unhandled {
		t.Fatalf("a pick routed with no table: %+v", got)
	}
	g.tick(t, nil, nil)
	if s := g.status(t); s.Counters.BuildPanics != 2 {
		t.Fatalf("the loop stopped after the panic: %+v", s)
	}
	if n := warnings(); n != 1 {
		t.Fatalf("%d build-failure warnings after two panics", n)
	}

	g.rt.loop.beforeBuild.Store(nil)
	g.tick(t, nil, nil)
	if s := g.status(t); !s.Active || s.TableReason != "" || s.Counters.BuildPanics != 2 {
		t.Fatalf("the next build did not recover: %+v", s)
	}
	if got := pick(t, g.rt, pickRequest(t, "", "0b")); got != routedTo("0b") {
		t.Fatalf("after recovering: %+v", got)
	}

	// A panic after the recovery is still the same configure.
	g.rt.loop.beforeBuild.Store(&hook)
	g.tick(t, nil, nil)
	if s := g.status(t); s.TableReason != "build_failed" || s.Counters.BuildPanics != 3 {
		t.Fatalf("after the panic that followed a good build: %+v", s)
	}
	if n := warnings(); n != 1 {
		t.Fatalf("%d build-failure warnings within one configure", n)
	}

	// Another configure arms the warning again.
	g.configure(t, "route-api-credits: true\n")
	g.tick(t, nil, nil)
	if n := warnings(); n != 2 {
		t.Fatalf("%d build-failure warnings over two configures", n)
	}
}

// panickyClock hands out timers whose Stop panics.
type panickyClock struct{ clock.Clock }

type panickyTimer struct{}

func (c panickyClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	c.Clock.AfterFunc(d, f)
	return panickyTimer{}
}

func (panickyTimer) Stop() bool { panic("timer stop") }

// Quiesce and shutdown stop the loop and withdraw the table, and keep the
// exploration history for a rollback configure after a quiesce; shutdown also
// turns routing off. Shutdown runs from the native export with nothing above
// it to recover, so neither may panic, even when stopping the loop does.
func TestStopIsPanicProof(t *testing.T) {
	g := newRoutingRig(t, "route-api-credits: true\n")
	g.clk.Advance(0)
	if got := pick(t, g.rt, pickRequest(t, "", "0a", "0b")); got != routedTo("0a") {
		t.Fatalf("the re-check did not claim an exploration slot: %+v", got)
	}
	if env := decodeEnvelope(t, g.rt.Dispatch(hostapi.MethodPluginQuiesce, nil)); !env.OK {
		t.Fatalf("quiesce: %+v", env.Error)
	}
	if s := g.status(t); s.TableReason != "no_table" || !s.Enabled || len(s.Exploring) != 1 {
		t.Fatalf("after quiesce: %+v", s)
	}
	g.tick(t, nil, nil)
	g.clk.Advance(time.Hour)
	if s := g.status(t); s.TableReason != "no_table" {
		t.Fatalf("the loop kept building after quiesce: %+v", s)
	}
	g.rt.Shutdown()
	if s := g.rt.router.Status(g.clk.Now()); s.Enabled || s.TableReason != "no_table" || len(s.Exploring) != 1 {
		t.Fatalf("after shutdown: %+v", s)
	}

	caller := newFakeHostCaller()
	clk := clock.NewFake(routingAt)
	rt := NewRuntime(caller, WithClock(clk), WithRunAsync(func(f func()) { f() }))
	rt.loop.clk = panickyClock{clk}
	h := &routingRig{rt: rt, clk: clk, caller: caller, path: filepath.Join(t.TempDir(), "snapshot.json")}
	h.publish(t, routingAt, nil, nil)
	registerRuntime(t, rt, h.config("route-api-credits: true\n"))
	clk.Advance(0)
	if s := routerStatus(t, rt); !s.Active {
		t.Fatalf("not routing: %+v", s)
	}
	rt.Quiesce()
	if s := rt.router.Status(clk.Now()); s.TableReason != "no_table" {
		t.Fatalf("a panicking loop stop left the table: %+v", s)
	}
	rt.Shutdown()
	if s := rt.router.Status(clk.Now()); s.Enabled || s.TableReason != "no_table" {
		t.Fatalf("after shutdown: %+v", s)
	}
}

// A quiesce that arrives while a build is reading Quota Cache's files waits
// for that build, so the table it publishes is withdrawn too: picks must not
// keep routing on a table nobody rebuilds.
func TestStopWaitsForABuildInFlight(t *testing.T) {
	g := newRoutingRig(t, "route-api-credits: true\n")
	g.clk.Advance(0)
	if got := pick(t, g.rt, pickRequest(t, "", "0b")); got != routedTo("0b") {
		t.Fatalf("not routing: %+v", got)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func() {
		once.Do(func() { close(entered) })
		<-release
	}
	g.rt.loop.beforeBuild.Store(&hook)
	g.publish(t, g.clk.Now(), nil, nil)
	built := make(chan struct{})
	go func() {
		defer close(built)
		g.clk.Advance(routerEvery)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the loop's next build never started")
	}

	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		g.rt.Quiesce()
	}()
	select {
	case <-quiesced:
		close(release)
		t.Fatal("Quiesce returned while a build was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for name, done := range map[string]chan struct{}{"the build": built, "Quiesce": quiesced} {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never returned once the build was released", name)
		}
	}
	if s := g.status(t); s.TableReason != "no_table" || s.Active {
		t.Fatalf("the build in flight published after quiesce: %+v", s)
	}
	if got := pick(t, g.rt, pickRequest(t, "", "0b")); got != unhandled {
		t.Fatalf("a pick routed after quiesce: %+v", got)
	}
}

// Declared, and unable to route for a reason that needs the operator, the
// plugin says so once, InactiveWarnAfter after the reason began, with the
// reason in the message, and not again however long the reason lasts. Each
// lasting reason warns; the passing ones never do, and nothing warns while
// routing is off. Another configure arms the warning again.
func TestLastingInactiveWarnsOnce(t *testing.T) {
	removeFile := func(path func(string) string) func(*routingRig, *testing.T) {
		return func(g *routingRig, t *testing.T) {
			g.publish(t, g.clk.Now(), nil, nil)
			if err := os.Remove(path(g.path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	corrupt := func(path func(string) string) func(*routingRig, *testing.T) {
		return func(g *routingRig, t *testing.T) {
			g.publish(t, g.clk.Now(), nil, nil)
			g.write(t, path(g.path), []byte("{"))
		}
	}
	edit := func(credits, meter func(map[string]any)) func(*routingRig, *testing.T) {
		return func(g *routingRig, t *testing.T) { g.publish(t, g.clk.Now(), credits, meter) }
	}
	// Turning Quota Cache off on CPA's Plugins page shuts it down.
	stopped := func(m map[string]any) {
		m["stopped_at"], m["stop_reason"] = m["flushed_at"], qc.MeterStopShutdown
	}
	for _, tc := range []struct {
		name string
		// each publishes what the next build reads; nil publishes nothing.
		each func(*routingRig, *testing.T)
		// first, when set, publishes once before the first build instead.
		first  func(*routingRig, *testing.T)
		warnAt time.Duration // after the state first applies; 0 never
	}{
		{name: "credits_missing", each: removeFile(qc.CreditsPath), warnAt: 31 * time.Minute},
		{name: "credits_unreadable", each: corrupt(qc.CreditsPath), warnAt: 31 * time.Minute},
		// Quota Cache turned off: its final state carries the stopped meter,
		// which routes as meter_stopped until the state is older than
		// MaxCreditStateAge, and credits_stale from then on.
		{name: "credits_stale", first: func(g *routingRig, t *testing.T) {
			g.publish(t, g.clk.Now(), func(c map[string]any) {
				meter := object(object(c, "api_credits"), "meter")
				meter["stoppedAtEpoch"], meter["stopReason"] = meter["updatedAtEpoch"], qc.MeterStopShutdown
			}, stopped)
		}, warnAt: 34 * time.Minute},
		{name: "settings_unavailable", each: edit(func(c map[string]any) {
			object(c, "settings")["state"] = qc.CreditSettingsUnreadable
		}, nil), warnAt: 31 * time.Minute},
		{name: "meter_missing", each: removeFile(qc.MeterPath), warnAt: 31 * time.Minute},
		{name: "meter_stopped", each: edit(nil, stopped), warnAt: 31 * time.Minute},
		{name: "meter_too_old", each: edit(nil, func(m map[string]any) { delete(m, "auth_ids_since") }), warnAt: 31 * time.Minute},
		{name: "no_api_credits", each: edit(func(c map[string]any) { c["api_credits"] = nil }, nil), warnAt: 31 * time.Minute},
		{name: "no_linked_orgs", each: edit(func(c map[string]any) {
			for _, account := range object(c, "api_credits")["accounts"].([]any) {
				account.(map[string]any)["organizationId"] = ""
			}
		}, nil), warnAt: 31 * time.Minute},
		{name: "meter_unreadable", each: corrupt(qc.MeterPath)},
		{name: "meter_stale", each: edit(nil, func(m map[string]any) {
			flushed, _ := time.Parse(time.RFC3339, m["flushed_at"].(string))
			m["flushed_at"] = flushed.Add(-20 * time.Minute).Format(time.RFC3339)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, routing := range []bool{true, false} {
				g := newRoutingRig(t, fmt.Sprintf("route-api-credits: %t\n", routing))
				g.clk.Advance(0)
				start := g.clk.Now()
				if tc.first != nil {
					tc.first(g, t)
				}
				var warned []time.Duration
				seen := 0
				// Past twice InactiveWarnAfter, so a warning that comes again
				// fails the case.
				for minute := 1; minute <= 100; minute++ {
					if tc.each != nil {
						tc.each(g, t)
					}
					g.clk.Advance(routerEvery)
					if minute == 1 && tc.first == nil {
						if s := g.status(t); s.TableReason != tc.name {
							t.Fatalf("table reason %q; want %q", s.TableReason, tc.name)
						}
					}
					lines := g.caller.logged()
					for _, line := range lines[seen:] {
						if strings.Contains(line.Message, "cannot route") {
							if line.Level != "warn" || line.Message != fmt.Sprintf(logRoutingCannotGo, tc.name) {
								t.Fatalf("warning %+v; want one for %s", line, tc.name)
							}
							warned = append(warned, g.clk.Now().Sub(start))
						}
					}
					seen = len(lines)
				}
				switch {
				case !routing || tc.warnAt == 0:
					if len(warned) != 0 {
						t.Fatalf("routing %v: warned at %v", routing, warned)
					}
				case len(warned) != 1 || warned[0] != tc.warnAt:
					t.Fatalf("warned at %v; want once at %v", warned, tc.warnAt)
				}
				if s := g.status(t); s.TableReason != tc.name {
					t.Fatalf("table reason %q at the end; want %q", s.TableReason, tc.name)
				}
			}
		})
	}

	// Another configure arms it again.
	g := newRoutingRig(t, "route-api-credits: true\n")
	tooOld := func(m map[string]any) { delete(m, "auth_ids_since") }
	warnings := func() int {
		n := 0
		for _, line := range g.caller.logged() {
			if line.Message == fmt.Sprintf(logRoutingCannotGo, "meter_too_old") {
				n++
			}
		}
		return n
	}
	for range 70 {
		g.tick(t, nil, tooOld)
	}
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings over 70 minutes of one configure", n)
	}
	g.configure(t, "route-api-credits: true\nreconcile-interval: 2h\n")
	for range 29 {
		g.tick(t, nil, tooOld)
	}
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings before the second configure's 30 minutes", n)
	}
	for range 2 {
		g.tick(t, nil, tooOld)
	}
	if n := warnings(); n != 2 {
		t.Fatalf("%d warnings after another configure", n)
	}
	for range 40 {
		g.tick(t, nil, tooOld)
	}
	if n := warnings(); n != 2 {
		t.Fatalf("%d warnings over 71 minutes of the second configure", n)
	}
}

// CPA's Plugins page draws its settings form from ConfigFields and saves only
// what it lists, so every key the decoder knows is either a field or excluded
// here on purpose. Every field is a key the decoder knows.
func TestConfigFieldsCoverEveryKey(t *testing.T) {
	excluded := map[string]string{
		"codex-reset-window-activation": "accepted and ignored with a warning",
		"refresh-interval":              "an alias of reconcile-interval",
		"providers":                     "an alias of manage-claude and manage-codex",
	}
	fields := map[string]hostapi.ConfigField{}
	for _, field := range configFields() {
		if _, dup := fields[field.Name]; dup {
			t.Errorf("config field %q is listed twice", field.Name)
		}
		fields[field.Name] = field
		switch field.Type {
		case "boolean", "integer", "number", "string", "enum", "array", "object":
		default:
			t.Errorf("config field %q has type %q, which the console does not draw", field.Name, field.Type)
		}
	}
	keys := map[string]bool{}
	for _, key := range config.Keys() {
		keys[key] = true
		_, listed := fields[key]
		if _, skip := excluded[key]; listed == skip {
			t.Errorf("key %q: listed %v, excluded %v", key, listed, skip)
		}
	}
	for name := range fields {
		if !keys[name] {
			t.Errorf("config field %q is not a key the decoder knows", name)
		}
	}
	for name := range excluded {
		if !keys[name] {
			t.Errorf("excluded %q is not a key the decoder knows", name)
		}
	}

	routing := fields["route-api-credits"]
	if routing.Type != "boolean" || !strings.Contains(routing.Description, "default false") || !strings.Contains(routing.Description, "Quota Cache 0.2.0") {
		t.Errorf("route-api-credits field: %+v", routing)
	}
	// The form shows an absent boolean as off, so a key that defaults to true
	// says so.
	for _, name := range []string{"manage-claude", "manage-codex"} {
		if !strings.Contains(fields[name].Description, "Default true; this form shows it off until you save it once") {
			t.Errorf("%s field: %q", name, fields[name].Description)
		}
	}
}

// The form shows a boolean the configuration leaves out as off, and saves only
// the fields the operator changed. So a boolean whose absence is not false,
// alone or beside a custom quota-cache-path, which turns use-quota-cache on,
// says the form shows it off, and every other boolean does not.
// use-quota-cache also says how to save it as false there.
func TestBooleanFieldsSayWhenTheFormShowsThemOff(t *testing.T) {
	parse := func(yaml string) config.Config {
		t.Helper()
		cfg, err := config.Parse([]byte(yaml))
		if err != nil {
			t.Fatalf("parse %q: %v", yaml, err)
		}
		return cfg
	}
	for _, field := range configFields() {
		if field.Type != "boolean" {
			continue
		}
		absentIsOn := false
		for _, base := range []string{"", "quota-cache-path: /data/quota.json\n"} {
			if !reflect.DeepEqual(parse(base), parse(base+field.Name+": false\n")) {
				absentIsOn = true
			}
		}
		if says := strings.Contains(field.Description, "this form shows it off"); says != absentIsOn {
			t.Errorf("%s: absent differs from false %v, description says the form shows it off %v: %q", field.Name, absentIsOn, says, field.Description)
		}
	}
	for _, field := range configFields() {
		if field.Name == "use-quota-cache" && !strings.Contains(field.Description, "switch this on and off again and save, so false is written") {
			t.Errorf("use-quota-cache field does not say how to save it as false: %q", field.Description)
		}
	}
}

// The console opens that form, Configure Reset Priority, from Reset
// Priority's Edit config button on its Plugins page; it has no Plugins →
// Reset Priority → Settings path. The docs send operators there in the
// console's own words.
func TestDocsNameTheEditConfigButton(t *testing.T) {
	root := filepath.Join("..", "..")
	paths, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(root, "README.md"), filepath.Join(root, "REFERENCE.md"), filepath.Join(root, "config.example.yaml"))
	opened := map[string]int{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(strings.Join(strings.Fields(string(raw)), " "), "**", "")
		for _, invented := range []string{"Plugins →", "Plugins >", "→ Settings"} {
			if strings.Contains(text, invented) {
				t.Errorf("%s says %q, a path the console does not have", path, invented)
			}
		}
		named := strings.Count(text, "Edit config")
		opened[filepath.Base(path)] = strings.Count(text, "CPA's Plugins page, open Reset Priority's Edit config")
		if named != opened[filepath.Base(path)] {
			t.Errorf("%s names Edit config %d times, %d of them as \"on CPA's Plugins page, open Reset Priority's Edit config\"", path, named, opened[filepath.Base(path)])
		}
	}
	for _, name := range []string{"README.md", "REFERENCE.md"} {
		if opened[name] == 0 {
			t.Errorf("%s never says where the console edits Reset Priority's configuration", name)
		}
	}
}
