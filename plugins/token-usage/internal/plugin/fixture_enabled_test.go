//go:build nativefixture

package plugin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/usage"
)

func TestFixtureRecorderFIFOAndSnapshot(t *testing.T) {
	var recorder fixtureRecorder
	snapshot := map[string]any{"diagnostics": map[string]string{"observed_events": "70", "rejected_events": "4"}}
	value := map[string]any{}
	recorder.addStatus(value, snapshot)
	if records := value["fixture_records"].([]protocol.UsageRecord); records == nil || len(records) != 0 {
		t.Fatalf("empty fixture records must be an empty array: %#v", records)
	}
	if value["schema6_probe"] != "<model>&literal" || value["observed_events"] != "70" || value["rejected_events"] != "4" {
		t.Fatalf("fixture status changed: %v", value)
	}
	var want []protocol.UsageRecord
	for i := range 66 {
		e := usage.Event{Provider: "p", Model: fmt.Sprint(i), ExecutorType: "fixture", Alias: "alias", RequestedAt: testNow, Generate: true, Failed: true, Status: 503, Tokens: protocol.UsageDetail{InputTokens: int64(i)}}
		recorder.record(e)
		want = append(want, e.WireRecord())
	}
	recorder.addStatus(value, snapshot)
	records := value["fixture_records"].([]protocol.UsageRecord)
	if !reflect.DeepEqual(records, want[2:]) {
		t.Fatalf("fixture FIFO did not retain the latest 64 records: %#v", records)
	}
	recorder.record(usage.Event{Provider: "p", Model: "66"})
	if !reflect.DeepEqual(records, want[2:]) {
		t.Fatal("later observation changed an earlier status snapshot")
	}
	records[1].Model = "caller-mutated"
	recorder.addStatus(value, snapshot)
	if value["fixture_records"].([]protocol.UsageRecord)[0].Model != "3" {
		t.Fatal("status snapshot shares the recorder backing array")
	}
}

func TestFixtureRecorderConcurrentStatus(t *testing.T) {
	var recorder fixtureRecorder
	snapshot := map[string]any{"diagnostics": map[string]string{}}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				recorder.record(usage.Event{Provider: "p", Model: "m"})
				value := map[string]any{}
				recorder.addStatus(value, snapshot)
				if len(value["fixture_records"].([]protocol.UsageRecord)) > 64 {
					t.Error("fixture capacity exceeded")
				}
			}
		}()
	}
	wg.Wait()
}

func TestFixtureRecordsOnlyAdmittedUsage(t *testing.T) {
	p := newRegistered(t)
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(validUsage)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodUsageHandle, []byte(`{`)); err == nil {
		t.Fatal("expected invalid usage rejection")
	}
	waitCommitted(t, p, "1")
	response := manage(t, p, "/v0/management/plugins/token-usage/status", nil)
	var status struct {
		Records  []protocol.UsageRecord `json:"fixture_records"`
		Observed string                 `json:"observed_events"`
		Rejected string                 `json:"rejected_events"`
	}
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(status.Records) != 1 || status.Observed != "2" || status.Rejected != "1" {
		t.Fatalf("fixture admission/status changed: %d %s", response.StatusCode, response.Body)
	}
}
