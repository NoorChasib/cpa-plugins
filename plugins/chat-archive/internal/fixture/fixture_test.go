package fixture

import "testing"

func TestRecorderBounded(t *testing.T) {
	var r Recorder
	for i := 0; i < 100; i++ {
		r.Add(Record{Hook: "stream_chunk", Chunk: i, Len: 10})
	}
	got := r.Snapshot()
	if !Enabled {
		if got != nil {
			t.Fatalf("production recorder kept %d records", len(got))
		}
		return
	}
	if len(got) != ringSize || got[0].Chunk != 100-ringSize || got[len(got)-1].Chunk != 99 {
		t.Fatalf("ring = %d records, first %d", len(got), got[0].Chunk)
	}
}
