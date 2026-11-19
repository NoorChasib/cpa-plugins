package health

import (
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/protocol"
)

func runtimeSnapshot(now time.Time, status, message string, unavailable bool, retry time.Time, httpStatus int) RuntimeSnapshot {
	entry := protocol.HostAuthFileEntry{
		AuthIndex:      "one",
		Provider:       "claude",
		Type:           "claude",
		AccountType:    "oauth",
		Status:         status,
		StatusMessage:  message,
		Unavailable:    unavailable,
		NextRetryAfter: retry,
	}
	return FromHostEntry(entry, FailureEvidence{HTTPStatus: httpStatus, ObservedAt: now}, now)
}

func TestClassifierUsesProductionAvailableEvidence(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		snapshot    RuntimeSnapshot
		want        State
		reason      ReasonCode
		confirm     ConfirmationClass
		confirmAs   State
		forbidQuota bool
	}{
		{
			name:     "healthy enabled auth",
			snapshot: runtimeSnapshot(now, "active", "", false, time.Time{}, 0),
			want:     Healthy,
		},
		{
			name:     "429 arbitrary provider body",
			snapshot: runtimeSnapshot(now, "error", `{"opaque":"provider prose without known words"}`, true, now.Add(time.Hour), 429),
			want:     QuotaLimited,
			reason:   ReasonHTTP429Quota,
		},
		{
			name:        "401 body containing quota words is not quota",
			snapshot:    runtimeSnapshot(now, "error", "quota exhausted unauthorized provider prose", true, now.Add(time.Minute), 401),
			want:        Suspect,
			reason:      ReasonUnauthorizedRequest,
			confirm:     ConfirmationUnauthorized,
			confirmAs:   ReauthRequired,
			forbidQuota: true,
		},
		{
			name:     "active runtime overrides stale 401",
			snapshot: runtimeSnapshot(now, "active", "unauthorized", false, time.Time{}, 401),
			want:     Healthy,
		},
		{
			name:     "canonical refresh unauthorized is definitive",
			snapshot: runtimeSnapshot(now, "error", "unauthorized", true, time.Time{}, 0),
			want:     ReauthRequired,
			reason:   ReasonUnauthorizedRefreshFailed,
		},
		{
			name:      "request unauthorized inside retry window needs confirmation",
			snapshot:  runtimeSnapshot(now, "error", "unauthorized", true, now.Add(time.Minute), 401),
			want:      Suspect,
			reason:    ReasonUnauthorizedRequest,
			confirm:   ConfirmationUnauthorized,
			confirmAs: ReauthRequired,
		},
		{
			name:      "structured 503 is transient",
			snapshot:  runtimeSnapshot(now, "error", `{"raw":"do not use"}`, true, now.Add(time.Minute), 503),
			want:      Suspect,
			reason:    ReasonHTTP503,
			confirm:   ConfirmationTransient,
			confirmAs: CredentialDown,
		},
		{
			name:     "unknown future cooldown is nonpromotable",
			snapshot: runtimeSnapshot(now, "error", `{"raw":"unknown"}`, true, now.Add(time.Minute), 0),
			want:     Suspect,
			reason:   ReasonCooldownActive,
		},
		{
			name:     "expired cooldown requires confirmed recovery",
			snapshot: runtimeSnapshot(now, "error", "", true, now.Add(-time.Minute), 0),
			want:     Suspect,
			reason:   ReasonCooldownRecoveryUnconfirmed,
		},
		{
			name:     "available residual model error is nonpromotable",
			snapshot: runtimeSnapshot(now, "error", `{"model":"fable","residual":"quota"}`, false, time.Time{}, 0),
			want:     Suspect,
			reason:   ReasonAvailableResidualError,
		},
		{
			name:      "ambiguous 403",
			snapshot:  runtimeSnapshot(now, "error", "payment_required", true, now.Add(time.Minute), 403),
			want:      Suspect,
			reason:    ReasonHTTP403,
			confirm:   ConfirmationTransient,
			confirmAs: CredentialDown,
		},
		{
			name:     "canonical quota state",
			snapshot: runtimeSnapshot(now, "error", "quota exhausted", true, now.Add(time.Hour), 0),
			want:     QuotaLimited,
			reason:   ReasonQuotaExhausted,
		},
		{
			name:     "invalid grant",
			snapshot: runtimeSnapshot(now, "error", "invalid_grant", true, time.Time{}, 0),
			want:     ReauthRequired,
			reason:   ReasonInvalidGrant,
		},
		{
			name: "operator disabled",
			snapshot: func() RuntimeSnapshot {
				s := runtimeSnapshot(now, "disabled", "", false, time.Time{}, 0)
				s.Disabled = true
				return s
			}(),
			want:   Disabled,
			reason: ReasonOperatorDisabled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClaudeClassifier().Classify(test.snapshot)
			if got.State != test.want || got.ReasonCode != test.reason || got.Confirmation != test.confirm || got.ConfirmAs != test.confirmAs {
				t.Fatalf("observation=%+v, want state=%q reason=%q confirmation=%q target=%q", got, test.want, test.reason, test.confirm, test.confirmAs)
			}
			if test.forbidQuota && got.State == QuotaLimited {
				t.Fatal("401 was incorrectly classified as quota")
			}
			if strings.Contains(got.Detail, "provider prose") || strings.Contains(string(got.ReasonCode), "provider prose") {
				t.Fatalf("raw provider text escaped classifier: %+v", got)
			}
		})
	}
}

func TestClassifiableFailureStatusUsesClosedMapping(t *testing.T) {
	for _, statusCode := range []int{401, 403, 408, 429, 500, 502, 503, 504} {
		if !IsClassifiableFailureStatus(statusCode) {
			t.Fatalf("status %d was not classifiable", statusCode)
		}
	}
	for _, statusCode := range []int{0, 200, 400, 402, 404, 409, 499, 501, 505} {
		if IsClassifiableFailureStatus(statusCode) {
			t.Fatalf("status %d escaped the closed mapping", statusCode)
		}
	}
}

func TestCanonicalRuntimeMessageRejectsFreeFormBodies(t *testing.T) {
	for input, want := range map[string]RuntimeMessageCode{
		"unauthorized":             RuntimeMessageUnauthorized,
		" INVALID_GRANT ":          RuntimeMessageInvalidGrant,
		"invalid token":            RuntimeMessageInvalidToken,
		"quota exhausted":          RuntimeMessageQuotaExhausted,
		"transient upstream error": RuntimeMessageTransientError,
		"request failed":           RuntimeMessageRequestFailed,
	} {
		if got := CanonicalRuntimeMessage(input); got != want {
			t.Fatalf("CanonicalRuntimeMessage(%q)=%q want %q", input, got, want)
		}
	}
	for _, input := range []string{
		`{"error":"unauthorized"}`,
		"prefix unauthorized",
		"unauthorized suffix",
		"weekly limit exhausted",
		"5-hour limit reached",
		"arbitrary provider body",
	} {
		if got := CanonicalRuntimeMessage(input); got != RuntimeMessageUnknown {
			t.Fatalf("free-form body %q mapped to %q", input, got)
		}
	}
}

func TestNormalizeReasonCodeRejectsLegacyRawText(t *testing.T) {
	if got := NormalizeReasonCode(`{"secret":"provider body"}`); got != ReasonUnclassifiedCredentialError {
		t.Fatalf("legacy raw reason normalized to %q", got)
	}
	if got := NormalizeReasonCode(string(ReasonHTTP503)); got != ReasonHTTP503 {
		t.Fatalf("known reason normalized to %q", got)
	}
}

func antigravitySnapshot(now time.Time, status, message string, disabled, unavailable bool, retry time.Time, httpStatus int) RuntimeSnapshot {
	entry := protocol.HostAuthFileEntry{
		AuthIndex:      "ag",
		Provider:       "antigravity",
		Type:           "antigravity",
		AccountType:    "oauth",
		Email:          "ag@example.com",
		Status:         status,
		StatusMessage:  message,
		Disabled:       disabled,
		Unavailable:    unavailable,
		NextRetryAfter: retry,
	}
	return FromHostEntry(entry, FailureEvidence{HTTPStatus: httpStatus, ObservedAt: now}, now)
}

func TestAntigravityClassifier(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	classifier, ok := Classifiers()["antigravity"]
	if !ok {
		t.Fatal("no antigravity classifier registered")
	}
	tests := []struct {
		name     string
		snapshot RuntimeSnapshot
		want     State
		reason   ReasonCode
	}{
		// CPA sets disabled, status "disabled" and status_message
		// "tos_violation" together when Google suspends the account.
		{"tos suspension", antigravitySnapshot(now, "disabled", "tos_violation", true, true, time.Time{}, 0), CredentialDown, ReasonTOSSuspended},
		{"tos suspension status only", antigravitySnapshot(now, "disabled", " TOS_VIOLATION ", false, true, time.Time{}, 0), CredentialDown, ReasonTOSSuspended},
		{"operator disabled", antigravitySnapshot(now, "disabled", "disabled via management API", true, false, time.Time{}, 0), Disabled, ReasonOperatorDisabled},
		{"disabled after restart without message", antigravitySnapshot(now, "disabled", "", true, false, time.Time{}, 0), Disabled, ReasonOperatorDisabled},
		// A re-enabled credential has its message cleared by CPA; a stale one
		// on an active credential must not alert.
		{"active with stale tos message", antigravitySnapshot(now, "active", "tos_violation", false, false, time.Time{}, 0), Healthy, ReasonNone},
		{"request invalid_grant", antigravitySnapshot(now, "error", "invalid_grant", false, true, now.Add(30*time.Minute), 0), ReauthRequired, ReasonInvalidGrant},
		{"refresh unauthorized", antigravitySnapshot(now, "error", "unauthorized", false, true, time.Time{}, 0), ReauthRequired, ReasonUnauthorizedRefreshFailed},
		{"structured 429", antigravitySnapshot(now, "error", "", false, true, now.Add(time.Hour), 429), QuotaLimited, ReasonHTTP429Quota},
		{"healthy", antigravitySnapshot(now, "active", "", false, false, time.Time{}, 0), Healthy, ReasonNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifier.Classify(test.snapshot)
			if got.State != test.want || got.ReasonCode != test.reason {
				t.Fatalf("observation=%+v, want state=%q reason=%q", got, test.want, test.reason)
			}
			if test.reason == ReasonTOSSuspended && !strings.Contains(got.Detail, "appeal") {
				t.Fatalf("suspension detail=%q", got.Detail)
			}
		})
	}
}

// The ToS rule is Antigravity's alone: every other provider keeps the generic
// classification, which reads a disabled credential as an operator action.
func TestTOSViolationLeavesOtherProvidersUnchanged(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, provider := range []string{"claude", "codex", "xai"} {
		snapshot := antigravitySnapshot(now, "disabled", "tos_violation", true, true, time.Time{}, 0)
		snapshot.Provider = provider
		got := Classifiers()[provider].Classify(snapshot)
		if got.State != Disabled || got.ReasonCode != ReasonOperatorDisabled {
			t.Fatalf("%s: observation=%+v", provider, got)
		}
		// An error-state credential carrying the message classifies exactly as
		// one with an unrecognized message.
		withTOS := antigravitySnapshot(now, "error", "tos_violation", false, true, time.Time{}, 0)
		withTOS.Provider = provider
		withUnknown := antigravitySnapshot(now, "error", "some other message", false, true, time.Time{}, 0)
		withUnknown.Provider = provider
		if a, b := Classifiers()[provider].Classify(withTOS), Classifiers()[provider].Classify(withUnknown); a != b {
			t.Fatalf("%s: tos=%+v unknown=%+v", provider, a, b)
		}
	}
	if got := CanonicalRuntimeMessage("tos_violation"); got != RuntimeMessageTOSViolation {
		t.Fatalf("CanonicalRuntimeMessage(tos_violation)=%q", got)
	}
	if got := CanonicalRuntimeMessage("tos violation suspended"); got != RuntimeMessageUnknown {
		t.Fatalf("free-form TOS text mapped to %q", got)
	}
	if got := NormalizeReasonCode(string(ReasonTOSSuspended)); got != ReasonTOSSuspended {
		t.Fatalf("persisted tos_suspended normalized to %q", got)
	}
}
