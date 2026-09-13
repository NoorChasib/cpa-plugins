package main

import (
	"errors"
	"testing"
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
