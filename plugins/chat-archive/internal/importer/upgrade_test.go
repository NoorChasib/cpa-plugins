package importer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// legacyRec is one line whose key changed since importer 0.1.1 while its body did not.
type legacyRec struct {
	name string
	f    fileRef
	line []byte
	bl   *built // as this importer builds it: bl.prior is the 0.1.1 key
}

// legacyFixture is a main session file with a clean record and a record whose id segments are clean
// alone but complete a secret once joined into the key, and a workflow journal whose run id (only in its
// key and path) is secret-shaped. 0.1.1's archive re-checked only bodies and pieces, which hold nothing
// masked here, so it accepted both records under keys that carry the raw values.
type legacyFixture struct {
	paths    []string
	cleanKey string
	legacy   []legacyRec
	secrets  []string // must never leave the machine again
}

func writeLegacyFixture(t *testing.T, x *h) legacyFixture {
	t.Helper()
	b := testBuilder(t)
	sec := recheckSecrets["gitlab"]
	typ := strings.Repeat("Ab3D", 10)[:39] + "-x"
	if b.changes("secret_key:") || b.changes(typ) || !b.changes("secret_key:/"+typ) || !b.changes("wf_"+sec) {
		t.Fatal("fixture: the segments must be clean alone, the joined key and the run id detected")
	}
	mainRel := gProject + "/" + gSession + ".jsonl"
	cleanLine := jsonLine(t, M{"type": "user", "uuid": "u-clean-1", "sessionId": gSession, "timestamp": "2026-10-01T09:00:00Z",
		"message": M{"role": "user", "content": "hello " + x.g.Words(4)}})
	joinedLine := jsonLine(t, M{"type": typ, "sessionId": "secret_key:", "timestamp": "2026-10-01T09:00:01Z"})
	runRel := gProject + "/" + gSession + "/subagents/workflows/wf_" + sec + "/journal.jsonl"
	runLine := jsonLine(t, M{"type": "result", "agentId": "a-1", "timestamp": "2026-10-01T09:00:02Z",
		"result": M{"summary": "synthetic " + x.g.Words(6)}})
	fx := legacyFixture{secrets: []string{sec, "secret_key:/" + typ}}
	for _, w := range []struct {
		rel   string
		lines [][]byte
	}{{mainRel, [][]byte{cleanLine, joinedLine}}, {runRel, [][]byte{runLine}}} {
		p := filepath.Join(x.projects(), filepath.FromSlash(w.rel))
		var ls []string
		for _, l := range w.lines {
			ls = append(ls, string(l))
		}
		x.write(p, ls...)
		fx.paths = append(fx.paths, p)
	}
	fx.cleanKey = buildFor(t, b, mustClassify(t, RootClaudeProjects, mainRel), cleanLine).key
	for _, r := range []legacyRec{{name: "joined", f: mustClassify(t, RootClaudeProjects, mainRel), line: joinedLine},
		{name: "run id", f: mustClassify(t, RootClaudeProjects, runRel), line: runLine}} {
		r.bl = buildFor(t, b, r.f, r.line)
		if r.bl.prior == "" || r.bl.prior == r.bl.key || b.changes(r.bl.key) || !b.changes(r.bl.prior) {
			t.Fatalf("%s: key %q prior %q: want a clean key and the 0.1.1 key as prior", r.name, r.bl.key, r.bl.prior)
		}
		fx.legacy = append(fx.legacy, r)
		fx.secrets = append(fx.secrets, r.bl.prior)
	}
	return fx
}

// seedLegacy commits, from another source (an earlier install), the envelopes importer 0.1.1 sent for the
// legacy records: the same body and pieces under the 0.1.1 key, the raw path in the locator. The archive
// accepts them, as 0.1.1's did.
func seedLegacy(t *testing.T, x *h, fx legacyFixture) {
	t.Helper()
	ctx := context.Background()
	_, tok := x.srv.AddSource("importer", "earlier-install")
	c := client.New(x.srv.URL, tok, client.Options{})
	self, err := c.SourcesSelf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var envs [][]byte
	var inline []wire.BatchPiece
	for i, r := range fx.legacy {
		raw, err := canon.Marshal(wire.Envelope{
			V: wire.ProtocolV1, Kind: r.bl.kind, OutboxID: strings.Repeat("0a", 16), Seq: uint64(i + 1), EpochSeen: self.Epoch,
			Key: r.bl.prior, MaskVersion: mask.Version, CanonVersion: canon.Version, CreatedAt: r.bl.createdAt, Refs: r.bl.refs,
			Loc:  &wire.Locator{MachineID: testMachineID, FileKey: r.f.Key, LineOrdinal: 1, ByteLen: int64(len(r.line)), EndOffset: int64(len(r.line)) + 1},
			Body: r.bl.body,
		})
		if err != nil {
			t.Fatal(err)
		}
		envs = append(envs, raw)
		for _, p := range r.bl.pieces {
			inline = append(inline, wire.BatchPiece{Hash: p.Hash, Kind: p.Kind, ContentType: p.ContentType, Raw: p.Bytes})
		}
	}
	resp, err := c.Commit(ctx, self.Epoch, self.ArchiveID, inline, envs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range resp.Results {
		if res.Status != wire.StatusOK {
			t.Fatalf("seed: seq %d %s %s", res.Seq, res.Status, res.Reason)
		}
	}
}

// rewrite gives every fixture file a new inode with the same bytes, which starts a rescan from 0.
func (fx legacyFixture) rewrite(t *testing.T, x *h) {
	t.Helper()
	for _, p := range fx.paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		x.writeRaw(p+".new", data)
		if err := os.Rename(p+".new", p); err != nil {
			t.Fatal(err)
		}
		x.pin(p)
	}
}

// checkArchived: the archive holds one record per line, the legacy records under their new keys only.
func (fx legacyFixture) checkArchived(t *testing.T, x *h) {
	t.Helper()
	want := []string{fx.cleanKey}
	for _, r := range fx.legacy {
		want = append(want, r.bl.key)
	}
	slices.Sort(want)
	if got := x.srv.Keys(); !slices.Equal(got, want) {
		t.Fatalf("archived keys %d, want %d: the clean key and each legacy record under its new key, once", len(got), len(want))
	}
}

// checkNothingLeaks: no request since seq from of the archive's log holds a raw secret, the joined form or
// a 0.1.1 key, and detection finds nothing in any envelope or tombstone this importer sent.
func (fx legacyFixture) checkNothingLeaks(t *testing.T, x *h, from int) {
	t.Helper()
	for i, body := range x.srv.RequestBodies()[from:] {
		for j, s := range fx.secrets {
			if bytes.Contains(body, []byte(s)) {
				t.Errorf("request %d holds secret %d", from+i, j)
			}
		}
	}
	m := testBuilder(t).m
	for _, r := range x.envelopes() {
		fs, err := canon.DetectPiece(wire.PieceJSON, "", r.Raw, m)
		if err != nil || len(fs) != 0 {
			t.Errorf("envelope %s: detection finds %d (%v)", r.Envelope.Key, len(fs), err)
		}
	}
	for _, tb := range x.srv.Tombstones(x.sourceID) {
		if fs := m.Detect(tb.Key); len(fs) != 0 {
			t.Errorf("tombstone %d: its key is detected", tb.Seq)
		}
	}
}

// received returns what the archive answered this importer for key.
func received(x *h, key string) []wire.Status {
	var out []wire.Status
	for _, r := range x.envelopes() {
		if r.Envelope.Key == key {
			out = append(out, r.Status)
		}
	}
	return out
}

// TestLegacyKeyedRecordsReconciled: records importer 0.1.1 archived under keys that carry a raw
// secret-shaped value are sent under their clean keys with the sha256 of the 0.1.1 key, never the key
// itself. An archive that reconciles prior keys answers duplicate and moves each record to its new key,
// on a replay (a new install reads every line from 0) and on an inode rescan, so no second record is
// archived and no raw secret leaves the machine.
func TestLegacyKeyedRecordsReconciled(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.srv.SetPriorKeys(true)
	fx := writeLegacyFixture(t, x)
	seedLegacy(t, x, fx)
	from := len(x.srv.RequestBodies())

	x.mustRun()
	for _, r := range fx.legacy {
		if got := received(x, r.bl.key); !slices.Equal(got, []wire.Status{wire.StatusDuplicate}) {
			t.Errorf("%s: answered %v, want one duplicate", r.name, got)
		}
	}
	for _, e := range x.envelopes() {
		want := []wire.Hash(nil)
		for _, r := range fx.legacy {
			if e.Envelope.Key == r.bl.key {
				want = []wire.Hash{wire.KeySHA256(r.bl.prior)}
			}
		}
		if !slices.Equal(e.Envelope.PriorKeySHA256, want) {
			t.Errorf("%s: prior_key_sha256 %v, want %v", e.Envelope.Key, e.Envelope.PriorKeySHA256, want)
		}
	}
	fx.checkArchived(t, x)

	n := len(x.envelopes())
	fx.rewrite(t, x)
	x.mustRun()
	again := x.envelopes()[n:]
	if len(again) != 3 || statuses(again)[wire.StatusDuplicate] != 3 {
		t.Fatalf("rescan: %d envelopes, statuses %v, want 3 duplicate", len(again), statuses(again))
	}
	for _, e := range again {
		if e.Envelope.Loc.ScanGen != 1 {
			t.Errorf("%s: scan_gen %d, want a rescan", e.Envelope.Key, e.Envelope.Loc.ScanGen)
		}
	}
	fx.checkArchived(t, x)
	fx.checkNothingLeaks(t, x, from)
	x.checkHW()
}

// TestLegacyKeyedRecordsHeldWithoutReconciliation: against an archive that does not advertise
// accepted.prior_key_sha256, such a record is neither sent under its new key (a second record) nor under
// the 0.1.1 key (the raw value): it becomes a local key_migration dead letter whose tombstone carries the
// clean key. A plain requeue sends it once the archive reconciles prior keys.
func TestLegacyKeyedRecordsHeldWithoutReconciliation(t *testing.T) {
	x := newH(t)
	x.enroll()
	fx := writeLegacyFixture(t, x)
	seedLegacy(t, x, fx)
	from := len(x.srv.RequestBodies())

	x.mustRun()
	tombs := map[string]string{}
	for _, tb := range x.srv.Tombstones(x.sourceID) {
		tombs[tb.Key] = tb.Reason
	}
	st := x.state()
	rows, err := st.deadRows(deadKeyMigration, "dead", 0)
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for _, d := range rows {
		held = append(held, d.Key)
	}
	for _, r := range fx.legacy {
		if got := received(x, r.bl.key); len(got) != 0 {
			t.Errorf("%s: sent under its new key (%v) to an archive that cannot reconcile it", r.name, got)
		}
		if tombs[r.bl.key] != wire.TombRejectedLocal || !slices.Contains(held, r.bl.key) {
			t.Errorf("%s: tombstone %q, held %v", r.name, tombs[r.bl.key], slices.Contains(held, r.bl.key))
		}
	}
	if got := received(x, fx.cleanKey); !slices.Equal(got, []wire.Status{wire.StatusOK}) {
		t.Errorf("clean record answered %v, want ok", got)
	}
	keys := x.srv.Keys()
	if len(keys) != 3 || !slices.Contains(keys, fx.cleanKey) {
		t.Fatalf("%d archived keys, want the clean record and the two seeded ones", len(keys))
	}
	x.checkHW()

	// Before the archive reconciles prior keys, a requeue holds them again and the dry run says so.
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", deadKeyMigration, "--dry-run"}, x.env); rc != 0 {
		t.Fatalf("requeue --dry-run rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	for _, r := range fx.legacy {
		if !strings.Contains(x.out.String(), "check "+r.bl.key+" "+deadKeyMigration+"\n") {
			t.Errorf("%s: dry run does not report it held:\n%s", r.name, x.out.String())
		}
	}
	if rc := Main([]string{"requeue", "--reason", deadKeyMigration}, x.env); rc != 0 {
		t.Fatalf("requeue rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	if rows, _ := st.deadRows(deadKeyMigration, "dead", 0); len(rows) != 2 || len(x.srv.Keys()) != 3 {
		t.Fatalf("%d held rows and %d archived keys after a requeue the archive cannot reconcile", len(rows), len(x.srv.Keys()))
	}
	for _, r := range fx.legacy {
		if got := received(x, r.bl.key); len(got) != 0 {
			t.Errorf("%s: requeue sent it (%v) before the archive reconciles prior keys", r.name, got)
		}
	}
	x.checkHW()

	x.srv.SetPriorKeys(true)
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", deadKeyMigration}, x.env); rc != 0 {
		t.Fatalf("requeue rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	if !strings.Contains(x.out.String(), "found=2 gone=0") {
		t.Fatalf("requeue output:\n%s", x.out.String())
	}
	for _, r := range fx.legacy {
		if got := received(x, r.bl.key); !slices.Equal(got, []wire.Status{wire.StatusDuplicate}) {
			t.Errorf("%s: requeue answered %v, want one duplicate", r.name, got)
		}
	}
	if rows, _ := st.deadRows(deadKeyMigration, "requeued", 0); len(rows) != 2 {
		t.Errorf("%d requeued rows, want 2", len(rows))
	}
	fx.checkArchived(t, x)
	fx.checkNothingLeaks(t, x, from)
	x.checkHW()
}
