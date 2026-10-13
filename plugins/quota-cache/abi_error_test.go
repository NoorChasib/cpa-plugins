package main

import (
	"encoding/json"
	"errors"
	"testing"

	pluginimpl "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

func TestLifecycleErrorsKeepSafeDiagnosticsAndRedactUnknownDetails(t *testing.T) {
	for _, safe := range []string{"cache path changes require native restart", "cache directory must be private (0700)", "another quota-cache writer owns this path"} {
		if sanitizeError(errors.New(safe)) != safe {
			t.Fatal("safe diagnostic was hidden")
		}
	}
	if sanitizeError(errors.New("access_token=private-canary /private/path")) != "quota cache request failed" {
		t.Fatal("private error escaped")
	}
}

// A method this version does not handle, such as one a newer CPA sends, is
// answered unknown_method, not a plugin failure. The method name is wire input
// and is not echoed: the message stays the fixed one.
func TestAnUnknownMethodAnswersUnknownMethodWithAFixedMessage(t *testing.T) {
	_, err := handleGuarded(pluginimpl.New(nil), "plugin.not-yet/access_token=private-canary", nil)
	if err == nil {
		t.Fatal("an unknown method was handled")
	}
	failure := decodeCallError(t, callErrorEnvelope(err))
	if failure.Code != "unknown_method" || failure.Message != "quota cache request failed" {
		t.Fatalf("error = %+v, want unknown_method with the fixed message", failure)
	}
	// The other failures keep their codes.
	if failure := decodeCallError(t, callErrorEnvelope(errPluginPanic)); failure.Code != "plugin_panic" {
		t.Fatalf("panic = %+v", failure)
	}
	if failure := decodeCallError(t, callErrorEnvelope(errors.New("cache lock unavailable"))); failure.Code != "plugin_error" || failure.Message != "cache lock unavailable" {
		t.Fatalf("failure = %+v", failure)
	}
}

func decodeCallError(t *testing.T, raw []byte) protocol.EnvelopeError {
	t.Helper()
	var envelope protocol.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.OK || envelope.Error == nil {
		t.Fatalf("envelope = %s, %v", raw, err)
	}
	return *envelope.Error
}
