package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/plugin"
)

// These tests decide scheduler.pick envelopes in pure Go. They never touch
// nativeShuttingDown, which abi_lifecycle_test.go turns on for good: the
// native gate's verdict is passed to schedulerPickEnvelope instead.

// routingAt is the instant Quota Cache's api-credits-routing fixtures
// describe.
var routingAt = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

const routedEnvelope = `{"ok":true,"result":{"handled":true,"auth_id":"claude:apikey:00000000000b"}}`

// quietHost answers the callbacks a configure makes: an empty credential list
// for the OAuth engine and ok for every log line.
type quietHost struct{}

func (quietHost) Call(method string, _ []byte) ([]byte, error) {
	if method == hostapi.MethodHostAuthList {
		return []byte(`{"ok":true,"result":{"files":[]}}`), nil
	}
	return []byte(`{"ok":true,"result":{}}`), nil
}

// routingRuntime is a Runtime whose router has built its first table from
// Quota Cache's routing fixtures, with route-api-credits as given.
func routingRuntime(t *testing.T, routing bool, opts ...plugin.Option) *plugin.Runtime {
	t.Helper()
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "snapshot.json")
	fixtures := filepath.Join("..", "quota-cache", "internal", "credits", "testdata")
	for from, to := range map[string]string{
		filepath.Join(fixtures, "golden", "api-credits-routing.credits.json"):  qc.CreditsPath(snapshot),
		filepath.Join(fixtures, "snapshots", "api-credits-routing.meter.json"): qc.MeterPath(snapshot),
	} {
		raw, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clk := clock.NewFake(routingAt)
	rt := plugin.NewRuntime(quietHost{}, append([]plugin.Option{
		plugin.WithClock(clk), plugin.WithRunAsync(func(f func()) { f() }),
	}, opts...)...)
	t.Cleanup(rt.Shutdown)
	yaml := fmt.Sprintf("enabled: true\ndry-run: true\nuse-quota-cache: false\nquota-cache-path: %q\nroute-api-credits: %t\n", snapshot, routing)
	request, err := json.Marshal(hostapi.LifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: hostapi.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	var env hostapi.Envelope
	if err := json.Unmarshal(rt.Dispatch(hostapi.MethodPluginRegister, request), &env); err != nil || !env.OK {
		t.Fatalf("register: %v %+v", err, env.Error)
	}
	clk.Advance(0) // the loop's first build
	return rt
}

// pickFixture is a second pick in a round as CPA v8.0.23 sends it: the
// previous attempt, …0a (alpha, which is out), failed, and the round offers
// …0b (bravo, which refills soonest with credit left) and …0c (charlie, out).
func pickFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "scheduler-pick-v8.0.23.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// CPA fails the client's request on any scheduler error, a non-zero return
// code or an envelope it cannot decode, so every doubt is exactly the
// handled:false envelope, and a routed pick exactly the handled one.
func TestSchedulerPickNeverReturnsAnError(t *testing.T) {
	request := pickFixture(t)
	routed := routingRuntime(t, true)
	if got := schedulerPickEnvelope(routed, false, false, request); string(got) != routedEnvelope {
		t.Fatalf("routed pick = %s; want %s", got, routedEnvelope)
	}
	panicking := routingRuntime(t, true, plugin.WithBeforeSchedulerPick(func() { panic("PANIC-VALUE-CANARY") }))
	for _, tc := range []struct {
		name                  string
		rt                    *plugin.Runtime
		shuttingDown, invalid bool
		raw                   []byte
	}{
		{"no runtime", nil, false, false, request},
		{"shutting down", routed, true, false, request},
		{"invalid request", routed, false, true, nil},
		{"invalid and shutting down", routed, true, true, nil},
		{"panicking pick", panicking, false, false, request},
		{"bad JSON", routed, false, false, []byte(`{"Candidates":`)},
		{"empty request", routed, false, false, nil},
		{"routing off", routingRuntime(t, false), false, false, request},
		{"never configured", plugin.NewRuntime(quietHost{}, plugin.WithClock(clock.NewFake(routingAt))), false, false, request},
	} {
		if got := schedulerPickEnvelope(tc.rt, tc.shuttingDown, tc.invalid, tc.raw); !bytes.Equal(got, unhandledEnvelope) {
			t.Errorf("%s: %s; want %s", tc.name, got, unhandledEnvelope)
		}
	}
	if string(unhandledEnvelope) != `{"ok":true,"result":{"handled":false}}` {
		t.Fatalf("unhandledEnvelope was modified: %s", unhandledEnvelope)
	}
}

// pickResponse is a copy of CPA's SchedulerPickResponse decoding at v8.0.23
// (sdk/pluginapi/types.go:546-590): each field takes its Go name first, else
// its snake_case name.
type pickResponse struct {
	AuthID, DelegateBuiltin, RejectReason, RejectCode string
	Handled, Reject                                   bool
}

func (r *pickResponse) UnmarshalJSON(data []byte) error {
	var raw struct {
		AuthID          *string `json:"AuthID"`
		AltAuthID       *string `json:"auth_id"`
		DelegateBuiltin *string `json:"DelegateBuiltin"`
		AltDelegate     *string `json:"delegate_builtin"`
		Handled         *bool   `json:"Handled"`
		AltHandled      *bool   `json:"handled"`
		Reject          *bool   `json:"Reject"`
		AltReject       *bool   `json:"reject"`
		RejectReason    *string `json:"RejectReason"`
		AltRejectReason *string `json:"reject_reason"`
		RejectCode      *string `json:"RejectCode"`
		AltRejectCode   *string `json:"reject_code"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	pickString := func(dst *string, primary, alternate *string) {
		if primary != nil {
			*dst = *primary
		} else if alternate != nil {
			*dst = *alternate
		}
	}
	pickBool := func(dst *bool, primary, alternate *bool) {
		if primary != nil {
			*dst = *primary
		} else if alternate != nil {
			*dst = *alternate
		}
	}
	pickString(&r.AuthID, raw.AuthID, raw.AltAuthID)
	pickString(&r.DelegateBuiltin, raw.DelegateBuiltin, raw.AltDelegate)
	pickBool(&r.Handled, raw.Handled, raw.AltHandled)
	pickBool(&r.Reject, raw.Reject, raw.AltReject)
	pickString(&r.RejectReason, raw.RejectReason, raw.AltRejectReason)
	pickString(&r.RejectCode, raw.RejectCode, raw.AltRejectCode)
	return nil
}

// The method, the request as CPA marshals it and both answers as CPA decodes
// them, all at v8.0.23. testdata/scheduler-pick-v8.0.23.json holds the bytes of
// a second pick in a round, generated once by marshalling
// pluginapi.SchedulerPickRequest exactly as CPA's plugin client does, with
// placeholder IDs and canary headers. Its Plugin block names the plugin that
// held the scheduler when the bytes were captured; the router never decodes it.
func TestSchedulerPickMatchesCPAsContract(t *testing.T) {
	if hostapi.MethodSchedulerPick != "scheduler.pick" {
		t.Fatalf("method = %q", hostapi.MethodSchedulerPick)
	}
	raw := pickFixture(t)
	var sent struct {
		Options struct {
			Metadata map[string]any
		}
		Candidates []struct {
			ID         string
			Attributes map[string]string
		}
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Options.Metadata["selected_auth_id"] != "claude:apikey:00000000000a" || len(sent.Candidates) != 2 ||
		sent.Candidates[0].ID != "claude:apikey:00000000000b" || sent.Candidates[0].Attributes["auth_kind"] != "apikey" {
		t.Fatalf("the fixture is not the second pick it should be: %s", raw)
	}

	routed := schedulerPickEnvelope(routingRuntime(t, true), false, false, raw)
	for _, tc := range []struct {
		envelope []byte
		want     pickResponse
	}{
		{routed, pickResponse{Handled: true, AuthID: "claude:apikey:00000000000b"}},
		{unhandledEnvelope, pickResponse{}},
	} {
		var env hostapi.Envelope
		if err := json.Unmarshal(tc.envelope, &env); err != nil || !env.OK || env.Error != nil {
			t.Fatalf("%s is not an ok envelope: %v", tc.envelope, err)
		}
		var got pickResponse
		if err := json.Unmarshal(env.Result, &got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s decodes as %+v; want %+v", tc.envelope, got, tc.want)
		}
	}

	for _, tc := range []struct {
		capabilities hostapi.Capabilities
		want         string
	}{
		{hostapi.Capabilities{ManagementAPI: true}, `{"management_api":true}`},
		{hostapi.Capabilities{ManagementAPI: true, Scheduler: true}, `{"management_api":true,"scheduler":true}`},
	} {
		got, err := json.Marshal(tc.capabilities)
		if err != nil || string(got) != tc.want {
			t.Errorf("capabilities %+v marshal to %s; want %s", tc.capabilities, got, tc.want)
		}
	}
}

// Every other method keeps its envelopes: errors stay errors, which the
// native entry point returns with rc 1, and lifecycle answers are unchanged.
func TestDispatchKeepsOtherMethodsUnchanged(t *testing.T) {
	rt := routingRuntime(t, true)
	decode := func(raw []byte) hostapi.Envelope {
		t.Helper()
		var env hostapi.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	if env := decode(rt.Dispatch("model.route", nil)); env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Errorf("an unknown method = %+v", env)
	}
	if env := decode(rt.Dispatch(hostapi.MethodManagementRegister, nil)); !env.OK {
		t.Errorf("management.register = %+v", env)
	}
	if env := decode(rt.Dispatch(hostapi.MethodPluginRegister, []byte("{"))); env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Errorf("a malformed register = %+v", env)
	}
	if env := decode(rt.Dispatch("Scheduler.Pick", pickFixture(t))); env.OK {
		t.Errorf("a method that only resembles scheduler.pick was answered as one: %+v", env)
	}
}
