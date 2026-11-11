package importer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func (x *h) claude(session string) *synth.Claude {
	return &synth.Claude{G: x.g, SessionID: session}
}

// byLine maps the received envelopes of one file by line ordinal, last arrival winning.
func byFile(rs []clienttest.Received, fileKeySuffix string) []clienttest.Received {
	var out []clienttest.Received
	for _, r := range rs {
		if r.Envelope.Loc != nil && strings.HasSuffix(r.Envelope.Loc.FileKey, fileKeySuffix) {
			out = append(out, r)
		}
	}
	return out
}

func checkNoConflict(t *testing.T, rs []clienttest.Received) {
	t.Helper()
	for _, r := range rs {
		if r.Status == wire.StatusConflict || r.Status == wire.StatusRejected {
			t.Errorf("seq %d %s: %s %s", r.Envelope.Seq, r.Envelope.Key, r.Status, r.Reason)
		}
	}
}

// checkHW asserts the fake's hw for the current outbox reached next_seq-1 (every seq has a receipt).
func (x *h) checkHW() {
	x.t.Helper()
	m := x.meta()
	if hw := x.srv.HW(x.sourceID, m.OutboxID); hw != m.NextSeq-1 {
		x.t.Errorf("hw %d, want %d (next_seq %d)", hw, m.NextSeq-1, m.NextSeq)
	}
}

func TestForkedCodexRollout(t *testing.T) {
	x := newH(t)
	x.enroll()
	fk := &synth.Codex{G: x.g, ThreadID: gChild, Session: gThread}
	path := filepath.Join(x.codex(), "2026", "10", "02", "rollout-2026-10-02T00-00-00-"+gChild+".jsonl")
	x.write(path, fk.SessionMeta("openai", gThread, 4), fk.ParentMeta(gThread), fk.UserMessage("inherited a"),
		fk.AssistantMessage("inherited b"), fk.UserMessage("own c"))
	x.mustRun()
	got := x.envelopes()
	if len(got) != 4 {
		t.Fatalf("%d envelopes, want 4 (the second session_meta skipped)", len(got))
	}
	want := map[int64]bool{1: false, 3: true, 4: true, 5: false} // line -> inherited
	for _, r := range got {
		rec := decodeRecord(t, r.Envelope)
		ln := r.Envelope.Loc.LineOrdinal
		inh, ok := want[ln]
		if !ok {
			t.Fatalf("unexpected line %d (%s)", ln, rec.RecordType)
		}
		if rec.Inherited != inh {
			t.Errorf("line %d inherited=%v, want %v", ln, rec.Inherited, inh)
		}
		if rec.ThreadID != gChild || rec.SessionID != gThread {
			t.Errorf("line %d ids thread=%s session=%s", ln, rec.ThreadID, rec.SessionID)
		}
	}
	x.checkHW()
}

// coveredRollout writes a cliproxyapi rollout with three calls, the second one CPA covers. Inside that
// call sit an item_completed with a large aggregated_output and a world_state with a large leaf: records
// of no call, which always stay full with their pieces. Lines (1-based):
// 1 session_meta; 2-3 call 1; 4 usage resp_a; 5 input, 6 output, 7 item_completed, 8 world_state,
// 9 output of call 2; 10 usage resp_b; 11-12 call 3; 13 usage resp_c; 14 an input of an open call.
func (x *h) coveredRollout() {
	x.t.Helper()
	cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
	x.write(filepath.Join(x.codex(), "2026", "10", "02", "rollout-2026-10-02T00-00-00-"+gThread+".jsonl"),
		cx.SessionMeta("cliproxyapi", "", 0),            // 1
		cx.UserMessage("q1"),                            // 2 call 1
		cx.AssistantMessage("a1"),                       // 3
		cx.TokenUsage("resp_a"),                         // 4
		cx.UserMessage("q2"),                            // 5 call 2 (covered): input
		cx.Reasoning("r2"),                              // 6 output
		cx.CommandExecution(synth.Text(4<<10, 9), true), // 7 no call: full
		cx.WorldState(synth.Text(2<<10, 10)),            // 8 no call: full
		cx.AssistantMessage("a2"),                       // 9 output
		cx.TokenUsage("resp_b"),                         // 10
		cx.UserMessage("q3"),                            // 11 call 3
		cx.AssistantMessage("a3"),                       // 12
		cx.TokenUsage("resp_c"),                         // 13
		cx.UserMessage("q4"),                            // 14 no token_usage_record yet: full
	)
	x.srv.SetCoverage("codex:"+gThread, wire.Coverage{CPASeen: true, Responses: []string{"resp_b"}})
}

// TestCodexPerResponseCoverage: with one of three responses covered, only that call's input and output
// records are metadata-only; records of no call inside it stay full with their refs. Another provider
// stays full.
func TestCodexPerResponseCoverage(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.coveredRollout()
	x.mustRun()
	meta := map[int64]bool{}
	for _, r := range x.envelopes() {
		rec := decodeRecord(t, r.Envelope)
		ln := r.Envelope.Loc.LineOrdinal
		if rec.Mode == ModeMetadata {
			meta[ln] = true
			for _, it := range rec.Items {
				if it.P != nil {
					t.Errorf("line %d: metadata item with a payload", ln)
				}
			}
			if len(r.Envelope.Refs) != 0 {
				t.Errorf("line %d: metadata record with refs", ln)
			}
		}
		if ln == 7 || ln == 8 {
			if rec.Mode != ModeFull || len(r.Envelope.Refs) == 0 || len(rec.Items) == 0 || rec.Items[0].P == nil {
				t.Errorf("line %d (%s): mode %s, %d refs; want full with its text piece", ln, rec.RecordType, rec.Mode, len(r.Envelope.Refs))
			}
		}
	}
	if len(meta) != 3 || !meta[5] || !meta[6] || !meta[9] {
		t.Errorf("metadata lines %v, want exactly 5, 6, 9", meta)
	}

	// Another provider: always full.
	y := newH(t)
	y.enroll()
	cy := &synth.Codex{G: y.g, ThreadID: gThread, Session: gThread}
	y.write(filepath.Join(y.codex(), "2026", "10", "02", "rollout-x-"+gThread+".jsonl"),
		cy.SessionMeta("openai", "", 0), cy.UserMessage("q"), cy.AssistantMessage("a"), cy.TokenUsage("resp_b"))
	y.srv.SetCoverage("codex:"+gThread, wire.Coverage{CPASeen: true, Responses: []string{"resp_b"}})
	y.mustRun()
	for _, r := range y.envelopes() {
		if decodeRecord(t, r.Envelope).Mode != ModeFull {
			t.Errorf("provider openai: line %d not full", r.Envelope.Loc.LineOrdinal)
		}
	}
}

// TestCodexObservedRecordOrder replays the order Codex writes rollouts in: a call's input items, its
// output items, item_completed records, then its token_usage_record; the tool outputs that follow are the
// next call's inputs, with token_count and task records in between. With the second and third responses
// covered, exactly those calls' input and output records are metadata-only; item_completed, token_count,
// task records and the first call stay full.
func TestCodexObservedRecordOrder(t *testing.T) {
	x := newH(t)
	x.enroll()
	cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
	x.write(filepath.Join(x.codex(), "2026", "10", "03", "rollout-2026-10-03T00-00-00-"+gThread+".jsonl"),
		cx.SessionMeta("cliproxyapi", "", 0),         // 1
		cx.TaskStarted(),                             // 2
		cx.UserMessage("q1"),                         // 3 call a: input
		cx.Reasoning("r1"),                           // 4 output
		cx.CustomToolCall("call_1", "ls"),            // 5 output
		cx.CommandExecution("listing", true),         // 6 item_completed
		cx.TokenUsage("resp_a"),                      // 7
		cx.CustomToolCallOutput("call_1", "listing"), // 8 call b (covered): input
		cx.TokenCount(),                              // 9
		cx.Reasoning("r2"),                           // 10 output
		cx.FunctionCall("call_2", "wait", `{}`),      // 11 output
		cx.CommandExecution("waited", true),          // 12 item_completed
		cx.TokenUsage("resp_b"),                      // 13
		cx.FunctionCallOutput("call_2", "done"),      // 14 call c (covered): input
		cx.TokenCount(),                              // 15
		cx.AssistantMessage("a3"),                    // 16 output
		cx.TokenUsage("resp_c"),                      // 17
		cx.TokenCount(),                              // 18
		cx.TaskComplete(),                            // 19
	)
	x.srv.SetCoverage("codex:"+gThread, wire.Coverage{CPASeen: true, Responses: []string{"resp_b", "resp_c"}})
	x.mustRun()
	if len(x.envelopes()) != 19 {
		t.Fatalf("%d envelopes, want 19", len(x.envelopes()))
	}
	meta := map[int64]bool{}
	for _, r := range x.envelopes() {
		if decodeRecord(t, r.Envelope).Mode == ModeMetadata {
			meta[r.Envelope.Loc.LineOrdinal] = true
		}
	}
	want := map[int64]bool{8: true, 10: true, 11: true, 14: true, 16: true}
	if len(meta) != len(want) {
		t.Errorf("metadata lines %v, want %v", meta, want)
	}
	for ln := range want {
		if !meta[ln] {
			t.Errorf("metadata lines %v, want %v", meta, want)
			break
		}
	}
	x.checkHW()
}

// TestCodexAmbiguousGroupingGoesFull: a token_usage_record before its call's outputs (the reverse of
// the order Codex writes) makes the shape check fail, so every record stays full. The reversed order
// can also leave a call of outputs only ([in1][usage a][out1][usage b]); a call needs an input first.
func TestCodexAmbiguousGroupingGoesFull(t *testing.T) {
	for name, lines := range map[string]func(cx *synth.Codex) []string{
		"reversed": func(cx *synth.Codex) []string {
			return []string{cx.SessionMeta("cliproxyapi", "", 0), cx.UserMessage("q1"), cx.TokenUsage("resp_a"),
				cx.AssistantMessage("a1"), cx.UserMessage("q2"), cx.AssistantMessage("a2"), cx.TokenUsage("resp_b")}
		},
		"outputs only": func(cx *synth.Codex) []string {
			return []string{cx.SessionMeta("cliproxyapi", "", 0), cx.UserMessage("q1"), cx.TokenUsage("resp_a"),
				cx.AssistantMessage("a1"), cx.TokenUsage("resp_b")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newH(t)
			x.enroll()
			cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
			x.write(filepath.Join(x.codex(), "2026", "10", "02", "rollout-y-"+gThread+".jsonl"), lines(cx)...)
			x.srv.SetCoverage("codex:"+gThread, wire.Coverage{CPASeen: true, Responses: []string{"resp_a", "resp_b"}})
			x.mustRun()
			for _, r := range x.envelopes() {
				if decodeRecord(t, r.Envelope).Mode != ModeFull {
					t.Errorf("line %d metadata despite an ambiguous grouping", r.Envelope.Loc.LineOrdinal)
				}
			}
		})
	}
}

// TestRewrittenRecordNewKey: a record rewritten in place (same uuid, new content) is a new key, never a
// conflict; the unchanged records come back duplicate after the rescan.
func TestRewrittenRecordNewKey(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	l1, l2, l3 := c.UserText("one"), c.AssistantText("msg_r", "two"), c.UserText("three")
	path := x.mainFile(gProject, gSession)
	x.write(path, l1, l2, l3)
	x.mustRun()
	first := x.envelopes()
	l2b := strings.Replace(l2, `"text":"two"`, `"text":"two, rewritten"`, 1)
	x.write(path, l1, l2b, l3)
	x.mustRun()
	all := x.envelopes()
	second := all[len(first):]
	checkNoConflict(t, all)
	if len(second) != 3 {
		t.Fatalf("rescan sent %d envelopes, want 3", len(second))
	}
	st := statuses(second)
	if st[wire.StatusDuplicate] != 2 || st[wire.StatusOK] != 1 {
		t.Errorf("statuses %v, want 2 duplicate and 1 ok", st)
	}
	if second[1].Envelope.Key == first[1].Envelope.Key {
		t.Error("the rewritten record kept its key")
	}
	if !strings.HasPrefix(second[1].Envelope.Key, strings.Join(strings.Split(first[1].Envelope.Key, "/")[:4], "/")) {
		t.Error("the rewritten record changed session or uuid")
	}
	for _, r := range second {
		if r.Envelope.Loc.ScanGen != 1 {
			t.Errorf("scan_gen %d, want 1", r.Envelope.Loc.ScanGen)
		}
	}
	x.checkHW()
}

// TestTruncatedFileRescan: a file that shrank below end_offset is rescanned from 0; later appends
// continue from the new cursor.
func TestTruncatedFileRescan(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	l1, l2, l3 := c.UserText("one"), c.UserText("two"), c.UserText("three")
	path := x.mainFile(gProject, gSession)
	x.write(path, l1, l2, l3)
	x.mustRun()
	x.write(path, l1)
	x.mustRun()
	got := x.envelopes()
	if len(got) != 4 || got[3].Status != wire.StatusDuplicate || got[3].Envelope.Loc.ScanGen != 1 || got[3].Envelope.Loc.LineOrdinal != 1 {
		t.Fatalf("after truncation: %d envelopes, last %+v %s", len(got), got[len(got)-1].Envelope.Loc, got[len(got)-1].Status)
	}
	l4 := c.UserText("four")
	if err := synth.Append(path, l4); err != nil {
		t.Fatal(err)
	}
	x.mustRun()
	got = x.envelopes()
	last := got[len(got)-1]
	if len(got) != 5 || last.Status != wire.StatusOK || last.Envelope.Loc.LineOrdinal != 2 || last.Envelope.Loc.ScanGen != 1 {
		t.Fatalf("after append: %d envelopes, last line %d gen %d %s", len(got), last.Envelope.Loc.LineOrdinal, last.Envelope.Loc.ScanGen, last.Status)
	}
	x.checkHW()
}

// TestLocatorGoldens: a re-read after a prefix truncation and a touched file with unchanged content
// come back duplicate with new locators, and mode A -> B -> A keeps three locators.
func TestLocatorGoldens(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	l1, l2, l3 := c.UserText("one"), c.UserText("two"), c.UserText("three")
	path := x.mainFile(gProject, gSession)
	x.write(path, l1, l2, l3)
	meta := filepath.Join(x.projects(), gProject, gSession, "subagents", "agent-a1.meta.json")
	x.writeRaw(meta, []byte(synth.AgentMeta(x.g)))
	mode := x.mainFile(gProject, "44444444-4444-4444-8444-444444444444")
	cm := x.claude("44444444-4444-4444-8444-444444444444")
	x.write(mode, cm.State("mode", M{"mode": "plan"}), cm.State("mode", M{"mode": "normal"}), cm.State("mode", M{"mode": "plan"}))
	x.mustRun()
	n0 := len(x.envelopes())

	// Prefix truncation: the first record is cut; the rest shift.
	x.write(path, l2, l3)
	// Touch: same bytes, new mtime.
	later := pinTime.Add(time.Hour)
	if err := os.Chtimes(meta, later, later); err != nil {
		t.Fatal(err)
	}
	x.mustRun()
	all := x.envelopes()
	checkNoConflict(t, all)
	var lines []string
	for _, r := range all {
		e := r.Envelope
		if e.Loc == nil {
			continue
		}
		lines = append(lines, strings.Join([]string{string(r.Status), e.Key[:min(len(e.Key), 40)], e.Loc.FileKey[len(e.Loc.FileKey)-24:],
			itoa(int64(e.Loc.ScanGen)), itoa(e.Loc.LineOrdinal), itoa(e.Loc.ByteOffset), itoa(e.Loc.EndOffset), itoa(e.Loc.Size)}, " "))
	}
	checkGolden(t, "locators.golden.txt", []byte(strings.Join(lines, "\n")+"\n"))

	after := all[n0:]
	if len(after) != 3 {
		t.Fatalf("second run sent %d envelopes, want 3 (two re-read records, one touched file)", len(after))
	}
	for _, r := range after {
		if r.Status != wire.StatusDuplicate {
			t.Errorf("%s: %s, want duplicate", r.Envelope.Key, r.Status)
		}
		if r.Envelope.Loc.ScanGen != 1 {
			t.Errorf("%s: scan_gen %d, want 1", r.Envelope.Key, r.Envelope.Loc.ScanGen)
		}
	}
	modes := byFile(all, "44444444-4444-4444-8444-444444444444.jsonl")
	if len(modes) != 3 || modes[0].Envelope.Key != modes[2].Envelope.Key || modes[0].Envelope.Key == modes[1].Envelope.Key {
		t.Fatalf("mode A->B->A: %d envelopes", len(modes))
	}
	if modes[2].Status != wire.StatusDuplicate {
		t.Errorf("the repeated A is %s, want duplicate", modes[2].Status)
	}
	locs := map[int64]bool{}
	for _, r := range modes {
		locs[r.Envelope.Loc.LineOrdinal] = true
	}
	if len(locs) != 3 {
		t.Errorf("mode A->B->A: %d distinct locators, want 3", len(locs))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestCrashBetweenCommitAndCursor: a crash after the commit and before the cursor transaction re-reads
// the same records into the same envelopes (duplicate, no gaps); after files grew, seqs move to other
// keys and hw still reaches the end.
func TestCrashBetweenCommitAndCursor(t *testing.T) {
	x := newH(t)
	x.enroll()
	ca, cb := x.claude(gSession), x.claude("55555555-5555-4555-8555-555555555555")
	pa, pb := x.mainFile(gProject, gSession), x.mainFile(gProject, "55555555-5555-4555-8555-555555555555")
	x.write(pa, ca.UserText("a1"), ca.UserText("a2"), ca.UserText("a3"))
	x.write(pb, cb.UserText("b1"), cb.UserText("b2"), cb.UserText("b3"))
	crash := errors.New("simulated crash")
	x.env.hooks = &testHooks{afterCommit: func() error { return crash }}
	if rc := x.run(); rc == 0 {
		t.Fatal("the crashed run exited 0")
	}
	first := x.envelopes()
	if len(first) != 6 || x.meta().NextSeq != 1 {
		t.Fatalf("after crash: %d envelopes, next_seq %d", len(first), x.meta().NextSeq)
	}
	x.env.hooks = nil
	x.mustRun()
	second := x.envelopes()[6:]
	for i, r := range second {
		if r.Status != wire.StatusDuplicate || r.Envelope.Seq != first[i].Envelope.Seq || r.Envelope.Key != first[i].Envelope.Key {
			t.Errorf("re-run %d: %s seq %d key %s", i, r.Status, r.Envelope.Seq, r.Envelope.Key)
		}
	}
	x.checkHW()

	// Crash again with grown files: seqs move to other keys.
	y := newH(t)
	y.enroll()
	ya, yb := y.claude(gSession), y.claude("55555555-5555-4555-8555-555555555555")
	y.write(pa2(y), ya.UserText("a1"), ya.UserText("a2"), ya.UserText("a3"))
	y.write(pb2(y), yb.UserText("b1"), yb.UserText("b2"), yb.UserText("b3"))
	y.env.hooks = &testHooks{afterCommit: func() error { return crash }}
	y.run()
	y.env.hooks = nil
	if err := synth.Append(pa2(y), ya.UserText("a4"), ya.UserText("a5")); err != nil {
		t.Fatal(err)
	}
	y.mustRun()
	got := y.envelopes()
	if len(got) != 6+8 {
		t.Fatalf("%d envelopes, want 14", len(got))
	}
	moved := 0
	for _, r := range got[6:] {
		for _, f := range got[:6] {
			if f.Envelope.Seq == r.Envelope.Seq && f.Envelope.Key != r.Envelope.Key {
				moved++
			}
		}
	}
	if moved == 0 {
		t.Error("no seq moved to another key")
	}
	checkNoConflict(t, got)
	y.checkHW()
	if hw := y.srv.HW(y.sourceID, y.meta().OutboxID); hw != 8 {
		t.Errorf("hw %d, want 8", hw)
	}
}

func pa2(x *h) string { return x.mainFile(gProject, gSession) }
func pb2(x *h) string { return x.mainFile(gProject, "55555555-5555-4555-8555-555555555555") }

// TestEpochChange: an epoch change starts a new outbox and rescans only files whose last committed seq
// is above the old outbox's hw.
func TestEpochChange(t *testing.T) {
	x := newH(t)
	x.enroll()
	sz, sa, sb := "66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888"
	cz, ca, cb := x.claude(sz), x.claude(sa), x.claude(sb)
	x.write(x.mainFile(gProject, sz), cz.UserText("z1"), cz.UserText("z2"))
	stuck := ca.UserText("a2")
	x.write(x.mainFile(gProject, sa), ca.UserText("a1"), stuck, ca.UserText("a3"))
	x.write(x.mainFile(gProject, sb), cb.UserText("b1"), cb.UserText("b2"))
	// a2 is answered busy every time: z (seqs 1-2) and b (6-7) are acked, a stops after a1 (seq 3),
	// so hw is 3 while b's last seq is 7.
	stuckKey := keyOfLine(t, x, x.mainFile(gProject, sa), 2)
	x.srv.RejectKey(stuckKey, wire.ReasonBusy)
	if rc := x.run(); rc == 0 {
		t.Fatal("run with a stuck seq exited 0")
	}
	m1 := x.meta()
	hw1 := x.srv.HW(x.sourceID, m1.OutboxID)
	if hw1 != 3 {
		t.Fatalf("hw after the stuck run %d, want 3", hw1)
	}
	n1 := len(x.envelopes())
	x.srv.AcceptKey(stuckKey)
	x.srv.SetEpoch(2)
	x.mustRun()
	m2 := x.meta()
	if m2.OutboxID == m1.OutboxID || m2.EpochSeen != 2 {
		t.Fatalf("outbox %s -> %s, epoch %d", m1.OutboxID, m2.OutboxID, m2.EpochSeen)
	}
	files := map[string]int{}
	for _, r := range x.envelopes()[n1:] {
		if r.Envelope.OutboxID != m2.OutboxID {
			t.Errorf("seq %d sent under the old outbox", r.Envelope.Seq)
		}
		k := r.Envelope.Loc.FileKey
		files[k[strings.LastIndex(k, "/")+1:]]++
		if strings.Contains(k, sb) && r.Envelope.Loc.ScanGen != 1 {
			t.Errorf("rescanned file b: scan_gen %d, want 1", r.Envelope.Loc.ScanGen)
		}
	}
	if files[sz+".jsonl"] != 0 {
		t.Errorf("file z (at or below hw) was re-read: %d envelopes", files[sz+".jsonl"])
	}
	if files[sb+".jsonl"] != 2 {
		t.Errorf("file b (above hw) re-read %d records, want 2", files[sb+".jsonl"])
	}
	if files[sa+".jsonl"] != 2 {
		t.Errorf("file a continued with %d records, want 2 (a2, a3)", files[sa+".jsonl"])
	}
	x.checkHW()
}

// keyOfLine builds the key of one line the way a run would, without uploading.
func keyOfLine(t *testing.T, x *h, path string, line int) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ls := strings.Split(string(data), "\n")
	f, _ := classify(RootClaudeProjects, strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(path, x.projects())), "/"))
	f.Path = path
	b := testBuilder(t)
	out, err := b.claudeRecord(f, []byte(ls[line-1]))
	if err != nil {
		t.Fatal(err)
	}
	return out.key
}

// TestDeadLetterMaskViolationRequeue: a mask_violation record is recorded in dead, its cursor moves
// past it, and requeue --remask uploads it with a new seq once the archive accepts it.
func TestDeadLetterMaskViolationRequeue(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), c.UserText("two"), c.UserText("three"))
	bad := keyOfLine(t, x, path, 2)
	x.srv.RejectKey(bad, wire.ReasonMaskViolation)
	x.mustRun()
	st := x.state()
	dead, err := st.deadRows("", "", 0)
	if err != nil || len(dead) != 1 || dead[0].Key != bad || dead[0].Reason != "mask_violation" || dead[0].State != "dead" || dead[0].LineOrdinal != 2 {
		t.Fatalf("dead rows %+v (%v)", dead, err)
	}
	curs, _ := st.cursors()
	fi, _ := os.Stat(path)
	if c := curs["claude-projects/"+gProject+"/"+gSession+".jsonl"]; c.EndOffset != fi.Size() || c.LineOrdinal != 3 {
		t.Fatalf("cursor stopped at %d/%d, want the end", c.EndOffset, c.LineOrdinal)
	}
	x.checkHW()
	oldSeq := dead[0].Seq
	x.srv.AcceptKey(bad)
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", "mask_violation", "--remask"}, x.env); rc != 0 {
		t.Fatalf("requeue rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	all := x.envelopes()
	last := all[len(all)-1]
	if last.Envelope.Key != bad || last.Status != wire.StatusOK || last.Envelope.Seq <= oldSeq {
		t.Fatalf("requeued: key match %v, %s, seq %d (old %d)", last.Envelope.Key == bad, last.Status, last.Envelope.Seq, oldSeq)
	}
	dead, _ = x.state().deadRows("", "", 0)
	if len(dead) != 1 || dead[0].State != "requeued" {
		t.Fatalf("dead row after requeue: %+v", dead)
	}
	x.checkHW()

	// A record whose bytes changed since is gone.
	y := newH(t)
	y.enroll()
	cy := y.claude(gSession)
	py := y.mainFile(gProject, gSession)
	y.write(py, cy.UserText("one"), cy.UserText("two"))
	y.srv.RejectKey(keyOfLine(t, y, py, 2), wire.ReasonSchema)
	y.mustRun()
	y.write(py, cy.UserText("one"), cy.UserText("TWO"))
	y.mustRun() // rescan: scan_gen moves on
	y.out.Reset()
	if rc := Main([]string{"requeue", "--reason", "schema"}, y.env); rc != 0 {
		t.Fatalf("requeue rc=%d", rc)
	}
	if !strings.Contains(y.out.String(), "gone=1") {
		t.Errorf("requeue output %q, want gone=1", y.out.String())
	}
}

// TestTooLargeRecordTombstone: an envelope over limits.envelope_json is dead-lettered locally and its
// seq reaches the archive as a tombstone, so hw passes it.
func TestTooLargeRecordTombstone(t *testing.T) {
	x := newH(t)
	lim := wire.DefaultLimits
	lim.EnvelopeJSON = 32 << 10
	x.srv.Close()
	x.srv = clienttest.NewServer(clienttest.Options{Limits: lim})
	t.Cleanup(x.srv.Close)
	x.enroll()
	c := x.claude(gSession)
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), c.System("local_command", synth.Text(64<<10, 3)), c.UserText("three"))
	x.mustRun()
	tombs := x.srv.Tombstones(x.sourceID)
	if len(tombs) != 1 || tombs[0].Reason != wire.TombRejectedTooLarge || tombs[0].Seq != 2 {
		t.Fatalf("tombstones %+v", tombs)
	}
	dead, _ := x.state().deadRows("rejected_too_large", "dead", 0)
	if len(dead) != 1 {
		t.Fatalf("dead rows %+v", dead)
	}
	x.checkHW()
	if n, _ := x.state().count(`SELECT count(*) FROM tombstones WHERE state='pending'`); n != 0 {
		t.Errorf("%d tombstones still pending", n)
	}

	// requeue --split moves the large frame string into a piece, so it fits.
	if rc := Main([]string{"requeue", "--reason", "rejected_too_large", "--split"}, x.env); rc != 0 {
		t.Fatalf("requeue --split rc=%d: %s", rc, x.errb.String())
	}
	all := x.envelopes()
	last := all[len(all)-1]
	if last.Status != wire.StatusOK || len(last.Envelope.Refs) == 0 {
		t.Fatalf("split requeue: %s with %d refs", last.Status, len(last.Envelope.Refs))
	}
	x.checkHW()
}

// tooLargeServer replaces x's fake archive with one using lim, before enrollment.
func (x *h) tooLargeServer(lim wire.Limits) {
	x.t.Helper()
	x.srv.Close()
	x.srv = clienttest.NewServer(clienttest.Options{Limits: lim})
	x.t.Cleanup(x.srv.Close)
}

// TestStaleTombstoneAfterPartialRun: a run that stops before every seq has a receipt moves next_seq back
// to the first such seq, and drops the pending tombstones at or above it. The
// next run gives such a seq to a new envelope, and that envelope's own result decides: a rejected record
// gets its dead row, and a retried one is never taken as acknowledged. Run 2 is a full run (files A =
// [a1], B = [too large, b2]) or `run --session B` (A = [a1, too large, a3], B = [b1, b2, b3]); in both,
// the too-large record had seq 2 in run 1 and b2 gets seq 2 in run 2.
func TestStaleTombstoneAfterPartialRun(t *testing.T) {
	sb := "99999999-9999-4999-8999-999999999999"
	for _, tc := range []struct {
		name    string
		session bool
		reason  wire.Reason
	}{
		{"full run, rejected", false, wire.ReasonMaskViolation},
		{"full run, retry", false, wire.ReasonBusy},
		{"session run, rejected", true, wire.ReasonMaskViolation},
		{"session run, retry", true, wire.ReasonBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newH(t)
			lim := wire.DefaultLimits
			lim.EnvelopeJSON = 32 << 10
			x.tooLargeServer(lim)
			x.enroll()
			ca, cb := x.claude(gSession), x.claude(sb)
			pa, pb := x.mainFile(gProject, gSession), x.mainFile(gProject, sb)
			var args []string
			if tc.session {
				x.write(pa, ca.UserText("a1"), ca.System("local_command", synth.Text(64<<10, 3)), ca.UserText("a3"))
				x.write(pb, cb.UserText("b1"), cb.UserText("b2"), cb.UserText("b3"))
				args = []string{"--session", sb}
			} else {
				x.write(pa, ca.UserText("a1"))
				x.write(pb, cb.System("local_command", synth.Text(64<<10, 3)), cb.UserText("b2"))
			}
			for i := 0; i <= maxTransient; i++ {
				x.srv.FailNext("POST "+wire.PathCommit, http.StatusServiceUnavailable, 0)
			}
			if rc := x.run(); rc == 0 {
				t.Fatal("run 1 with an unavailable archive exited 0")
			}
			if m := x.meta(); m.NextSeq != 1 {
				t.Fatalf("next_seq %d after run 1, want 1", m.NextSeq)
			}
			if n, _ := x.state().count(`SELECT count(*) FROM tombstones WHERE state='pending'`); n != 0 {
				t.Fatalf("%d pending tombstones at or above the rewound next_seq", n)
			}
			if dead, _ := x.state().deadRows("rejected_too_large", "dead", 0); len(dead) != 1 {
				t.Fatalf("too-large dead rows %+v, want 1 (kept for requeue)", dead)
			}

			b2 := keyOfLine(t, x, pb, 2)
			x.srv.RejectKey(b2, tc.reason)
			rc := x.run(args...)
			var arrivals []clienttest.Received
			for _, r := range x.envelopes() {
				if r.Envelope.Key == b2 {
					arrivals = append(arrivals, r)
				}
			}
			if len(arrivals) == 0 || arrivals[0].Envelope.Seq != 2 {
				t.Fatalf("b2 arrivals %d, want seq 2 first", len(arrivals))
			}
			if tombs := x.srv.Tombstones(x.sourceID); len(tombs) != 0 {
				t.Errorf("run 2 sent %d tombstones, want none (the stale seq-2 tombstone was dropped)", len(tombs))
			}
			bKey := "claude-projects/" + gProject + "/" + sb + ".jsonl"
			if tc.reason == wire.ReasonMaskViolation {
				if rc != 0 {
					t.Fatalf("run 2 rc %d", rc)
				}
				dead, _ := x.state().deadRows("mask_violation", "dead", 0)
				if len(dead) != 1 || dead[0].Key != b2 {
					t.Fatalf("mask_violation dead rows %+v, want b2's", dead)
				}
			} else {
				if rc == 0 {
					t.Fatal("run 2 with b2 answered busy exited 0")
				}
				for _, r := range arrivals {
					if r.Status != wire.StatusRetry {
						t.Errorf("b2 seq %d answered %s", r.Envelope.Seq, r.Status)
					}
				}
				curs, _ := x.state().cursors()
				if c := curs[bKey]; c.LineOrdinal != 1 {
					t.Fatalf("file B's cursor at line %d, want 1 (before b2)", c.LineOrdinal)
				}
				x.srv.AcceptKey(b2)
				x.mustRun(args...) // b2 again from the cursor (and b3, already stored: duplicate)
				found := false
				for _, r := range x.envelopes() {
					found = found || r.Envelope.Key == b2 && r.Status == wire.StatusOK
				}
				if !found {
					t.Fatal("b2 never stored")
				}
			}
			x.mustRun() // the rest (session A in the --session case)
			x.checkHW()
		})
	}
}

// TestBatchHeldBytes: pieces sent by PUT (over limits.piece_inline) count toward the bytes a batch holds,
// so a sweep over many large saved outputs flushes each time the batch holds maxHeldBytes.
func TestBatchHeldBytes(t *testing.T) {
	old := maxHeldBytes
	maxHeldBytes = 256 << 10
	t.Cleanup(func() { maxHeldBytes = old })
	x := newH(t)
	lim := wire.DefaultLimits
	lim.PieceInline = 32 << 10
	x.tooLargeServer(lim)
	x.enroll()
	for i := 0; i < 6; i++ {
		x.writeRaw(filepath.Join(x.projects(), gProject, gSession, "tool-results", "out"+itoa(int64(i))+".txt"), []byte(synth.Text(100<<10, uint64(20+i))))
	}
	flushes := 0
	x.env.hooks = &testHooks{afterCommit: func() error { flushes++; return nil }}
	x.mustRun()
	if flushes != 2 {
		t.Errorf("%d flushes for six 100 KiB pieces with a 256 KiB bound, want 2", flushes)
	}
	if got := statuses(x.envelopes()); got[wire.StatusOK] != 6 {
		t.Errorf("statuses %v, want 6 ok", got)
	}
	x.checkHW()
}

// TestOversizeWholeFileNotRead: a whole file larger than limits.text_piece and limits.attachment is a local
// rejected_too_large dead letter keyed by the sha256 of its bytes, built without masking or holding it;
// requeue rebuilds the same key, and the row stays dead.
func TestOversizeWholeFileNotRead(t *testing.T) {
	x := newH(t)
	lim := wire.DefaultLimits
	lim.TextPiece, lim.Attachment = 64<<10, 64<<10
	x.tooLargeServer(lim)
	x.enroll()
	data := []byte(synth.Text(100<<10, 5))
	x.writeRaw(filepath.Join(x.projects(), gProject, gSession, "tool-results", "big.txt"), data)
	x.mustRun()
	if n := len(x.envelopes()); n != 0 {
		t.Fatalf("%d envelopes sent for an oversize file", n)
	}
	want := wire.ImportFileKey(ClientClaudeCode, "claude-projects/"+gProject+"/"+gSession+"/tool-results/big.txt", sha256.Sum256(data))
	dead, _ := x.state().deadRows("rejected_too_large", "dead", 0)
	if len(dead) != 1 || dead[0].Key != want || dead[0].ByteLen != int64(len(data)) {
		t.Fatalf("dead rows %+v, want one keyed by the file's sha256", dead)
	}
	if tombs := x.srv.Tombstones(x.sourceID); len(tombs) != 1 || tombs[0].Reason != wire.TombRejectedTooLarge {
		t.Fatalf("tombstones %+v", tombs)
	}
	if hbs := x.srv.Heartbeats(x.sourceID); len(hbs) != 1 || hbs[0].Masker.Bytes != 0 {
		t.Fatalf("the oversize file was masked (%d heartbeats)", len(hbs))
	}
	x.checkHW()
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", "rejected_too_large"}, x.env); rc != 0 || !strings.Contains(x.out.String(), "found=1 gone=0") {
		t.Fatalf("requeue rc=%d: %s", rc, x.out.String())
	}
	if dead, _ := x.state().deadRows("rejected_too_large", "dead", 0); len(dead) != 1 || dead[0].Key != want {
		t.Fatalf("after requeue: %+v", dead)
	}
	x.checkHW()
}

// atLimitLimits scales the production limits down: text_piece is the larger piece limit, so a whole file
// of exactly text_piece bytes is read and masked, and its piece goes by PUT.
func atLimitLimits() wire.Limits {
	lim := wire.DefaultLimits
	lim.TextPiece, lim.Attachment, lim.PieceInline = 64<<10, 50<<10, 32<<10
	return lim
}

const atLimitFileKey = "claude-projects/" + gProject + "/" + gSession + "/tool-results/out.txt"

// TestMaskGrowthOverTextPieceRequeued: a saved output of exactly limits.text_piece bytes holds short
// credentials whose placeholders are longer than they are, so its masked piece is over the limit. It
// becomes a rejected_too_large dead letter keyed by the masked piece, never sent. A dry-run requeue only
// counts it, and a real requeue at the same limit dead-letters it again under the same key. Once the
// archive raises text_piece, a plain requeue uploads it.
func TestMaskGrowthOverTextPieceRequeued(t *testing.T) {
	x := newH(t)
	lim := atLimitLimits()
	x.tooLargeServer(lim)
	x.enroll()
	var b strings.Builder
	for i := range 8 {
		b.WriteString("GET https://api.example.invalid/v1/items?page=" + strconv.Itoa(i) + "&token=" + x.g.Alnum(10) + " 200\n")
	}
	b.WriteString(synth.Text(int(lim.TextPiece), 7))
	data := []byte(b.String()[:lim.TextPiece])
	x.writeRaw(filepath.Join(x.projects(), gProject, gSession, "tool-results", "out.txt"), data)
	p, err := testBuilder(t).textPiece(string(data))
	if err != nil || int64(len(p.Bytes)) <= lim.TextPiece {
		t.Fatalf("masked to %d bytes (%v), want over text_piece %d", len(p.Bytes), err, lim.TextPiece)
	}
	want := wire.ImportFileKey(ClientClaudeCode, atLimitFileKey, p.Hash)

	x.mustRun()
	dead, _ := x.state().deadRows("rejected_too_large", "dead", 0)
	if len(dead) != 1 || dead[0].Key != want || dead[0].ByteLen != int64(len(data)) {
		t.Fatalf("dead rows %+v, want one keyed by the masked piece", dead)
	}
	stillDead := func(stage string, tombs int) {
		t.Helper()
		if n := len(x.envelopes()); n != 0 {
			t.Fatalf("%s: %d envelopes sent", stage, n)
		}
		if ts := x.srv.Tombstones(x.sourceID); len(ts) != tombs || ts[len(ts)-1].Reason != wire.TombRejectedTooLarge {
			t.Fatalf("%s: tombstones %+v, want %d", stage, ts, tombs)
		}
		if dead, _ := x.state().deadRows("rejected_too_large", "", 0); len(dead) != 1 || dead[0].Key != want || dead[0].State != "dead" {
			t.Fatalf("%s: dead rows %+v", stage, dead)
		}
		x.checkHW()
	}
	stillDead("run", 1)
	requeue := func(args ...string) {
		t.Helper()
		x.out.Reset()
		if rc := Main(append([]string{"requeue", "--reason", "rejected_too_large"}, args...), x.env); rc != 0 || !strings.Contains(x.out.String(), "found=1 gone=0") {
			t.Fatalf("requeue %v rc=%d: %s", args, rc, x.out.String())
		}
	}
	// A dry run counts rows only: it cannot show whether the archive's limit is raised yet.
	requeue("--dry-run")
	stillDead("dry-run requeue", 1)
	// A real requeue at the same limit sends only its new seq's tombstone.
	requeue()
	stillDead("requeue at the old limit", 2)

	lim.TextPiece = 72 << 10
	x.srv.SetLimits(lim)
	requeue()
	all := x.envelopes()
	if len(all) != 1 || all[0].Envelope.Kind != wire.KindImportFile || all[0].Envelope.Key != want || all[0].Status != wire.StatusOK {
		t.Fatalf("after the raise: %d envelopes, want one import.file ok under the dead key", len(all))
	}
	if kind, raw, ok := x.srv.Piece(p.Hash); !ok || kind != wire.PieceText || !bytes.Equal(raw, p.Bytes) {
		t.Fatalf("stored piece %v %d bytes %v, want the masked piece", kind, len(raw), ok)
	}
	if dead, _ := x.state().deadRows("rejected_too_large", "", 0); len(dead) != 1 || dead[0].State != "requeued" {
		t.Fatalf("dead rows after the raise %+v", dead)
	}
	x.checkHW()
}

// TestWholeFileAtTextPieceSent: a saved output of exactly limits.text_piece bytes that masking leaves
// unchanged is read whole and sent as one piece. The limit is inclusive.
func TestWholeFileAtTextPieceSent(t *testing.T) {
	x := newH(t)
	lim := atLimitLimits()
	x.tooLargeServer(lim)
	x.enroll()
	data := []byte(synth.Text(int(lim.TextPiece), 9)[:lim.TextPiece])
	p, err := testBuilder(t).textPiece(string(data))
	if err != nil || !bytes.Equal(p.Bytes, data) {
		t.Fatalf("masking changed the file (%v)", err)
	}
	x.writeRaw(filepath.Join(x.projects(), gProject, gSession, "tool-results", "out.txt"), data)
	x.mustRun()
	all := x.envelopes()
	if len(all) != 1 || all[0].Envelope.Key != wire.ImportFileKey(ClientClaudeCode, atLimitFileKey, p.Hash) || all[0].Status != wire.StatusOK {
		t.Fatalf("%d envelopes, want one import.file ok", len(all))
	}
	if _, raw, ok := x.srv.Piece(p.Hash); !ok || len(raw) != len(data) {
		t.Fatalf("stored piece %d bytes %v, want %d", len(raw), ok, len(data))
	}
	if dead, _ := x.state().deadRows("", "", 0); len(dead) != 0 {
		t.Fatalf("dead rows %+v", dead)
	}
	x.checkHW()
}

// TestSymlinkedRoot: a root that is a symlink (projects on another disk) is walked, with keys relative to
// the root.
func TestSymlinkedRoot(t *testing.T) {
	x := newH(t)
	x.enroll()
	real := filepath.Join(x.home, "disk2", "projects")
	c := x.claude(gSession)
	x.write(filepath.Join(real, gProject, gSession+".jsonl"), c.UserText("one"))
	if err := os.MkdirAll(filepath.Dir(x.projects()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, x.projects()); err != nil {
		t.Fatal(err)
	}
	x.mustRun()
	got := x.envelopes()
	if len(got) != 1 || got[0].Envelope.Loc.FileKey != "claude-projects/"+gProject+"/"+gSession+".jsonl" || got[0].Status != wire.StatusOK {
		t.Fatalf("%d envelopes through a symlinked root", len(got))
	}
}

// TestReenrollOtherArchive: enrolling against another archive resets every cursor, so it gets the whole
// history; re-enrolling against the same archive keeps the cursors.
func TestReenrollOtherArchive(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	x.write(x.mainFile(gProject, gSession), c.UserText("one"), c.UserText("two"))
	x.writeRaw(filepath.Join(x.projects(), gProject, gSession, "tool-results", "out.txt"), []byte("saved output "+x.g.Words(8)+"\n"))
	x.mustRun()
	if n := len(x.envelopes()); n != 3 {
		t.Fatalf("first archive got %d envelopes, want 3", n)
	}
	x.enroll() // the same archive: a new source, cursors kept
	x.mustRun()
	if n := len(x.envelopes()); n != 0 {
		t.Fatalf("re-enrolling against the same archive re-sent %d envelopes", n)
	}
	other := clienttest.NewServer(clienttest.Options{ArchiveID: "00000000-0000-4000-8000-00000000b002"})
	t.Cleanup(other.Close)
	x.srv = other
	x.enroll()
	if !strings.Contains(x.out.String(), "new archive: 2 files to read again") {
		t.Errorf("enroll output %q", x.out.String())
	}
	x.mustRun()
	if got := statuses(x.envelopes()); got[wire.StatusOK] != 3 {
		t.Fatalf("the other archive got %v, want 3 ok", got)
	}
	x.checkHW()
}

// TestMissingRefThreeTimes: three consecutive missing_ref answers dead-letter the seq locally as
// missing_ref and send a tombstone.
func TestMissingRefThreeTimes(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), c.UserText("two"))
	x.srv.RejectKey(keyOfLine(t, x, path, 1), wire.ReasonMissingRef)
	x.mustRun()
	tombs := x.srv.Tombstones(x.sourceID)
	if len(tombs) != 1 || tombs[0].Reason != wire.TombMissingRef || tombs[0].Seq != 1 {
		t.Fatalf("tombstones %+v", tombs)
	}
	x.checkHW()
}

// TestAuthFailureBacksOff: a 401 stops uploads for 15 minutes and is never retried in a loop.
func TestAuthFailureBacksOff(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	x.write(x.mainFile(gProject, gSession), c.UserText("one"))
	before := len(x.srv.RequestBodies())
	x.srv.FailNext("GET "+wire.PathSourcesSelf, http.StatusUnauthorized, 0)
	if rc := x.run(); rc != 1 {
		t.Fatalf("rc %d, want 1", rc)
	}
	if n := len(x.srv.RequestBodies()) - before; n != 1 {
		t.Fatalf("%d requests after a 401, want exactly 1", n)
	}
	if m := x.meta(); m.AuthBackoffUntil != x.now.Add(authBackoff).Unix() || !strings.Contains(m.Faults, "auth_failed") {
		t.Fatalf("meta %+v", m)
	}
	if rc := x.run(); rc != 1 || len(x.srv.RequestBodies())-before != 1 {
		t.Fatal("a run inside the backoff contacted the archive")
	}
	x.now = x.now.Add(16 * time.Minute)
	x.mustRun()
	if len(x.envelopes()) != 1 {
		t.Fatal("no upload after the backoff")
	}
}

// TestRunSessionWhileBackfillHoldsLock: run --session exits 0 at once without touching state or the
// archive while another process holds the lock.
func TestRunSessionWhileBackfillHoldsLock(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	x.write(x.mainFile(gProject, gSession), c.UserText("one"))
	p := pathsFor(x.home)
	sum := func() [32]byte {
		var all []byte
		for _, f := range []string{p.StateDB, p.StateDB + "-wal", p.StateDB + "-shm"} {
			b, _ := os.ReadFile(f)
			all = append(all, b...)
		}
		return sha256.Sum256(all)
	}
	before, reqs := sum(), len(x.srv.RequestBodies())
	release, busy, err := acquireLock(p.LockFile, false)
	if err != nil || busy {
		t.Fatal(err, busy)
	}
	defer release()
	if rc := x.run("--session", gSession); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if sum() != before {
		t.Error("state changed")
	}
	if len(x.srv.RequestBodies()) != reqs {
		t.Error("the archive was contacted")
	}
	if !strings.Contains(x.out.String(), "holds the lock") {
		t.Errorf("output %q", x.out.String())
	}
}

// TestConcurrentRunsNeverInterleave runs two importer processes against one home: the second exits 0
// at once while the first holds the lock, and the archive sees one contiguous run.
func TestConcurrentRunsNeverInterleave(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, c.UserText("line "+itoa(int64(i))))
	}
	x.write(x.mainFile(gProject, gSession), lines...)
	release := x.srv.Hold("POST " + wire.PathCommit)
	cmd := func() *exec.Cmd {
		cm := exec.Command(os.Args[0], "-test.run=^$", "--", "run")
		cm.Env = append(os.Environ(), "CA_IMPORT_HELPER=1", "CA_IMPORT_HOME="+x.home, "HOME="+x.home)
		return cm
	}
	base := len(x.srv.RequestBodies())
	first := cmd()
	var out1 strings.Builder
	first.Stdout = &out1
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	// The first run has sent sources/self and need and now waits on the held commit, with the lock.
	// (Polling the lock itself could take it for a moment and make the first run exit as busy.)
	deadline := time.Now().Add(60 * time.Second)
	for len(x.srv.RequestBodies()) < base+2 {
		if time.Now().After(deadline) {
			t.Fatal("the first run never reached its commit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	p := pathsFor(x.home)
	if _, busy, err := acquireLock(p.LockFile, false); err != nil || !busy {
		t.Fatalf("the lock is not held by the first run (busy=%v, %v)", busy, err)
	}
	reqs := len(x.srv.RequestBodies())
	second := cmd()
	out2, err := second.Output()
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(string(out2), "holds the lock") {
		t.Errorf("second run output %q", out2)
	}
	if len(x.srv.RequestBodies()) != reqs {
		t.Error("the second run contacted the archive")
	}
	release()
	if err := first.Wait(); err != nil {
		t.Fatalf("first run: %v (%s)", err, out1.String())
	}
	got := x.envelopes()
	if len(got) != 20 {
		t.Fatalf("%d envelopes, want 20", len(got))
	}
	for i, r := range got {
		if r.Envelope.Seq != uint64(i+1) || r.Status != wire.StatusOK {
			t.Errorf("envelope %d: seq %d %s", i, r.Envelope.Seq, r.Status)
		}
	}
	x.checkHW()
}

func TestDryRunTouchesNothing(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	x.mustRun("--dry-run")
	if len(x.envelopes()) != 0 {
		t.Fatal("dry run uploaded")
	}
	curs, _ := x.state().cursors()
	if len(curs) != 0 {
		t.Fatalf("dry run wrote %d cursors", len(curs))
	}
	if !strings.Contains(x.out.String(), "envelopes=") {
		t.Errorf("output %q", x.out.String())
	}
}

func TestSessionFilter(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	x.mustRun("--session", gSession)
	for _, r := range x.envelopes() {
		if !strings.Contains(r.Envelope.Loc.FileKey, gSession) {
			t.Errorf("--session uploaded %s", r.Envelope.Loc.FileKey)
		}
	}
	if len(x.envelopes()) == 0 {
		t.Fatal("nothing uploaded")
	}
}

// TestMaxBytesContinues: a run cut off by --max-bytes continues where it stopped.
func TestMaxBytesContinues(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, c.UserText(x.g.Words(40)))
	}
	x.write(x.mainFile(gProject, gSession), lines...)
	x.mustRun("--max-bytes", "1000")
	n := len(x.envelopes())
	if n == 0 || n >= 10 {
		t.Fatalf("first run sent %d", n)
	}
	for i := 0; i < 10 && len(x.envelopes()) < 10; i++ {
		x.mustRun("--max-bytes", "1000")
	}
	if got := x.envelopes(); len(got) != 10 {
		t.Fatalf("%d envelopes after the runs", len(got))
	}
	checkNoConflict(t, x.envelopes())
	for _, r := range x.envelopes() {
		if r.Status != wire.StatusOK {
			t.Errorf("seq %d %s", r.Envelope.Seq, r.Status)
		}
	}
	x.checkHW()

	// Whole files count too: once a run has read the limit, the remaining whole files wait.
	y := newH(t)
	y.enroll()
	for i := 0; i < 6; i++ {
		y.writeRaw(filepath.Join(y.projects(), gProject, gSession, "tool-results", "out"+itoa(int64(i))+".txt"), []byte(synth.Text(2<<10, uint64(30+i))))
	}
	y.mustRun("--max-bytes", "1000")
	if n := len(y.envelopes()); n != 1 {
		t.Fatalf("first run read %d whole files past --max-bytes, want 1", n)
	}
	if !strings.Contains(y.out.String(), "stopped at --max-bytes") {
		t.Errorf("output %q", y.out.String())
	}
	for i := 0; i < 10 && len(y.envelopes()) < 6; i++ {
		y.mustRun("--max-bytes", "1000")
	}
	if got := statuses(y.envelopes()); got[wire.StatusOK] != 6 {
		t.Fatalf("statuses %v after the runs, want 6 ok", got)
	}
	y.checkHW()
}

// TestHeartbeatAfterRun: every run ends with an importer heartbeat.
func TestHeartbeatAfterRun(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	x.mustRun()
	hbs := x.srv.Heartbeats(x.sourceID)
	if len(hbs) != 1 {
		t.Fatalf("%d heartbeats", len(hbs))
	}
	hb := hbs[0]
	if hb.Component != "importer" || hb.State != "ok" || hb.Importer == nil || hb.Importer.FilesTracked == 0 || hb.Outbox.HW != hb.Outbox.NextSeq-1 {
		t.Fatalf("heartbeat %+v", hb)
	}
	if hb.MaskVersion != "mask/v1" || hb.RulesetSHA == "" {
		t.Errorf("mask fields %q %q", hb.MaskVersion, hb.RulesetSHA)
	}
	// A second run with nothing new still heartbeats.
	x.mustRun()
	if len(x.srv.Heartbeats(x.sourceID)) != 2 {
		t.Error("no heartbeat for an idle run")
	}
}

// TestHeartbeatsDuringLongRun: a run that lasts longer than the heartbeat interval (a first backfill
// takes hours) heartbeats on the way with state partial, at most every 5 minutes, so the archive's
// 60-minute importer dead-man stays quiet; its last heartbeat still reports the outcome.
func TestHeartbeatsDuringLongRun(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	x.env.Now = func() time.Time { // every clock read moves a minute: the sweep takes simulated hours
		x.now = x.now.Add(time.Minute)
		return x.now
	}
	start := x.now
	x.mustRun()
	hbs := x.srv.Heartbeats(x.sourceID)
	if len(hbs) < 3 {
		t.Fatalf("%d heartbeats over a %s run, want several", len(hbs), x.now.Sub(start))
	}
	for i, hb := range hbs {
		last := i == len(hbs)-1
		if hb.BootID != hbs[0].BootID || hb.N != uint64(i+1) || hb.Component != "importer" || hb.Importer == nil {
			t.Errorf("heartbeat %d: boot %s n %d component %s importer %v", i, hb.BootID, hb.N, hb.Component, hb.Importer != nil)
		}
		if want := map[bool]string{false: "partial", true: "ok"}[last]; hb.State != want {
			t.Errorf("heartbeat %d: state %s, want %s", i, hb.State, want)
		}
		if i > 0 && !last && hb.SentAt.Sub(hbs[i-1].SentAt) < 5*time.Minute {
			t.Errorf("heartbeat %d came %s after the previous one", i, hb.SentAt.Sub(hbs[i-1].SentAt))
		}
	}
	// With a still clock (a short run) there is only the last heartbeat.
	x.env.Now = func() time.Time { return x.now }
	x.write(x.mainFile(gProject, "44444444-4444-4444-8444-444444444444"), (&synth.Claude{G: x.g, SessionID: "44444444-4444-4444-8444-444444444444"}).UserText("one more"))
	x.mustRun()
	if got := len(x.srv.Heartbeats(x.sourceID)) - len(hbs); got != 1 {
		t.Errorf("short run sent %d heartbeats, want 1", got)
	}
}
