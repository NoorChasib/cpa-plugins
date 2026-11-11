package importer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// zstRel is a rollout's path under the codex-sessions root.
var zstRel = filepath.Join("2026", "10", "02", "rollout-2026-10-02T00-00-00-"+gThread+".jsonl")

const zstKey = "codex-sessions/2026/10/02/rollout-2026-10-02T00-00-00-" + gThread + ".jsonl"

// zstBytes compresses lines the way Codex compresses an idle rollout (zstd level 3).
func zstBytes(t *testing.T, lines ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := zstd.NewWriter(&b, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// zstLines is a five-line rollout of a provider CPA does not cover (every record full).
func zstLines(g *synth.Gen) []string {
	cx := &synth.Codex{G: g, ThreadID: gThread, Session: gThread}
	return []string{cx.SessionMeta("openai", "", 0), cx.UserMessage("q1"), cx.AssistantMessage("a1"), cx.TokenUsage("resp_a"), cx.UserMessage("q2")}
}

type locOf struct {
	key, fileKey      string
	line, off, n, end int64
	gen               uint32
	status            wire.Status
}

func locs(rs []clienttest.Received) []locOf {
	out := make([]locOf, len(rs))
	for i, r := range rs {
		l := r.Envelope.Loc
		out[i] = locOf{r.Envelope.Key, l.FileKey, l.LineOrdinal, l.ByteOffset, l.ByteLen, l.EndOffset, l.ScanGen, r.Status}
	}
	return out
}

func (x *h) statusLine(prefix string) string {
	x.t.Helper()
	x.out.Reset()
	if rc := Main([]string{"status"}, x.env); rc != 0 {
		x.t.Fatalf("status rc=%d", rc)
	}
	for _, l := range strings.Split(x.out.String(), "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// TestCompressedRolloutReadsAsPlain: a rollout Codex compressed in place uploads exactly what its plain
// form uploads, under the plain file's key and with the decompressed content's offsets, and --session finds
// it.
func TestCompressedRolloutReadsAsPlain(t *testing.T) {
	p := newH(t)
	p.enroll()
	ls := zstLines(p.g)
	p.write(filepath.Join(p.codex(), zstRel), ls...)
	p.mustRun()

	z := newH(t)
	z.enroll()
	z.writeRaw(filepath.Join(z.codex(), zstRel+zstSuffix), zstBytes(t, ls...))
	z.mustRun("--session", gThread)

	a, b := locs(p.envelopes()), locs(z.envelopes())
	if len(a) != len(ls) || len(b) != len(a) {
		t.Fatalf("plain %d envelopes, compressed %d, want %d", len(a), len(b), len(ls))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("envelope %d: plain %+v, compressed %+v", i, a[i], b[i])
		}
		if b[i].fileKey != zstKey || b[i].status != wire.StatusOK || b[i].gen != 0 {
			t.Fatalf("envelope %d: %+v", i, b[i])
		}
	}
	if l := z.statusLine("cursors:"); !strings.Contains(l, "backlog 0 bytes") {
		t.Errorf("status after the read: %q", l)
	}
	z.checkHW()
}

// TestCodexCompressionCarriesCursor: when Codex compresses a rollout the importer read plain, the
// plain file's cursor carries over (same scan_gen): only the line written since is uploaded, and an
// unchanged compressed file is skipped.
func TestCodexCompressionCarriesCursor(t *testing.T) {
	x := newH(t)
	x.enroll()
	ls := zstLines(x.g)
	path := filepath.Join(x.codex(), zstRel)
	x.write(path, ls[:4]...)
	x.mustRun()
	n0 := len(x.envelopes())
	if n0 != 4 {
		t.Fatalf("%d envelopes after the plain read, want 4", n0)
	}
	// One more line lands, then Codex compresses the idle rollout in place.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	x.writeRaw(path+zstSuffix, zstBytes(t, ls...))
	x.mustRun()
	got := locs(x.envelopes())
	if len(got) != n0+1 {
		t.Fatalf("%d envelopes after compression, want %d (only the new line)", len(got), n0+1)
	}
	if last := got[len(got)-1]; last.line != 5 || last.gen != 0 || last.status != wire.StatusOK || last.fileKey != zstKey {
		t.Fatalf("new line %+v", last)
	}
	x.mustRun()
	if n := len(x.envelopes()); n != n0+1 {
		t.Fatalf("an unchanged compressed rollout uploaded again: %d envelopes", n)
	}
	x.checkHW()
}

// TestCompressedSiblingReadsPlain: with both forms on disk (Codex restored the plain file and appended to
// it before deleting the .zst), the plain file is read and the compressed one ignored.
func TestCompressedSiblingReadsPlain(t *testing.T) {
	x := newH(t)
	x.enroll()
	ls := zstLines(x.g)
	path := filepath.Join(x.codex(), zstRel)
	x.writeRaw(path+zstSuffix, zstBytes(t, ls[:2]...))
	x.write(path, ls...)
	x.mustRun()
	got := locs(x.envelopes())
	if len(got) != len(ls) {
		t.Fatalf("%d envelopes, want %d (the plain file once)", len(got), len(ls))
	}
	for i, l := range got {
		if l.line != int64(i+1) || l.gen != 0 || l.status != wire.StatusOK {
			t.Fatalf("envelope %d: %+v", i, l)
		}
	}
	x.checkHW()
}

// TestCompressedRolloutRewriteRescans: a compressed rollout whose head changed is read again from the start
// with scan_gen + 1.
func TestCompressedRolloutRewriteRescans(t *testing.T) {
	x := newH(t)
	x.enroll()
	ls := zstLines(x.g)
	path := filepath.Join(x.codex(), zstRel+zstSuffix)
	x.writeRaw(path, zstBytes(t, ls...))
	x.mustRun()
	cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
	changed := append([]string{ls[0], cx.UserMessage("q1 rewritten")}, ls[2:]...)
	x.writeRaw(path, zstBytes(t, changed...))
	x.mustRun()
	got := locs(x.envelopes())
	if len(got) != 2*len(ls) {
		t.Fatalf("%d envelopes, want %d", len(got), 2*len(ls))
	}
	for _, l := range got[len(ls):] {
		if l.gen != 1 {
			t.Fatalf("rescan envelope %+v, want scan_gen 1", l)
		}
	}
	x.checkHW()
}

// TestDamagedCompressedRollout: a stream that is not zstd, or is cut short, is never recorded as read, so
// the complete file is read once it appears.
func TestDamagedCompressedRollout(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func(full []byte) []byte
	}{
		{"not zstd", func([]byte) []byte { return []byte("not a zstd stream\n") }},
		{"truncated", func(full []byte) []byte { return full[:len(full)-8] }},
		{"empty", func([]byte) []byte { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newH(t)
			x.enroll()
			ls := zstLines(x.g)
			full := zstBytes(t, ls...)
			path := filepath.Join(x.codex(), zstRel+zstSuffix)
			x.writeRaw(path, tc.data(full))
			x.run()
			curs, err := x.state().cursors()
			if err != nil {
				t.Fatal(err)
			}
			if c, ok := curs[zstKey]; ok && c.MtimeNS != 0 {
				t.Fatalf("a damaged rollout counted as read: %+v", c)
			}
			x.writeRaw(path, full)
			x.mustRun()
			keys := map[string]bool{}
			for _, l := range locs(x.envelopes()) {
				keys[l.key] = true
			}
			if len(keys) != len(ls) {
				t.Fatalf("%d distinct keys after the repair, want %d", len(keys), len(ls))
			}
			if l := x.statusLine("cursors:"); !strings.Contains(l, "backlog 0 bytes") {
				t.Errorf("status after the repair: %q", l)
			}
			x.checkHW()
		})
	}
}

// TestBackfillOrdersCompressedRollouts: a compressed rollout sorts by its first record's timestamp too.
func TestBackfillOrdersCompressedRollouts(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, data []byte) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	meta := func(ts string) string { return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"id":"x"}}` }
	write("2026/11/01/rollout-b.jsonl", []byte(meta("2026-11-01T00:00:00Z")+"\n"))
	write("2026/05/18/rollout-c.jsonl.zst", zstBytes(t, meta("2026-05-18T00:00:00Z")))
	write("2026/10/01/rollout-a.jsonl.zst", zstBytes(t, meta("2026-10-01T00:00:00Z")))
	files, err := discover(map[string]string{RootCodexSessions: dir})
	if err != nil {
		t.Fatal(err)
	}
	orderByFirstRecord(files)
	var got []string
	for _, f := range files {
		got = append(got, f.Rel)
	}
	want := "2026/05/18/rollout-c.jsonl 2026/10/01/rollout-a.jsonl 2026/11/01/rollout-b.jsonl"
	if strings.Join(got, " ") != want {
		t.Fatalf("order %q, want %q", strings.Join(got, " "), want)
	}
}

// TestRequeueFromCompressedRollout: a dead record of a rollout Codex compressed since is rebuilt from the
// decompressed content.
func TestRequeueFromCompressedRollout(t *testing.T) {
	p := newH(t)
	p.enroll()
	ls := zstLines(p.g)
	p.write(filepath.Join(p.codex(), zstRel), ls...)
	p.mustRun()
	bad := p.envelopes()[2].Envelope.Key // line 3; keys do not depend on the machine

	x := newH(t)
	x.enroll()
	path := filepath.Join(x.codex(), zstRel)
	x.write(path, ls...)
	x.srv.RejectKey(bad, wire.ReasonSchema)
	x.mustRun()
	dead, err := x.state().deadRows("", "", 0)
	if err != nil || len(dead) != 1 || dead[0].Key != bad {
		t.Fatalf("dead rows %+v (%v)", dead, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	x.writeRaw(path+zstSuffix, zstBytes(t, ls...))
	x.srv.AcceptKey(bad)
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", "schema"}, x.env); rc != 0 {
		t.Fatalf("requeue rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	all := x.envelopes()
	if last := all[len(all)-1]; last.Envelope.Key != bad || last.Status != wire.StatusOK || last.Envelope.Loc.LineOrdinal != 3 {
		t.Fatalf("requeued: key match %v, %s, line %d", last.Envelope.Key == bad, last.Status, last.Envelope.Loc.LineOrdinal)
	}
	x.checkHW()
}

// TestClassifyCompressedRollout: only rollout-*.jsonl.zst is a compressed rollout, keyed by its plain name.
func TestClassifyCompressedRollout(t *testing.T) {
	f, ok := classify(RootCodexSessions, "2026/10/02/rollout-x-"+gThread+".jsonl.zst")
	if !ok || !f.Zstd || f.Role != RoleCodexRollout || f.Rel != "2026/10/02/rollout-x-"+gThread+".jsonl" ||
		f.Key != "codex-sessions/2026/10/02/rollout-x-"+gThread+".jsonl" || !matchesSession(f, gThread) {
		t.Fatalf("%+v ok=%v", f, ok)
	}
	if fileKeyShape(f.Key) != "codex-sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl" {
		t.Errorf("shape %q", fileKeyShape(f.Key))
	}
	for _, rel := range []string{"2026/10/02/rollout-x.zst", "2026/10/02/rollout-x.jsonl.gz", "2026/10/02/other.jsonl.zst"} {
		if _, ok := classify(RootCodexSessions, rel); ok {
			t.Errorf("%s classified", rel)
		}
	}
	if _, ok := classify(RootClaudeProjects, "p/s.jsonl.zst"); ok {
		t.Error("a compressed Claude Code file classified")
	}
}

// TestCodexRestoreCarriesCursor: when Codex resumes a thread it compressed, it writes the plain file back (a
// new inode) and appends to it. The cursor the compressed read left carries over: only the new line goes.
func TestCodexRestoreCarriesCursor(t *testing.T) {
	x := newH(t)
	x.enroll()
	ls := zstLines(x.g)
	path := filepath.Join(x.codex(), zstRel)
	x.writeRaw(path+zstSuffix, zstBytes(t, ls[:4]...))
	x.mustRun()
	if n := len(x.envelopes()); n != 4 {
		t.Fatalf("%d envelopes from the compressed rollout, want 4", n)
	}
	x.write(path, ls...)
	if err := os.Remove(path + zstSuffix); err != nil {
		t.Fatal(err)
	}
	x.mustRun()
	got := locs(x.envelopes())
	if len(got) != 5 {
		t.Fatalf("%d envelopes after the restore, want 5 (only the new line)", len(got))
	}
	if last := got[4]; last.line != 5 || last.gen != 0 || last.status != wire.StatusOK || last.fileKey != zstKey {
		t.Fatalf("new line %+v", last)
	}
	x.checkHW()
}

// TestTruncatedCompressedKeepsCursor: a compressed copy cut short is damage, not shorter content: the cursor
// the plain file left stays, nothing is uploaded again, and the repaired file only records its stat.
func TestTruncatedCompressedKeepsCursor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines int
	}{{"single block", 5}, {"several blocks", 4000}} {
		t.Run(tc.name, func(t *testing.T) {
			x := newH(t)
			x.enroll()
			cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
			ls := []string{cx.SessionMeta("openai", "", 0)}
			for i := 1; i < tc.lines; i++ {
				ls = append(ls, cx.UserMessage(fmt.Sprintf("q%d %s", i, synth.Text(256, uint64(i)))))
			}
			path := filepath.Join(x.codex(), zstRel)
			x.write(path, ls...)
			x.mustRun()
			n0 := len(x.envelopes())
			if n0 != tc.lines {
				t.Fatalf("%d envelopes from the plain read, want %d", n0, tc.lines)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			full := zstBytes(t, ls...)
			x.writeRaw(path+zstSuffix, full[:len(full)/2])
			x.run()
			if n := len(x.envelopes()); n != n0 {
				t.Fatalf("a truncated .zst uploaded %d records again", n-n0)
			}
			x.writeRaw(path+zstSuffix, full)
			x.mustRun()
			if n := len(x.envelopes()); n != n0 {
				t.Fatalf("the repaired .zst uploaded %d records again", n-n0)
			}
			curs, err := x.state().cursors()
			if err != nil {
				t.Fatal(err)
			}
			if c := curs[zstKey]; c.ScanGen != 0 || c.LineOrdinal != int64(tc.lines) {
				t.Fatalf("cursor %+v, want scan_gen 0 at line %d", c, tc.lines)
			}
			if l := x.statusLine("cursors:"); !strings.Contains(l, "backlog 0 bytes") {
				t.Errorf("status after the repair: %q", l)
			}
			x.checkHW()
		})
	}
}

// TestCompressedContentHelpers: contentSize and readContentAt see the decompressed content.
func TestCompressedContentHelpers(t *testing.T) {
	dir := t.TempDir()
	content := "line one\nline two\nline three\n"
	plain, zst := filepath.Join(dir, "r.jsonl"), filepath.Join(dir, "r.jsonl.zst")
	if err := os.WriteFile(plain, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zst, zstBytes(t, "line one", "line two", "line three"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []fileRef{{Path: plain}, {Path: zst, Zstd: true}} {
		if err := statRef(&f); err != nil {
			t.Fatal(err)
		}
		if n := contentSize(f); n != int64(len(content)) {
			t.Errorf("%s: contentSize %d, want %d", f.Path, n, len(content))
		}
		buf := make([]byte, 8)
		if err := readContentAt(f, buf, 9); err != nil || string(buf) != "line two" {
			t.Errorf("%s: readContentAt %q (%v)", f.Path, buf, err)
		}
		if p := contentPrefix(f, 4); string(p) != "line" {
			t.Errorf("%s: contentPrefix %q", f.Path, p)
		}
	}
}
