package client

import (
	"encoding/json"
	"testing"
)

// The classification both the dashboard and the spend rely on, case by case.
// The grants are written as the usage response carries them and decoded the
// way both readers decode it.
func TestClaudeResetKind(t *testing.T) {
	cases := []struct {
		name, grant, want string
	}{
		{"fable by gives_back_weekly", `{"gives_back_weekly":true}`, ResetKindFable},
		{"fable wins over a type", `{"gives_back_weekly":true,"type":"full","clears":["five_hour","seven_day"]}`, ResetKindFable},
		{"gives_back_weekly false falls through to type", `{"gives_back_weekly":false,"type":"weekly"}`, ResetKindWeekly},
		{"type full", `{"type":"full"}`, ResetKindFull},
		{"type session", `{"type":"session","clears":["seven_day"]}`, ResetKindSession},
		{"type weekly", `{"type":"weekly"}`, ResetKindWeekly},
		{"type null falls through to clears", `{"type":null,"clears":["five_hour"]}`, ResetKindSession},
		{"clears five_hour alone", `{"clears":["five_hour"]}`, ResetKindSession},
		{"clears weekly windows alone", `{"clears":["seven_day","seven_day_opus"]}`, ResetKindWeekly},
		{"clears both", `{"clears":["five_hour","seven_day"]}`, ResetKindFull},
		{"no clears predates kinds", `{}`, ResetKindFull},
		{"empty clears", `{"clears":[]}`, ResetKindFull},
		{"null clears", `{"clears":null}`, ResetKindFull},
		{"unknown windows are dropped first", `{"clears":["five_hour","seven_day_brand_new"]}`, ResetKindSession},
		{"only unknown windows", `{"clears":["seven_day_brand_new"]}`, ResetKindFull},
		{"non-string window is dropped", `{"clears":["five_hour",7]}`, ResetKindSession},

		{"type this build does not know", `{"type":"monthly","clears":["five_hour","seven_day"]}`, ResetKindUnknown},
		{"type fable without the flag", `{"type":"fable"}`, ResetKindUnknown},
		{"malformed gives_back_weekly", `{"gives_back_weekly":"yes","type":"full"}`, ResetKindUnknown},
		{"malformed type", `{"type":3}`, ResetKindUnknown},
		{"malformed clears", `{"clears":"five_hour"}`, ResetKindUnknown},

		// The order docs/cache-format.md's table gives, top row first: each
		// field is settled before the next is read, so a later field that is
		// malformed cannot undo an earlier one that decided.
		{"fable wins over a malformed type", `{"gives_back_weekly":true,"type":3}`, ResetKindFable},
		{"a known type wins over malformed clears", `{"type":"weekly","clears":"five_hour"}`, ResetKindWeekly},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var grant map[string]any
			if err := json.Unmarshal([]byte(c.grant), &grant); err != nil {
				t.Fatal(err)
			}
			if got := ClaudeResetKind(grant); got != c.want {
				t.Fatalf("ClaudeResetKind(%s) = %q, want %q", c.grant, got, c.want)
			}
		})
	}
}

func TestResetKindRank(t *testing.T) {
	order := []string{ResetKindFull, ResetKindFable, ResetKindWeekly, ResetKindSession, ResetKindUnknown}
	for i := 1; i < len(order); i++ {
		if ResetKindRank(order[i-1]) >= ResetKindRank(order[i]) {
			t.Fatalf("%q does not sort before %q", order[i-1], order[i])
		}
	}
	if ResetKindRank("brand_new") != ResetKindRank(ResetKindUnknown) {
		t.Fatal("a kind outside the vocabulary must sort with the unknown")
	}
}
