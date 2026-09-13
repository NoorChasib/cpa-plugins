package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/auto-baseline/internal/learner"
)

var t0 = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	return entries
}

func TestLoadMissingReturnsFreshState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "missing")
	st, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st == nil || st.SchemaVersion != SchemaVersion || st.Baselines == nil || st.Counters.RejectReasons == nil {
		t.Fatalf("fresh state malformed: %+v", st)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st := New()
	v := fingerprint.MustParseVersion("2.1.258")
	cand := fingerprint.Candidate{Provider: fingerprint.ProviderClaude, Version: v, UserAgent: fingerprint.CanonicalClaudeUserAgent(v), PackageVersion: "0.112.1", RuntimeVersion: "v26.3.0"}
	st.Baselines[fingerprint.ProviderClaude] = Baseline{Version: fingerprint.MustParseVersion("2.1.220"), UserAgent: fingerprint.CompiledClaudeUserAgent, ObservedAt: t0}
	st.Pending = []learner.Pending{{Key: cand.Key(), Candidate: cand, Records: []learner.Record{{SessionID: "s1", At: t0}}, FirstSeen: t0, LastSeen: t0}}
	st.RecordPromotion(Promotion{At: t0, Provider: fingerprint.ProviderClaude, From: fingerprint.MustParseVersion("2.1.220"), Candidate: cand, Source: "observed", Observations: 3, DistinctSessions: 2})
	st.Counters.Requests = 10
	st.Counters.RejectReasons["x"] = 2
	st.LastError = "boom"
	st.LastErrorAt = t0

	if err := Save(dir, st, t0.Add(time.Minute)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	entries := mustReadDir(t, dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}

	back, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !back.SavedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("SavedAt = %s", back.SavedAt)
	}
	if back.Baselines[fingerprint.ProviderClaude].Version.String() != "2.1.220" {
		t.Errorf("baseline = %+v", back.Baselines)
	}
	if len(back.Pending) != 1 || back.Pending[0].Key != cand.Key() || back.Pending[0].Records[0].SessionID != "s1" {
		t.Errorf("pending = %+v", back.Pending)
	}
	if lp := back.LastPromotion[fingerprint.ProviderClaude]; lp == nil || lp.Candidate.PackageVersion != "0.112.1" {
		t.Errorf("last promotion = %+v", lp)
	}
	if len(back.History) != 1 || back.Counters.Requests != 10 || back.Counters.RejectReasons["x"] != 2 {
		t.Errorf("history/counters = %+v / %+v", back.History, back.Counters)
	}
	if !back.LastWriteAt[fingerprint.ProviderClaude].Equal(t0) {
		t.Errorf("LastWriteAt = %v", back.LastWriteAt)
	}
	if back.LastError != "boom" {
		t.Errorf("LastError = %q", back.LastError)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	st := New()
	for i := 0; i < MaxHistory+7; i++ {
		st.RecordPromotion(Promotion{At: t0.Add(time.Duration(i) * time.Minute), Provider: fingerprint.ProviderCodex, DryRun: true})
	}
	if len(st.History) != MaxHistory {
		t.Fatalf("history = %d", len(st.History))
	}
	if !st.History[0].At.Equal(t0.Add(7 * time.Minute)) {
		t.Errorf("oldest retained = %s", st.History[0].At)
	}
	if _, ok := st.LastWriteAt[fingerprint.ProviderCodex]; ok {
		t.Error("dry-run promotion must not record a write time")
	}
}

func TestLoadCorruptOrWrongSchemaStartsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v", err)
	}
	if st == nil || len(st.Pending) != 0 {
		t.Fatalf("state = %+v", st)
	}

	if err := os.WriteFile(Path(dir), []byte(`{"schema_version": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = Load(dir)
	if err == nil || !strings.Contains(err.Error(), "schema") || st == nil {
		t.Fatalf("schema mismatch: err=%v state=%v", err, st)
	}

	// Sparse oversized file: only the size is checked, nothing is read, so a
	// file far larger than the limit costs no allocation.
	f, err := os.OpenFile(Path(dir), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(maxStateBytes) * 64); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Load(dir)
	if err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("oversize: %v", err)
	}
	if st == nil || len(st.Pending) != 0 {
		t.Fatalf("oversize did not start fresh: %+v", st)
	}
}

func TestSaveRejectsNilAndUnwritableDir(t *testing.T) {
	if err := Save(t.TempDir(), nil, t0); err == nil {
		t.Fatal("nil state accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A regular file where the dir should be cannot be MkdirAll'd.
	if err := Save(filepath.Join(file, "sub"), New(), t0); err == nil {
		t.Fatal("save into a non-directory succeeded")
	}
}

func TestCloneIsDeep(t *testing.T) {
	st := New()
	st.Counters.RejectReasons["x"] = 1
	st.Pending = []learner.Pending{{Key: "k", Records: []learner.Record{{SessionID: "s", At: t0}}}}
	st.RecordPromotion(Promotion{At: t0, Provider: fingerprint.ProviderClaude, AwaitingReload: true})
	cp := st.Clone()
	if !cp.LastPromotion[fingerprint.ProviderClaude].AwaitingReload || !cp.History[0].AwaitingReload {
		t.Fatal("Clone dropped AwaitingReload")
	}
	cp.Counters.RejectReasons["x"] = 99
	cp.Pending[0].Records[0].SessionID = "tampered"
	cp.LastPromotion[fingerprint.ProviderClaude].Observations = 42
	cp.History[0].Observations = 42
	if st.Counters.RejectReasons["x"] != 1 || st.Pending[0].Records[0].SessionID != "s" || st.LastPromotion[fingerprint.ProviderClaude].Observations != 0 || st.History[0].Observations != 0 {
		t.Fatal("Clone shares memory with the original")
	}
	var nilState *State
	if nilState.Clone() == nil {
		t.Fatal("nil clone")
	}
}
