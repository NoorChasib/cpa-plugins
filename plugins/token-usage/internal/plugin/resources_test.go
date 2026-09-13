package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/protocol"
)

func TestPublicSidebarIsFixedAcrossUnregisteredUnavailableActiveAndStoppedStates(t *testing.T) {
	const public = "/v0/resource/plugins/token-usage/status"
	p := New()
	p.now = func() time.Time { return testNow }
	t.Cleanup(p.Shutdown)
	baseline := manage(t, p, public, nil)
	if baseline.StatusCode != 200 || !strings.HasPrefix(baseline.Headers.Get("Content-Type"), "text/html") || !bytes.Contains(baseline.Body, []byte("Token Usage")) {
		t.Fatalf("unconfigured public shell unavailable: %d %.120s", baseline.StatusCode, baseline.Body)
	}
	check := func(state string) {
		t.Helper()
		for _, extra := range []string{
			`"Headers":{"Authorization":["private-key-canary"]}`,
			`"Query":{"provider":["private-provider-canary"],"api_key":["private-key-canary"]}`,
			`"Headers":42,"Query":42,"Body":"private-body-canary"`,
		} {
			raw := []byte(fmt.Sprintf(`{"Method":"GET","Path":%q,%s}`, public, extra))
			value, err := p.Handle(protocol.MethodManagementHandle, raw)
			if err != nil {
				t.Fatal(err)
			}
			response := value.(protocol.ManagementResponse)
			if response.StatusCode != baseline.StatusCode || !bytes.Equal(response.Body, baseline.Body) || !reflect.DeepEqual(response.Headers, baseline.Headers) {
				t.Fatalf("public shell varied with %s or caller data: %d %.120s", state, response.StatusCode, response.Body)
			}
			for _, canary := range []string{"private-provider-canary", "private-model-canary", "private-key-canary", "private-body-canary", "private-path-canary", "secret-canary"} {
				if bytes.Contains(response.Body, []byte(canary)) {
					t.Fatalf("public shell exposed %s in %s", canary, state)
				}
			}
		}
	}
	check("unregistered")
	bad := filepath.Join(t.TempDir(), "private-path-canary.sqlite")
	if err := os.WriteFile(bad, []byte("corrupt private history"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, registrationRequest(bad)); err != nil {
		t.Fatal(err)
	}
	check("initial storage failure")
	request := registrationRequest(filepath.Join(t.TempDir(), "private", "private-path-canary.sqlite"))
	if _, err := p.Handle(protocol.MethodPluginReconfigure, request); err != nil {
		t.Fatal(err)
	}
	wire := strings.ReplaceAll(strings.ReplaceAll(validUsage, `"Provider":"test"`, `"Provider":"private-provider-canary"`), `"Model":"model"`, `"Model":"private-model-canary"`)
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(wire)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "1")
	if r := manage(t, p, "/v0/management/plugins/token-usage/models", query()); r.StatusCode != 200 || !bytes.Contains(r.Body, []byte("private-provider-canary")) {
		t.Fatalf("private canary setup failed: %d %s", r.StatusCode, r.Body)
	}
	check("committed private statistics")
	p.Handle(protocol.MethodPluginQuiesce, nil)
	check("quiesced collector")
	if _, err := p.Handle(protocol.MethodPluginReconfigure, request); err != nil {
		t.Fatal(err)
	}
	check("reopened collector")
	p.Shutdown()
	check("terminal shutdown")
}

func TestDispatchAllowsOnlyExactResourceGETAndExistingPrivatePaths(t *testing.T) {
	p := newRegistered(t)
	public := "/v0/resource/plugins/token-usage/status"
	for _, method := range []string{"", "get", http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		for _, path := range []string{public, "/v0/management/plugins/token-usage/status", "/v0/management/plugins/token-usage/summary", "/v0/management/plugins/token-usage/models"} {
			raw, _ := json.Marshal(protocol.ManagementRequest{Method: method, Path: path})
			value, err := p.Handle(protocol.MethodManagementHandle, raw)
			if err != nil || value.(protocol.ManagementResponse).StatusCode != 404 {
				t.Fatalf("unexpected dispatch for %q %q: %v %v", method, path, value, err)
			}
		}
	}
	for _, path := range []string{
		public + "/", public + "?private=true", public + "/summary", "/v0/resource/plugins/token-usage/summary", "/v0/resource/plugins/token-usage/models",
		"/v0/resource/plugins/other/status", "/v0/resource/plugins/token-usage/%73tatus", "/v0/resource/plugins/token-usage/../token-usage/status",
		"/v0/management/plugins/token-usage/status/", "/v0/management/plugins/token-usage/resource/status", "/v0/management/plugins/token-usage/status.html", "/v0/resource/v0/management/plugins/token-usage/summary",
	} {
		if r := manage(t, p, path, nil); r.StatusCode != 404 || string(r.Body) != `{"error":"not_found"}` {
			t.Fatalf("unknown path exposed an API or shell: %q %d %.120s", path, r.StatusCode, r.Body)
		}
	}
}

func TestRegistrationSeparatesOnePublicSidebarFromThreePrivateAPIs(t *testing.T) {
	p := New()
	t.Cleanup(p.Shutdown)
	value, err := p.Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var registration map[string][]map[string]any
	if err := json.Unmarshal(wire, &registration); err != nil {
		t.Fatal(err)
	}
	// These spellings are CPA 7fac6b15's native RPC wrapper and SDK field names:
	// lowercase wrappers, capitalized members, and no Method on ResourceRoute.
	resources := registration["resources"]
	if len(registration) != 2 || len(resources) != 1 || resources[0]["Path"] != "/status" || resources[0]["Menu"] != "Token Usage" || resources[0]["Description"] == "" || len(resources[0]) != 3 {
		t.Fatalf("expected exactly one public shell resource: %s", wire)
	}
	routes := registration["routes"]
	if len(routes) != 3 {
		t.Fatalf("expected exactly three private routes: %s", wire)
	}
	for i, endpoint := range []string{"status", "summary", "models"} {
		if routes[i]["Method"] != http.MethodGet || routes[i]["Path"] != "/plugins/token-usage/"+endpoint || routes[i]["Menu"] != "" {
			t.Fatalf("management API became a public Menu or changed path/method: %s", wire)
		}
	}
}
