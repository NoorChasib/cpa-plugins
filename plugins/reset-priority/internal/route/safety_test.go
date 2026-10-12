package route

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A panic anywhere in a pick, even with explore.mu held, answers
// ReasonPanic, is counted, and leaves the router working: the mutex is
// released by defer, so nothing waits on it afterwards.
func TestPickRecoversFromAPanic(t *testing.T) {
	f := worked(t)
	r := routing(t, f.input())
	now := f.Now.Add(42 * time.Second)
	boom := func() { panic("hook") }

	r.beforeDecide.Store(&boom)
	expect(t, r, request(t, "", "0b"), now, decline(ReasonPanic))
	if c := r.Status(now).Counters; c.Picks != 1 || c.Declined["panic"] != 1 {
		t.Fatalf("counters after a panic = %+v", c)
	}
	r.beforeDecide.Store(nil)
	expect(t, r, request(t, "", "0b"), now, routed("0b"))

	r.beforeClaim.Store(&boom)
	expect(t, r, request(t, "", "0b", "12"), now, decline(ReasonPanic))
	r.beforeClaim.Store(nil)

	within := func(name string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			fn()
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s did not return within a second after a panic under explore.mu", name)
		}
	}
	within("Pick", func() { r.Pick(request(t, "", "0b", "12"), now) })
	within("Publish", func() { r.Publish(Build(f.input()), now) })
	within("Status", func() { r.Status(now) })
	if c := r.Status(now).Counters; c.Declined["panic"] != 2 || c.Explored != 1 {
		t.Fatalf("counters = %+v", c)
	}
}

// checkNoCanary holds the router's every output and its own printed state
// free of canary.
func checkNoCanary(t *testing.T, r *Router, canary string) {
	t.Helper()
	status, err := json.Marshal(r.Status(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	printed := []string{string(status), fmt.Sprintf("%+v %#v", r, r)}
	if table := r.table.Load(); table != nil {
		printed = append(printed, fmt.Sprintf("%+v %#v", table, table))
	}
	r.explore.mu.Lock()
	for id, e := range r.explore.m {
		printed = append(printed, id, fmt.Sprintf("%+v %#v", e, e))
	}
	r.explore.mu.Unlock()
	for _, text := range printed {
		if strings.Contains(text, canary) {
			t.Fatalf("the canary reached the router: %s", text)
		}
	}
}

// The client's headers, the round's other metadata and a header attribute
// are never decoded, so they cannot reach a decision, the status or the
// router's state.
func TestPickNeverMaterializesHeaders(t *testing.T) {
	const canary = "HEADER-CANARY"
	f := worked(t)
	r := routing(t, f.input())
	now := f.Now.Add(42 * time.Second)

	candidates := []cpaCandidate{configKey("0b"), configKey("12"), configKey("0d")}
	for i := range candidates {
		candidates[i].Attributes["header:X-Canary"] = canary
		candidates[i].Metadata = map[string]any{"note": canary}
	}
	// config_index is the one attribute that reaches the status, for an explored
	// key, so the unmapped …12 carries the canary there too.
	candidates[1].Attributes["config_index"] = canary
	req := newRequest(id("0f"), candidates...)
	req.Options.Headers = map[string][]string{
		"Authorization": {"Bearer " + canary},
		"X-Api-Key":     {canary},
		"X-Other":       {canary, canary},
	}
	req.Options.Metadata = map[string]any{
		"selected_auth_id":    id("0f"),
		"selected_auth_index": canary,
		"requested_model":     canary,
		"client":              map[string]any{"ip": canary, "nested": []any{canary}},
		canary:                canary,
	}
	req.Model = canary
	raw := req.bytes(t)
	expect(t, r, raw, now, routed("0b"))
	req.Options.Metadata["selected_auth_id"] = canary + "@example.invalid"
	expect(t, r, req.bytes(t), now, explored("12"))
	checkNoCanary(t, r, canary)
	if exploring := r.Status(now).Exploring; len(exploring) != 1 || exploring[0].ConfigIndex != "" {
		t.Fatalf("exploring = %+v; want one entry with no config_index", exploring)
	}

	// The status shows a config index only in the shape CPA writes one: 1 to 5
	// ASCII digits.
	for value, want := range map[string]string{
		"7": "7", "12345": "12345", "123456": "", "": "", "-1": "", " 7": "", "7 ": "", "0x7": "", "７": "",
	} {
		r := routing(t, f.input())
		c := configKey("12")
		c.Attributes["config_index"] = value
		expect(t, r, newRequest("", c).bytes(t), now, explored("12"))
		if exploring := r.Status(now).Exploring; len(exploring) != 1 || exploring[0].ConfigIndex != want {
			t.Errorf("config_index %q: exploring = %+v; want %q", value, exploring, want)
		}
	}

	// What the decode can hold at all.
	fields := func(typ reflect.Type) []string {
		out := []string{}
		for i := range typ.NumField() {
			out = append(out, typ.Field(i).Name)
		}
		return out
	}
	wire := reflect.TypeOf(pickWire{})
	options, _ := wire.FieldByName("Options")
	metadata, _ := options.Type.FieldByName("Metadata")
	candidate, _ := wire.FieldByName("Candidates")
	attributes, _ := candidate.Type.Elem().FieldByName("Attributes")
	for _, c := range []struct {
		typ  reflect.Type
		want []string
	}{
		{wire, []string{"Options", "Candidates"}},
		{options.Type, []string{"Metadata"}},
		{metadata.Type, []string{"SelectedAuthID"}},
		{candidate.Type.Elem(), []string{"ID", "Provider", "Attributes"}},
		{attributes.Type, []string{"AuthKind", "Source", "ConfigIndex"}},
	} {
		if got := fields(c.typ); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v holds %v; want only %v", c.typ, got, c.want)
		}
	}
	if typ := metadata.Type.Field(0).Type; typ != reflect.TypeOf(configID("")) {
		t.Errorf("SelectedAuthID is %v; want configID", typ)
	}
}

// Picks race publishes, toggles and status reads under -race, and every pick
// is counted exactly once.
func TestRouterIsSafeConcurrently(t *testing.T) {
	f := worked(t)
	tables := []*Table{Build(f.input())}
	g := worked(t)
	b := g.Meter.AuthIDs[id("0b")]
	b.Failures = 2
	g.Meter.AuthIDs[id("0b")] = b
	g.Accounts[0].EstimateLeftText = "$6.20"
	tables = append(tables, Build(g.input()))
	h := worked(t)
	h.Meter.AuthIDsSince = nil
	tables = append(tables, Build(h.input()))

	r := New()
	r.SetEnabled(true)
	r.Publish(tables[0], f.Now)
	now := f.Now.Add(42 * time.Second)
	requests := [][]byte{
		request(t, "", all...),
		request(t, id("0b"), without("0b")...),
		request(t, id("12"), without("12")...),
		request(t, id("0f"), "10", "11"),
		request(t, "", "0a", "0c", "13"),
		newRequest("", configKey("0b"), cpaCandidate{ID: "claude-oauth-placeholder.json", Attributes: map[string]string{"auth_kind": "oauth"}}).bytes(t),
		[]byte("not json"),
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var picks atomic.Uint64
	for n := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				r.Pick(requests[(n+i)%len(requests)], now.Add(time.Duration(i%60)*time.Second))
				picks.Add(1)
			}
		}()
	}
	background := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		}()
	}
	background(func(i int) {
		r.Publish(tables[i%len(tables)], f.Now.Add(time.Duration(i%300)*time.Second))
		if i%7 == 0 {
			r.Publish(nil, now)
		}
	})
	background(func(i int) { r.SetEnabled(i%3 != 0) })
	background(func(int) {
		if _, err := json.Marshal(r.Status(now)); err != nil {
			panic(err)
		}
	})
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()

	c := r.Status(now).Counters
	sum := c.Routed + c.Explored + c.Rechecked
	for _, n := range c.Declined {
		sum += n
	}
	if c.Picks != picks.Load() || sum != c.Picks || c.Declined["panic"] != 0 {
		t.Fatalf("picks %d (made %d), answers %d: %+v", c.Picks, picks.Load(), sum, c)
	}
	if c.Routed == 0 || c.Declined["disabled"] == 0 {
		t.Fatalf("the race exercised too little: %+v", c)
	}
}

// The package reads no clock, touches no file and imports only the packages
// listed below, so a pick cannot block on I/O and every instant is injected.
func TestRouteImportsAndClock(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "encoding/json": true, "sort": true, "strings": true, "sync": true, "sync/atomic": true, "time": true,
		"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if !allowed[path] {
				t.Errorf("%s imports %s, which the router may not use", name, path)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "time" && (selector.Sel.Name == "Now" || selector.Sel.Name == "Since") {
				t.Errorf("%s uses time.%s; the clock must be an argument", fset.Position(selector.Pos()), selector.Sel.Name)
			}
			if pkg.Name == "os" {
				t.Errorf("%s uses os.%s; the router does no I/O", fset.Position(selector.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("no source files found")
	}
}
