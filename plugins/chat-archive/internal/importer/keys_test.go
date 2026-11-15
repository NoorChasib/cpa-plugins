package importer

import (
	"bytes"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// joinedKeys returns two frame keys that are clean alone and form a ca-aws-secret match once joined
// into a pointer, "/<k1>/<k2>" (the rule's 40-character class holds "/").
func joinedKeys(t *testing.T, b *builder) (k1, k2 string) {
	t.Helper()
	k1, k2 = "secret_key="+strings.Repeat("Ab3D", 5), strings.Repeat("eF6g", 5)[:19]
	if b.changes(k1) || b.changes(k2) || !b.changes("/"+k1+"/"+k2) {
		t.Fatal("the synthetic keys must be clean alone and detected once joined")
	}
	return k1, k2
}

// TestKeySegmentsHashed: an id segment the masker would change (a session, uuid, type, thread, run or
// agent id that looks like a secret) leaves the machine hashed in the record's key, deterministically and
// distinctly, and the key importer 0.1.1 gave the same bytes is kept as an alias for requeue. Keys of
// records whose ids are real ids are byte-identical to that legacy key. Clean segments that complete a
// secret across a "/" hash every id segment.
func TestKeySegmentsHashed(t *testing.T) {
	b := testBuilder(t)
	const sid = "11111111-2222-4333-8444-555555555555"
	main := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sid+".jsonl")
	clean := buildFor(t, b, main, jsonLine(t, map[string]any{"type": "user", "uuid": "u-1", "sessionId": sid}))
	sha := clean.key[strings.LastIndex(clean.key, "/")+1:]
	if clean.key != wire.ClaudeUUIDKey(sid, "u-1", sha) || len(clean.aliases) != 0 {
		t.Fatalf("clean record: key %q aliases %q, want the 0.1.1 key", clean.key, clean.aliases)
	}
	seen := map[string]string{}
	for name, sec := range recheckSecrets {
		bl := buildFor(t, b, main, jsonLine(t, map[string]any{"type": "user", "uuid": "u-1", "sessionId": sec}))
		sha := bl.key[strings.LastIndex(bl.key, "/")+1:]
		if want := wire.ClaudeUUIDKey(hashSeg(sec), "u-1", sha); bl.key != want {
			t.Errorf("%s: key %q, want %q", name, bl.key, want)
		}
		legacy := wire.ClaudeUUIDKey(keyPart(sec), "u-1", sha)
		if !bl.keyedAs(legacy) || b.changes(bl.key) || !wire.ValidKey(bl.key) {
			t.Errorf("%s: key %q aliases %q: legacy key not kept, or the key is not clean", name, bl.key, bl.aliases)
		}
		if again := buildFor(t, b, main, jsonLine(t, map[string]any{"type": "user", "uuid": "u-1", "sessionId": sec})); again.key != bl.key {
			t.Errorf("%s: two builds keyed %q and %q", name, bl.key, again.key)
		}
		if other, dup := seen[bl.key]; dup {
			t.Errorf("%s and %s share key %q", name, other, bl.key)
		}
		seen[bl.key] = name
	}
	sec := recheckSecrets["gitlab"]
	codex := mustClassify(t, RootCodexSessions, "2026/10/01/rollout-x.jsonl")
	cx, err := b.codexRecord(codex, &codexFile{threadID: sec, sessionID: sec, histStart: -1}, jsonLine(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "x"}}), 3)
	if err != nil || !strings.HasPrefix(cx.key, "cx/"+hashSeg(sec)+"/2/") || len(cx.aliases) != 1 || !strings.HasPrefix(cx.aliases[0], "cx/"+sec+"/2/") {
		t.Fatalf("codex key %q aliases %q (%v)", cx.key, cx.aliases, err)
	}
	wf := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sid+"/subagents/workflows/wf_"+sec+"/journal.jsonl")
	j := buildFor(t, b, wf, jsonLine(t, map[string]any{"type": "result", "agentId": sec}))
	if !strings.HasPrefix(j.key, "cc/j/"+hashSeg("wf_"+sec)+"/"+hashSeg(sec)+"/result/") {
		t.Fatalf("journal key %q", j.key)
	}
	// Clean alone, a secret joined: "cc/n/secret_key:/<39 characters>-x/<sha>" matches ca-aws-secret.
	typ := strings.Repeat("Ab3D", 10)[:39] + "-x"
	if b.changes("secret_key:") || b.changes(typ) {
		t.Fatal("fixture: the segments must be clean alone")
	}
	cross := buildFor(t, b, main, jsonLine(t, map[string]any{"type": typ, "sessionId": "secret_key:"}))
	sha = cross.key[strings.LastIndex(cross.key, "/")+1:]
	if !b.changes(wire.ClaudeNoUUIDKey("secret_key:", typ, sha)) {
		t.Fatal("fixture: the joined key must be detected")
	}
	if want := wire.ClaudeNoUUIDKey(hashSeg("secret_key:"), hashSeg(typ), sha); cross.key != want || !cross.keyedAs(wire.ClaudeNoUUIDKey("secret_key:", typ, sha)) {
		t.Fatalf("joined key %q, want %q with the raw form as alias", cross.key, want)
	}
}

// TestOutPathAndCleanKey: a file key leaves the machine with each segment the masker would change hashed,
// its shape and other segments kept; a clean path, and a key built by this importer, are unchanged.
// cleanKey turns a tombstone key queued by importer 0.1.1 into a clean one.
func TestOutPathAndCleanKey(t *testing.T) {
	b := testBuilder(t)
	sec := recheckSecrets["hf"]
	for in, want := range map[string]string{
		"claude-projects/-home-synth-proj/s.jsonl":                                                         "claude-projects/-home-synth-proj/s.jsonl",
		"claude-projects/-home-synth-" + sec + "/s/tool-results/a.txt":                                     "claude-projects/" + hashSeg("-home-synth-"+sec) + "/s/tool-results/a.txt",
		"claude-skills/" + sec + "/SKILL.md":                                                               "claude-skills/" + hashSeg(sec) + "/SKILL.md",
		"codex-sessions/2026/10/01/rollout-" + sec + ".jsonl":                                              "codex-sessions/2026/10/01/" + hashSeg("rollout-"+sec+".jsonl"),
		"claude-projects/p/secret_key=" + strings.Repeat("Ab3D", 5) + "/" + strings.Repeat("eF6g", 5)[:19]: "claude-projects/" + hashSeg("p") + "/" + hashSeg("secret_key="+strings.Repeat("Ab3D", 5)) + "/" + hashSeg(strings.Repeat("eF6g", 5)[:19]),
	} {
		got := b.outPath(in)
		if got != want || b.changes(got) {
			t.Errorf("outPath(%q) = %q, want %q", strings.ReplaceAll(in, sec, "<secret>"), got, want)
		}
	}
	if a, c := b.outPath("claude-projects/-p-"+recheckSecrets["xai"]+"/s.jsonl"), b.outPath("claude-projects/-p-"+recheckSecrets["sk"]+"/s.jsonl"); a == c {
		t.Error("two secret-shaped directories share one file key")
	}
	k := "cc/u/" + sec + "/u-1/" + strings.Repeat("0", 64)
	if got := b.cleanKey(k); got != "cc/u/"+hashSeg(sec)+"/u-1/"+strings.Repeat("0", 64) || !wire.ValidKey(got) {
		t.Errorf("cleanKey = %q", got)
	}
	if k := "cc/u/" + gSession + "/u-1/" + strings.Repeat("0", 64); b.cleanKey(k) != k {
		t.Error("cleanKey changed a clean key")
	}
}

// TestRequeueLegacyKeyedDeadRow: a dead row importer 0.1.1 wrote under the raw form of a secret-shaped
// id is taken back by a plain requeue: the rebuilt record is keyed with the id hashed, its alias proves
// the bytes are unchanged, it is sent under the new key, and the old row becomes requeued.
func TestRequeueLegacyKeyedDeadRow(t *testing.T) {
	x := newH(t)
	x.enroll()
	sec := recheckSecrets["xai"]
	path := x.mainFile(gProject, gSession)
	line := string(jsonLine(t, map[string]any{"type": "user", "uuid": "u-legacy-1", "sessionId": sec, "timestamp": "2026-10-01T09:00:00Z",
		"message": map[string]any{"role": "user", "content": "hello " + x.g.Words(4)}}))
	x.write(path, line)
	x.mustRun()
	all := x.envelopes()
	if len(all) != 1 {
		t.Fatalf("%d envelopes", len(all))
	}
	key := all[0].Envelope.Key
	sha := key[strings.LastIndex(key, "/")+1:]
	if key != wire.ClaudeUUIDKey(hashSeg(sec), "u-legacy-1", sha) || strings.Contains(string(all[0].Raw), sec) {
		t.Fatalf("key %q: the session id is not hashed, or the envelope holds it", key)
	}
	legacy := wire.ClaudeUUIDKey(sec, "u-legacy-1", sha)
	fileKey := "claude-projects/" + gProject + "/" + gSession + ".jsonl"
	st := x.state()
	curs, err := st.cursors()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.tx(func(tx *sql.Tx) error {
		return putDead(tx, Dead{Key: legacy, Kind: string(wire.KindImportRecord), FileKey: fileKey, ScanGen: curs[fileKey].ScanGen,
			LineOrdinal: 1, ByteOffset: 0, ByteLen: int64(len(line)), Reason: "rejected_too_large", Seq: 99, DeadAt: x.now.Unix(), State: "dead"})
	}); err != nil {
		t.Fatal(err)
	}
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", "rejected_too_large"}, x.env); rc != 0 {
		t.Fatalf("requeue rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	if !strings.Contains(x.out.String(), "found=1 gone=0") {
		t.Fatalf("requeue output:\n%s", x.out.String())
	}
	all = x.envelopes()
	if len(all) != 2 || all[1].Envelope.Key != key {
		t.Fatalf("%d envelopes, last keyed %q, want %q", len(all), all[len(all)-1].Envelope.Key, key)
	}
	if rows, _ := st.deadRows("rejected_too_large", "requeued", 0); len(rows) != 1 || rows[0].Key != legacy {
		t.Fatalf("requeued rows %+v", rows)
	}
}

// longLeaf is a string of n bytes over leafMin, holding a synthetic secret the masker replaces.
func longLeaf(tag string, n int) string {
	return tag + " " + recheckSecrets["github"] + " " + synth.Text(n, uint64(len(tag)))
}

// TestJoinedPointerReconstruction: every path item's pointer resolves, so applying the path items to the
// frame in reverse order gives back the masked record, for leaf extraction (Codex, and Claude with
// --split) with several leaves and for the fit. A leaf whose joined pointer the masker would change stays
// in the frame, and the fit moves a subtree under such a pointer inside its nearest clean ancestor. The
// envelope passes detection everywhere (recheckBuild).
func TestJoinedPointerReconstruction(t *testing.T) {
	b := testBuilder(t)
	k1, k2 := joinedKeys(t, b)
	t.Run("codex leaves", func(t *testing.T) {
		line := string(jsonLine(t, M{"type": "event_msg", "timestamp": "2026-10-01T09:00:00Z", "payload": M{
			"type": "agent_reasoning", "a": longLeaf("a", 1500), "list": []any{longLeaf("l0", 1200), "short", M{"t": longLeaf("l2", 2000)}},
			k1: M{k2: longLeaf("joined", 1800), "near": longLeaf("near", 1100)},
		}}))
		bl := buildCodexLine(t, b, line, 2)
		r := bodyRecord(t, bl.body)
		want := []string{"/payload/a", "/payload/list/0", "/payload/list/2/t", "/payload/" + k1 + "/near"}
		if got := itemPaths(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("path items %q, want %q (the joined leaf inline)", got, want)
		}
		if !bytes.Equal(reassemble(t, r, builtPieces(bl)), maskedLine(t, line)) {
			t.Fatal("frame plus path items in reverse order is not the masked line")
		}
		if got := recheckBuild(t, b, mustClassify(t, RootCodexSessions, fitRolloutRel), []byte(line), &codexFile{threadID: gThread, sessionID: gThread, histStart: -1}); len(got) != 0 {
			t.Fatalf("the re-check finds %v", got)
		}
	})
	t.Run("claude split", func(t *testing.T) {
		main := mustClassify(t, RootClaudeProjects, gProject+"/"+gSession+".jsonl")
		line := jsonLine(t, M{"type": "system", "uuid": "u-1", "sessionId": gSession, "a": longLeaf("a", 1500),
			k1: M{k2: longLeaf("joined", 3000), "x": []any{longLeaf("x0", 1024)}}})
		sb := testBuilder(t)
		sb.split = true
		bl := buildFor(t, sb, main, line)
		r := bodyRecord(t, bl.body)
		want := []string{"/a", "/" + k1 + "/x/0"}
		if got := itemPaths(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("path items %q, want %q", got, want)
		}
		plain := bodyRecord(t, buildFor(t, testBuilder(t), main, line).body)
		if !bytes.Equal(reassemble(t, r, builtPieces(bl)), plain.Frame) {
			t.Fatal("frame plus path items in reverse order is not the frame built without --split")
		}
		if got := recheckBuild(t, sb, main, line); len(got) != 0 {
			t.Fatalf("the re-check finds %v", got)
		}
	})
	t.Run("fit", func(t *testing.T) {
		// k1 holds k2, whose 60 members are each too small to cover the overflow alone: without the clean
		// check the fit would stop at /k1/k2 and move it under a pointer the masker changes.
		inner := M{}
		for i := range 60 {
			inner[fmt.Sprintf("e%02d", i)] = M{"cells": cellList(fmt.Sprintf("e%02d", i), 12)}
		}
		main := mustClassify(t, RootClaudeProjects, gProject+"/"+gSession+".jsonl")
		line := jsonLine(t, M{"type": "system", "uuid": "u-2", "sessionId": gSession, "small": "kept", k1: M{k2: inner}})
		fb := limitBuilder(t, smallEnvelope())
		bl := buildFor(t, fb, main, line)
		if bl.tooLarge || bl.fitFrom == "" {
			t.Fatalf("record not fitted (too large %v)", bl.tooLarge)
		}
		r := bodyRecord(t, bl.body)
		if got := itemPaths(r); !reflect.DeepEqual(got, []string{"/" + k1}) {
			t.Fatalf("path items %q, want the clean ancestor /%s", got, k1)
		}
		if !bytes.Equal(reassemble(t, r, builtPieces(bl)), maskedLine(t, string(line))) {
			t.Fatal("frame plus path items in reverse order is not the masked line")
		}
		if got := recheckBuild(t, fb, main, line); len(got) != 0 {
			t.Fatalf("the re-check finds %v", got)
		}
	})
}
