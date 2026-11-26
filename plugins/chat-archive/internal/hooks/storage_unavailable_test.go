package hooks

// dropped.storage_unavailable at the engine (plugin 0.1.6). Every value is synthetic.

import "testing"

// TestStorageUnavailableGapAfterStop: once admission has stopped while the config still says
// capture on (a reconfigure's gap, or a final stop before the config goes off), each hook call is
// counted storage_unavailable; a payload chunk only in full capture. While stopping, calls keep
// 0.1.5's stopped count. With the config off nothing is counted. An unavailable config counts the
// same calls by its configured mode.
func TestStorageUnavailableGapAfterStop(t *testing.T) {
	calls := func(e *Engine) {
		e.BeforeAuth(rpc(map[string]any{"RequestID": "gap-1", "SourceFormat": "claude", "Body": []byte(`{"synthetic":1}`)}))
		e.AfterAuth(rpc(map[string]any{"RequestID": "gap-1"}))
		e.StreamChunk(rpc(map[string]any{"RequestID": "gap-1", "ChunkIndex": -1}))
		e.StreamChunk(rpc(map[string]any{"RequestID": "gap-1", "ChunkIndex": 0, "Body": []byte("data: {}\n\n")}))
		e.Complete(rpc(map[string]any{"RequestID": "gap-1", "Model": "model-x", "Outcome": "succeeded"}))
	}
	for _, c := range []struct {
		capture       string
		stopped, gap  uint64
		unavailableIn uint64
	}{{"full", 5, 5, 5}, {"metadata", 5, 4, 4}} {
		e := running(t, c.capture)
		e.state.Store(StateStopping)
		calls(e)
		if d := e.Stats.Dropped(); d["stopped"] != c.stopped || d["storage_unavailable"] != 0 {
			t.Fatalf("%s while stopping: %v, want stopped %d", c.capture, d, c.stopped)
		}
		e.Stopped()
		calls(e)
		if d := e.Stats.Dropped(); d["storage_unavailable"] != c.gap || d["stopped"] != c.stopped {
			t.Fatalf("%s stopped with capture on: %v, want storage_unavailable %d", c.capture, d, c.gap)
		}
		e.SetConfig(&Config{Capture: "off", MaxRPC: 8 << 20})
		calls(e)
		if d := e.Stats.Dropped(); d["storage_unavailable"] != c.gap {
			t.Fatalf("%s with capture off: %v", c.capture, d)
		}
		e.SetConfig(&Config{Capture: "off", MaxRPC: 8 << 20, Unavailable: c.capture})
		calls(e)
		if d := e.Stats.Dropped(); d["storage_unavailable"] != c.gap+c.unavailableIn {
			t.Fatalf("%s unavailable: %v, want storage_unavailable %d", c.capture, d, c.gap+c.unavailableIn)
		}
	}
}
