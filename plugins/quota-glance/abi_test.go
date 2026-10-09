package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// An older CPA without host.routing.reset_cooldown is told apart from a call
// that failed, and neither error carries the host's message, which elsewhere
// can hold upstream text.
func TestCallbackFailureNamesAnUnsupportedCallbackAndNothingElse(t *testing.T) {
	missing := callbackFailure(protocol.MethodHostRoutingResetCooldown, &protocol.EnvelopeError{
		Code: "host_call_failed", Message: "unsupported host callback host.routing.reset_cooldown",
	})
	if !errors.Is(missing, protocol.ErrUnsupportedCallback) {
		t.Fatalf("missing callback = %v, want ErrUnsupportedCallback", missing)
	}
	failed := callbackFailure(protocol.MethodHostRoutingResetCooldown, &protocol.EnvelopeError{
		Code: "host_call_failed", Message: "auth not found for auth_index secret-ish-detail",
	})
	if errors.Is(failed, protocol.ErrUnsupportedCallback) || strings.Contains(failed.Error(), "secret-ish-detail") {
		t.Fatalf("failed call = %v, want a plain failure without the host's message", failed)
	}
	if bare := callbackFailure(protocol.MethodHostRoutingResetCooldown, nil); bare == nil || errors.Is(bare, protocol.ErrUnsupportedCallback) {
		t.Fatalf("bare failure = %v", bare)
	}
}

// The callback's name and wire shape, as CPA declares them in
// sdk/pluginabi/types.go (MethodHostRoutingResetCooldown) and
// sdk/pluginapi/types.go (HostRoutingResetCooldownRequest and Response). The
// bridge itself only runs inside CPA, so this is where a drift in the mirror
// would show.
func TestTheCooldownCallbackMatchesCPAsContract(t *testing.T) {
	if protocol.MethodHostRoutingResetCooldown != "host.routing.reset_cooldown" {
		t.Fatalf("method = %q", protocol.MethodHostRoutingResetCooldown)
	}
	raw, err := json.Marshal(protocol.HostRoutingResetCooldownRequest{AuthIndex: "a1b2c3"})
	if err != nil || string(raw) != `{"auth_index":"a1b2c3"}` {
		t.Fatalf("request = %s, %v", raw, err)
	}
	var response protocol.HostRoutingResetCooldownResponse
	if err := json.Unmarshal([]byte(`{"auth_index":"a1b2c3","models":["claude-haiku-5-5"]}`), &response); err != nil ||
		response.AuthIndex != "a1b2c3" || len(response.Models) != 1 {
		t.Fatalf("response = %+v, %v", response, err)
	}
}
