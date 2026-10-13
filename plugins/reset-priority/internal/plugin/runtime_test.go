package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/engine"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
)

var testBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const testAccessToken = "tok-secret-access-token-aaaa"

// fakeHostCaller speaks the raw host-callback wire protocol so the Runtime
// is exercised end-to-end through the real Bridge, including base64 []byte
// handling and envelope decoding.
type fakeHostCaller struct {
	mu        sync.Mutex
	entries   []hostapi.AuthEntry
	docs      map[string]json.RawMessage // by auth index
	httpBody  map[string]string          // bearer token -> body
	httpCalls []hostapi.HTTPRequest
	saves     []hostapi.AuthSaveRequest
	logs      []hostapi.LogRequest
	listErr   error
}

func newFakeHostCaller() *fakeHostCaller {
	return &fakeHostCaller{
		docs:     make(map[string]json.RawMessage),
		httpBody: make(map[string]string),
	}
}

func ok(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(hostapi.Envelope{OK: true, Result: raw})
}

func (c *fakeHostCaller) Call(method string, request []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case hostapi.MethodHostAuthList:
		if c.listErr != nil {
			return nil, c.listErr
		}
		return ok(hostapi.AuthListResponse{Files: append([]hostapi.AuthEntry(nil), c.entries...)})
	case hostapi.MethodHostAuthGet:
		var req hostapi.AuthGetRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		doc, found := c.docs[req.AuthIndex]
		if !found {
			return json.Marshal(hostapi.Envelope{OK: false, Error: &hostapi.EnvelopeError{Code: "not_found", Message: "auth not found"}})
		}
		name := ""
		for _, e := range c.entries {
			if e.AuthIndex == req.AuthIndex {
				name = e.Name
			}
		}
		return ok(hostapi.AuthGetResponse{AuthIndex: req.AuthIndex, Name: name, JSON: doc})
	case hostapi.MethodHostAuthGetRuntime:
		var req hostapi.AuthGetRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		for _, e := range c.entries {
			if e.AuthIndex == req.AuthIndex {
				return ok(hostapi.AuthGetRuntimeResponse{Auth: e})
			}
		}
		return json.Marshal(hostapi.Envelope{OK: false, Error: &hostapi.EnvelopeError{Code: "not_found", Message: "auth not found"}})
	case hostapi.MethodHostAuthSave:
		var req hostapi.AuthSaveRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		c.saves = append(c.saves, req)
		for i := range c.entries {
			if c.entries[i].Name == req.Name {
				c.docs[c.entries[i].AuthIndex] = append(json.RawMessage(nil), req.JSON...)
			}
		}
		return ok(hostapi.AuthSaveResponse{Name: req.Name, Path: "/auths/" + req.Name})
	case hostapi.MethodHostHTTPDo:
		var req hostapi.HTTPRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		c.httpCalls = append(c.httpCalls, req)
		token := ""
		for key, values := range req.Headers {
			if strings.EqualFold(key, "Authorization") && len(values) > 0 {
				token = strings.TrimPrefix(values[0], "Bearer ")
			}
		}
		body, found := c.httpBody[token]
		if !found {
			return ok(hostapi.HTTPResponse{StatusCode: 404, Body: []byte(`{}`)})
		}
		return ok(hostapi.HTTPResponse{
			StatusCode: 200,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(body),
		})
	case hostapi.MethodHostLog:
		var req hostapi.LogRequest
		_ = json.Unmarshal(request, &req)
		c.logs = append(c.logs, req)
		return ok(struct{}{})
	default:
		return nil, fmt.Errorf("unexpected host callback %s", method)
	}
}

// shutdownGatedCaller simulates a synchronous native callback that cannot be
// cancelled after entry.
type shutdownGatedCaller struct {
	started  chan struct{}
	release  chan struct{}
	response []byte
}

func (c *shutdownGatedCaller) Call(method string, request []byte) ([]byte, error) {
	if method != hostapi.MethodHostAuthList {
		return nil, fmt.Errorf("unexpected host callback %s", method)
	}
	close(c.started)
	<-c.release
	return c.response, nil
}

func (c *fakeHostCaller) addClaudeAccount(name string, resetAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := "idx-" + name
	token := testAccessToken + "-" + name
	c.entries = append(c.entries, hostapi.AuthEntry{
		AuthIndex: idx,
		Name:      name + ".json",
		Path:      "/auths/" + name + ".json",
		Source:    "file",
		Provider:  "claude",
		Status:    "active",
		Email:     name + "@example.com",
	})
	doc, _ := json.Marshal(map[string]any{
		"type":          "claude",
		"access_token":  token,
		"refresh_token": "refresh-token-super-secret",
		"email":         name + "@example.com",
	})
	c.docs[idx] = doc
	c.httpBody[token] = fmt.Sprintf(
		`{"five_hour":{"utilization":50,"resets_at":"%s"},"seven_day":{"utilization":80,"resets_at":"%s"}}`,
		testBase.Add(time.Hour).Format(time.RFC3339Nano),
		resetAt.Format(time.RFC3339Nano),
	)
}

func (c *fakeHostCaller) httpCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.httpCalls)
}

func (c *fakeHostCaller) httpCallsSince(offset int) []hostapi.HTTPRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hostapi.HTTPRequest(nil), c.httpCalls[offset:]...)
}

func (c *fakeHostCaller) setListError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listErr = err
}

func (c *fakeHostCaller) saveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.saves)
}

// newTestRuntime returns a Runtime with an inline async runner and fake
// clock so register-triggered reconciliation happens synchronously.
func newTestRuntime(t *testing.T, caller hostapi.Caller) *Runtime {
	t.Helper()
	return NewRuntime(caller,
		WithClock(clock.NewFake(testBase)),
		WithRunAsync(func(f func()) { f() }),
	)
}

func lifecycleRequest(t *testing.T, yaml string, schema uint32) []byte {
	t.Helper()
	raw, err := json.Marshal(hostapi.LifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: schema})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) hostapi.Envelope {
	t.Helper()
	var env hostapi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	return env
}

func registerRuntime(t *testing.T, rt *Runtime, yaml string) hostapi.Envelope {
	t.Helper()
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginRegister, lifecycleRequest(t, yaml, hostapi.SchemaVersion)))
	if !env.OK {
		t.Fatalf("register failed: %s", env.Error.Message)
	}
	return env
}

func TestRegisterParsesBase64ConfigAndReconciles(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)

	// Build the raw request by hand to prove config_yaml travels as base64
	// (the wire form of a Go []byte).
	yaml := "enabled: true\npriority: 1\n"
	rawReq := []byte(fmt.Sprintf(`{"config_yaml":%q,"schema_version":4}`,
		base64.StdEncoding.EncodeToString([]byte(yaml))))
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginRegister, rawReq))
	if !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}

	// Registration result shape: schema, capitalized Metadata keys,
	// snake_case capability key.
	raw := string(env.Result)
	for _, want := range []string{
		`"schema_version":4`,
		`"Name":"Reset Priority"`,
		`"Version":"` + PluginVersion + `"`,
		`"GitHubRepository":"https://github.com/NoorChasib/cpa-plugins"`,
		`"management_api":true`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("registration result missing %s: %s", want, raw)
		}
	}

	// The startup reconcile ran synchronously: the single account got the
	// floor priority written through host.auth.save.
	if caller.saveCount() != 1 {
		t.Fatalf("saves = %d, want 1", caller.saveCount())
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(caller.saves[0].JSON, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["priority"]) != "100" {
		t.Errorf("written priority = %s, want 100", doc["priority"])
	}
	if string(doc["refresh_token"]) != `"refresh-token-super-secret"` {
		t.Errorf("unrelated field not preserved: %s", doc["refresh_token"])
	}
}

func TestRegisterInvalidConfigRejected(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginRegister,
		lifecycleRequest(t, "enabled: true\npriority-step: 0\n", 4)))
	if env.OK {
		t.Fatalf("invalid config accepted")
	}
	if env.Error == nil || env.Error.Code != "invalid_config" {
		t.Errorf("error = %+v, want invalid_config", env.Error)
	}
}

// The plugin answers min(offered, 6). Schema 6 is the first where CPA stops
// HTML-escaping management JSON, so the status's router labels and issue
// sentences reach a reader as written; nothing else here depends on it.
func TestSchemaNegotiation(t *testing.T) {
	for hostSchema, want := range map[uint32]uint32{4: 4, 3: 3, 0: 1, 5: 5, 6: 6, 9: 6} {
		rt := newTestRuntime(t, newFakeHostCaller())
		env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginRegister,
			lifecycleRequest(t, "enabled: false\n", hostSchema)))
		if !env.OK {
			t.Fatalf("register failed: %+v", env.Error)
		}
		var reg hostapi.Registration
		if err := json.Unmarshal(env.Result, &reg); err != nil {
			t.Fatal(err)
		}
		if reg.SchemaVersion != want {
			t.Errorf("host schema %d: negotiated %d, want %d", hostSchema, reg.SchemaVersion, want)
		}
	}
}

// CPA's plugin list HTML-escapes the metadata, every config field and every
// menu entry at any schema, and the console draws them as plain text, so an
// apostrophe in a field description shows as &#39; in the Configure dialog.
// Nothing the plugin registers may hold a character html.EscapeString changes.
func TestRegisteredTextSurvivesPluginListEscaping(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginRegister,
		lifecycleRequest(t, "enabled: false\n", hostapi.SchemaVersion)))
	if !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	var reg hostapi.Registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	env = decodeEnvelope(t, rt.Dispatch(hostapi.MethodManagementRegister, nil))
	if !env.OK {
		t.Fatalf("management.register failed: %+v", env.Error)
	}
	var mgmt hostapi.ManagementRegistration
	if err := json.Unmarshal(env.Result, &mgmt); err != nil {
		t.Fatal(err)
	}

	meta := reg.Metadata
	text := map[string]string{
		"metadata Name":             meta.Name,
		"metadata Version":          meta.Version,
		"metadata Author":           meta.Author,
		"metadata GitHubRepository": meta.GitHubRepository,
		"metadata Logo":             meta.Logo,
	}
	if len(meta.ConfigFields) == 0 {
		t.Fatal("registration lists no config fields")
	}
	for _, field := range meta.ConfigFields {
		text["config field "+field.Name+" Name"] = field.Name
		text["config field "+field.Name+" Type"] = field.Type
		text["config field "+field.Name+" Description"] = field.Description
		for i, value := range field.EnumValues {
			text[fmt.Sprintf("config field %s EnumValues[%d]", field.Name, i)] = value
		}
	}
	for _, managed := range mgmt.Routes {
		text["route "+managed.Path+" Path"] = managed.Path
		text["route "+managed.Path+" Menu"] = managed.Menu
		text["route "+managed.Path+" Description"] = managed.Description
	}
	for _, resource := range mgmt.Resources {
		text["resource "+resource.Path+" Path"] = resource.Path
		text["resource "+resource.Path+" Menu"] = resource.Menu
		text["resource "+resource.Path+" Description"] = resource.Description
	}
	for where, value := range text {
		if escaped := html.EscapeString(value); escaped != value {
			t.Errorf("%s reaches the console as %q", where, escaped)
		}
	}
}

func TestReconfigureAppliesNewFloorAndStep(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginReconfigure,
		lifecycleRequest(t, "enabled: true\npriority-floor: 500\npriority-step: 250\n", 4)))
	if !env.OK {
		t.Fatalf("reconfigure failed: %+v", env.Error)
	}
	snap := statusSnapshot(t, rt)
	if got := snap.Providers[0].Accounts[0].DesiredPriority; got != 500 {
		t.Errorf("desired after reconfigure = %d, want 500", got)
	}
}

func TestReconfigureDisableStopsEngine(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginReconfigure,
		lifecycleRequest(t, "enabled: false\n", 4)))
	if !env.OK {
		t.Fatalf("reconfigure failed: %+v", env.Error)
	}
	if snap := statusSnapshot(t, rt); !snap.Stopped {
		t.Errorf("engine not stopped after disable")
	}
}

func TestQuiesceStopsEngine(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginQuiesce, nil))
	if !env.OK {
		t.Fatalf("quiesce failed: %+v", env.Error)
	}
	if snap := statusSnapshot(t, rt); !snap.Stopped {
		t.Errorf("engine not stopped after quiesce")
	}
}

func TestQuiesceAllowsLaterReconfigure(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	registerRuntime(t, rt, "enabled: true\n")

	if env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginQuiesce, nil)); !env.OK {
		t.Fatalf("quiesce failed: %+v", env.Error)
	}
	if snap := statusSnapshot(t, rt); !snap.Stopped {
		t.Fatalf("engine not stopped after quiesce")
	}

	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginReconfigure,
		lifecycleRequest(t, "enabled: true\n", hostapi.SchemaVersion)))
	if !env.OK {
		t.Fatalf("reconfigure after quiesce failed: %+v", env.Error)
	}
	if snap := statusSnapshot(t, rt); snap.Stopped {
		t.Errorf("engine remained stopped after reconfigure")
	}
}

func TestNativeShutdownDrainsEnteredCallbackAndRejectsDispatch(t *testing.T) {
	response, err := ok(hostapi.AuthListResponse{})
	if err != nil {
		t.Fatal(err)
	}
	caller := &shutdownGatedCaller{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		response: response,
	}
	rt := NewRuntime(caller,
		WithClock(clock.NewFake(testBase)),
		WithRunAsync(func(f func()) { go f() }),
	)
	registerRuntime(t, rt, "enabled: true\n")
	<-caller.started
	released := false
	defer func() {
		if !released {
			close(caller.release)
		}
	}()

	shutdownDone := make(chan struct{})
	go func() {
		rt.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatalf("native shutdown returned while callback was still in flight")
	case <-time.After(20 * time.Millisecond):
	}

	close(caller.release)
	released = true
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("native shutdown did not return after callback drained")
	}

	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodManagementRegister, nil))
	if env.OK || env.Error == nil || env.Error.Code != "plugin_shutdown" {
		t.Errorf("post-shutdown dispatch envelope = %+v", env)
	}
}

func TestNativeShutdownDrainsScheduledPluginWork(t *testing.T) {
	scheduled := make(chan func(), 1)
	rt := NewRuntime(newFakeHostCaller(),
		WithClock(clock.NewFake(testBase)),
		WithRunAsync(func(f func()) { scheduled <- f }),
	)
	registerRuntime(t, rt, "enabled: true\n")
	work := <-scheduled

	shutdownDone := make(chan struct{})
	go func() {
		rt.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatalf("native shutdown returned while scheduled plugin work was pending")
	case <-time.After(20 * time.Millisecond):
	}

	work()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("native shutdown did not return after scheduled work drained")
	}
}

// panickingListCaller panics in the roster list once armed, with a value that
// stands for one carrying credential data.
type panickingListCaller struct {
	*fakeHostCaller
	armed atomic.Bool
}

func (c *panickingListCaller) Call(method string, request []byte) ([]byte, error) {
	if method == hostapi.MethodHostAuthList && c.armed.Load() {
		panic("roster body carrying " + testAccessToken)
	}
	return c.fakeHostCaller.Call(method, request)
}

// TestDispatchPanicDoesNotEchoThePanicValue: a management refresh runs the
// reconciliation on the Dispatch goroutine, so a panic in its roster list
// reaches Dispatch's recover. That value can carry credential data, and the
// envelope goes back to CPA, so it must not repeat it.
func TestDispatchPanicDoesNotEchoThePanicValue(t *testing.T) {
	caller := &panickingListCaller{fakeHostCaller: newFakeHostCaller()}
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	caller.armed.Store(true)

	reqRaw, err := json.Marshal(hostapi.ManagementRequest{
		Method:  "POST",
		Path:    "/v0/management/plugins/reset-priority/refresh",
		Headers: map[string][]string{refreshRequestHeader: {refreshRequestHeaderValue}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodManagementHandle, reqRaw))
	if env.OK || env.Error == nil || env.Error.Code != "plugin_panic" {
		t.Fatalf("refresh that panicked = %+v, want a plugin_panic error", env)
	}
	if env.Error.Message != "reset-priority: internal error" {
		t.Errorf("plugin_panic message = %q, want %q", env.Error.Message, "reset-priority: internal error")
	}
}

func TestUnknownMethodReturnsErrorEnvelope(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	env := decodeEnvelope(t, rt.Dispatch("model.route", nil))
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Errorf("unknown method envelope = %+v", env)
	}
}

// statusSnapshot fetches the parsed management status via the route.
func statusSnapshot(t *testing.T, rt *Runtime) engine.Snapshot {
	t.Helper()
	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status route returned %d", resp.StatusCode)
	}
	var snap engine.Snapshot
	if err := json.Unmarshal(resp.Body, &snap); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	return snap
}

func managementCall(t *testing.T, rt *Runtime, method, path string, query map[string][]string) hostapi.ManagementResponse {
	t.Helper()
	return managementRequest(t, rt, hostapi.ManagementRequest{Method: method, Path: path, Query: query})
}

func refreshCall(t *testing.T, rt *Runtime, headers map[string][]string, callbackID string) hostapi.ManagementResponse {
	t.Helper()
	if headers == nil {
		headers = make(map[string][]string)
	}
	if _, present := headers[refreshRequestHeader]; !present {
		headers[refreshRequestHeader] = []string{refreshRequestHeaderValue}
	}
	return managementRequest(t, rt, hostapi.ManagementRequest{
		Method:         "POST",
		Path:           "/v0/management/plugins/reset-priority/refresh",
		Headers:        headers,
		HostCallbackID: callbackID,
	})
}

func managementRequest(t *testing.T, rt *Runtime, req hostapi.ManagementRequest) hostapi.ManagementResponse {
	t.Helper()
	reqRaw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodManagementHandle, reqRaw))
	if !env.OK {
		t.Fatalf("management.handle failed: %+v", env.Error)
	}
	var resp hostapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	return resp
}
