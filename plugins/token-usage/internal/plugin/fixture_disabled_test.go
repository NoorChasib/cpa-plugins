//go:build !nativefixture

package plugin

import (
	"encoding/json"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/protocol"
)

func TestProductionStatusOmitsFixtureFields(t *testing.T) {
	p := newRegistered(t)
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, p, "1")
	response := manage(t, p, "/v0/management/plugins/token-usage/status", nil)
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, response.Body)
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fixture_records", "schema6_probe", "observed_events", "rejected_events"} {
		if _, ok := status[key]; ok {
			t.Fatalf("production exposed fixture field %q", key)
		}
	}
}
