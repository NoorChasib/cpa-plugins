package importer

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Synthetic secret shapes, one per rule family the re-check knows; none is a real credential.
var recheckSecrets = map[string]string{
	"xai":    "xai-" + strings.Repeat("Q7v0mZr3", 3),
	"sk":     "sk-" + "Synth0Valu3With9Digits4And8More",
	"github": "ghp_" + strings.Repeat("Ab1Cd2Ef3G", 4)[:36],
	"gitlab": "glpat-" + strings.Repeat("x9Y8z7W6", 3),
	"hf":     "hf_" + strings.Repeat("AbCdEf0123", 3),
	"aws-id": "AKIA" + "QWERTYUIOPASDFGH",
}

// recheckBuild builds one record or file of f from data with b and returns the rule ids detection finds
// in everything that leaves the machine for it: the whole serialized envelope (key, locator, body and
// every other field, each string decoded, as canon.DetectPiece reads a json piece), every piece, and the
// tombstone a local dead letter of it would send. recheck must name the same rules. A Codex record gets
// cf (nil: a rollout whose session_meta named no ids).
func recheckBuild(t *testing.T, b *builder, f fileRef, data []byte, cf ...*codexFile) []string {
	t.Helper()
	bl := buildFor(t, b, f, data, cf...)
	r := &runner{b: b, cfg: Config{MachineID: "00000000-1111-4222-8333-444444444444"}, meta: Meta{OutboxID: strings.Repeat("ab", 16)}}
	u := &unitRec{fs: &fileState{ref: f}, b: bl, seq: 7, ordinal: 1, length: int64(len(data)), end: int64(len(data)) + 1}
	env, err := r.envelope(u)
	if err != nil {
		t.Fatal(err)
	}
	tomb, err := canon.Marshal(wire.Tombstone{V: wire.ProtocolV1, OutboxID: r.meta.OutboxID, Seq: u.seq, Key: bl.key, Reason: wire.TombRejectedTooLarge})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	detect := func(kind wire.PieceKind, ct string, raw []byte) {
		fs, err := canon.DetectPiece(kind, ct, raw, b.m)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			ids[f.RuleID] = true
		}
	}
	detect(wire.PieceJSON, "", env)
	detect(wire.PieceJSON, "", tomb)
	for _, p := range bl.pieces {
		detect(p.Kind, p.ContentType, p.Bytes)
	}
	var all []string
	for id := range ids {
		all = append(all, id)
	}
	sort.Strings(all)
	rules, _, err := recheck(b.m, bl, r.locator(u))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rules, all) {
		t.Fatalf("%s: recheck names %v, detection over the envelope finds %v", f.Key, rules, all)
	}
	if !wire.ValidKey(bl.key) {
		t.Fatalf("%s: invalid key %q", f.Key, bl.key)
	}
	return all
}

// buildFor builds one record or file of f from data with b.
func buildFor(t *testing.T, b *builder, f fileRef, data []byte, cf ...*codexFile) *built {
	t.Helper()
	var bl *built
	var err error
	switch {
	case f.Whole:
		bl, err = b.fileBody(f, data, func(fileRef) map[[32]byte]bool { return nil })
	case f.Role == RoleCodexRollout:
		c := &codexFile{histStart: -1}
		if len(cf) > 0 {
			c = cf[0]
		}
		bl, err = b.codexRecord(f, c, data, 1)
	default:
		bl, err = b.claudeRecord(f, data)
	}
	if err != nil {
		t.Fatalf("%s: build: %v", f.Key, err)
	}
	if bl == nil {
		t.Fatalf("%s: nothing built", f.Key)
	}
	return bl
}

func mustClassify(t *testing.T, root, rel string) fileRef {
	t.Helper()
	f, ok := classify(root, rel)
	if !ok {
		t.Fatalf("classify %s/%s: ignored", root, rel)
	}
	return f
}

func jsonLine(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRecheckPassesBuiltEnvelopes: whatever position a secret takes in a record, a file or its path, the
// importer sends none of it: detection over the whole serialized envelope (key, locator, body), every
// piece and the record's tombstone finds nothing. A secret the archive's re-check detects in the body or
// a piece makes the record a mask_violation dead letter that no requeue --remask can recover, and one in
// the key or locator would leave the machine unmasked.
func TestRecheckPassesBuiltEnvelopes(t *testing.T) {
	b := testBuilder(t)
	for name, sec := range recheckSecrets {
		if len(b.m.Detect(sec)) == 0 {
			t.Fatalf("synthetic %s is not detected; the test would prove nothing", name)
		}
	}
	const sid = "11111111-2222-4333-8444-555555555555"
	for name, sec := range recheckSecrets {
		t.Run(name, func(t *testing.T) {
			main := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sid+".jsonl")
			cases := map[string]map[string]any{
				"type":       {"type": sec, "uuid": "u-1", "sessionId": sid},
				"uuid":       {"type": "user", "uuid": sec, "sessionId": sid},
				"sessionId":  {"type": "user", "uuid": "u-1", "sessionId": sec},
				"agentId":    {"type": "user", "uuid": "u-1", "sessionId": sid, "agentId": sec},
				"key":        {"type": "user", "uuid": "u-1", sec: "v"},
				"nested key": {"type": "user", "uuid": "u-1", "a": map[string]any{sec: map[string]any{"b": "v"}}},
				"text": {"type": "user", "uuid": "u-1", "message": map[string]any{"role": "user",
					"content": []any{map[string]any{"type": "text", "text": "see " + sec + " here"}}}},
				"tool_use": {"type": "assistant", "uuid": "u-1", "message": map[string]any{"role": "assistant",
					"content": []any{map[string]any{"type": "tool_use", "id": sec, "name": sec, "input": map[string]any{sec: sec}}}}},
				"toolUseResult": {"type": "user", "uuid": "u-1", "toolUseResult": map[string]any{"filePath": "/x/" + sec, "structuredPatch": map[string]any{sec: 1}}},
				"queue":         {"type": "queue-operation", "content": "q " + sec},
				"shape":         {"type": "api-request-shape", "shape": map[string]any{"system": []any{sec}, "tools": []any{map[string]any{sec: sec}}}},
				"attachment":    {"type": "attachment", "attachment": map[string]any{"type": "x", sec: sec}},
				"escaped":       {"type": "user", "uuid": "u-1", "note": `{\"api_key\": \"` + sec + `\"}`},
				"unicode esc":   {"type": "user", "uuid": "u-1", "note": `\u0022` + sec + `\u0022`},
				"percent":       {"type": "user", "uuid": "u-1", "note": strings.ReplaceAll(sec, "-", "%2D")},
				"adjacent":      {"type": "user", "uuid": "u-1", "note": sec + sec + recheckSecrets["sk"] + sec},
				"long":          {"type": "user", "uuid": "u-1", "note": sec + strings.Repeat("A1b2C3d4", 20000)},
			}
			for cname, rec := range cases {
				if got := recheckBuild(t, b, main, jsonLine(t, rec)); len(got) != 0 {
					t.Errorf("claude record, secret in %s: the re-check finds %v", cname, got)
				}
			}
			sub := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sid+"/subagents/agent-"+sec+".jsonl")
			if got := recheckBuild(t, b, sub, jsonLine(t, map[string]any{"type": "user", "uuid": "u-2"})); len(got) != 0 {
				t.Errorf("subagent file named with the secret: the re-check finds %v", got)
			}
			journal := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sid+"/subagents/workflows/wf_1/journal.jsonl")
			if got := recheckBuild(t, b, journal, jsonLine(t, map[string]any{"type": sec, "agentId": sec, "result": map[string]any{sec: sec}})); len(got) != 0 {
				t.Errorf("workflow journal: the re-check finds %v", got)
			}
			// A main file named by a secret-shaped session id, and a workflow journal under a secret-shaped
			// session and run id.
			named := mustClassify(t, RootClaudeProjects, "-home-synth-"+sec+"/"+sec+".jsonl")
			if got := recheckBuild(t, b, named, jsonLine(t, map[string]any{"type": "user", "uuid": sec})); len(got) != 0 {
				t.Errorf("main file named with the secret: the re-check finds %v", got)
			}
			if got := recheckBuild(t, b, named, jsonLine(t, map[string]any{"type": sec})); len(got) != 0 {
				t.Errorf("record without a uuid in a file named with the secret: the re-check finds %v", got)
			}
			wf := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sec+"/subagents/workflows/wf_"+sec+"/journal.jsonl")
			if got := recheckBuild(t, b, wf, jsonLine(t, map[string]any{"type": sec, "agentId": sec})); len(got) != 0 {
				t.Errorf("workflow journal under a secret-shaped run id: the re-check finds %v", got)
			}
			for _, rel := range []string{
				"-home-synth-" + sec + "/" + sid + "/tool-results/out.txt",
				"-home-synth-proj/" + sec + "/tool-results/out.txt",
				"-home-synth-proj/" + sid + "/tool-results/" + sec + ".txt",
			} {
				f := mustClassify(t, RootClaudeProjects, rel)
				if got := recheckBuild(t, b, f, []byte("out "+sec+"\n")); len(got) != 0 {
					t.Errorf("tool-result file %s: the re-check finds %v", strings.ReplaceAll(rel, sec, "<secret>"), got)
				}
				if got := recheckBuild(t, b, f, append([]byte("bin \xff\xfe "+sec+" "), 0x80, 'x')); len(got) != 0 {
					t.Errorf("binary tool-result file %s: the re-check finds %v", strings.ReplaceAll(rel, sec, "<secret>"), got)
				}
			}
			skill := mustClassify(t, RootClaudeSkills, sec+"/SKILL.md")
			if got := recheckBuild(t, b, skill, []byte("# skill\n")); len(got) != 0 {
				t.Errorf("skill directory named with the secret: the re-check finds %v", got)
			}
			if got := recheckBuild(t, b, mustClassify(t, RootCodexSessions, "2026/10/01/rollout-"+sec+".jsonl"), jsonLine(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "x"}})); len(got) != 0 {
				t.Errorf("codex rollout named with the secret: the re-check finds %v", got)
			}
			rollout := mustClassify(t, RootCodexSessions, "2026/10/01/rollout-x.jsonl")
			for cname, rec := range map[string]map[string]any{
				"session_meta": {"type": "session_meta", "payload": map[string]any{"id": sec, "session_id": sec}},
				"type":         {"type": sec, "payload": map[string]any{"type": sec}},
				"event":        {"type": "event_msg", "payload": map[string]any{"type": "x", "message": sec, sec: sec}},
			} {
				if got := recheckBuild(t, b, rollout, jsonLine(t, rec)); len(got) != 0 {
					t.Errorf("codex %s: the re-check finds %v", cname, got)
				}
			}
			ids := &codexFile{threadID: sec, sessionID: sec, histStart: -1}
			if got := recheckBuild(t, b, rollout, jsonLine(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "x"}}), ids); len(got) != 0 {
				t.Errorf("codex rollout whose session_meta ids hold the secret: the re-check finds %v", got)
			}
		})
	}
}

// TestRecheckJoinedPointerSegments: a JSON pointer joins frame keys that are masked one by one, so two
// keys that are clean alone can form a secret across the "/" between them (ca-aws-secret's 40-character
// class holds "/"). No path item carries such a pointer: the leaf stays in the frame, the envelope passes
// the re-check, and the record is exactly the one built without --split.
func TestRecheckJoinedPointerSegments(t *testing.T) {
	b := testBuilder(t)
	b.split = true
	const sid = "11111111-2222-4333-8444-555555555555"
	main := mustClassify(t, RootClaudeProjects, "-home-synth-proj/"+sid+".jsonl")
	k1, k2 := joinedKeys(t, b)
	line := jsonLine(t, map[string]any{"type": "user", "uuid": "u-1", k1: map[string]any{k2: strings.Repeat("long leaf text ", 400)}})
	if got := recheckBuild(t, b, main, line); len(got) != 0 {
		t.Errorf("the re-check finds %v in a joined pointer", got)
	}
	split, plain := buildFor(t, b, main, line), buildFor(t, testBuilder(t), main, line)
	if split.key != plain.key || !bytes.Equal(split.body, plain.body) || len(split.pieces) != 0 {
		t.Errorf("a leaf under a secret-shaped pointer was moved: %d pieces", len(split.pieces))
	}
}

// TestRecheckNamesRulesAndParts: recheck reports the rule ids and the part (key, loc, body, pieces) of an
// envelope the archive would refuse or that would carry a secret off the machine, ids only; localRules
// gives them to a mask_violation the archive answered without rules.
func TestRecheckNamesRulesAndParts(t *testing.T) {
	b := testBuilder(t)
	sec := recheckSecrets["xai"]
	body, err := json.Marshal(wire.ImportFile{Client: ClientClaudeCode, FileKey: "claude-projects/-p-" + sec + "/s/tool-results/a.txt", Kind: FileToolResult})
	if err != nil {
		t.Fatal(err)
	}
	piece := canon.NewPiece(wire.PieceText, "", []byte("text with "+recheckSecrets["github"]))
	bl := &built{kind: wire.KindImportFile, key: "f/k", body: body, pieces: []canon.Piece{piece}}
	rules, parts, err := recheck(b.m, bl, nil)
	if err != nil || strings.Join(rules, ",") != "ca-github,ca-xai" || strings.Join(parts, ",") != "body,pieces" {
		t.Fatalf("recheck = %v in %v (%v), want ca-github,ca-xai in body,pieces", rules, parts, err)
	}
	dirty := *bl
	dirty.key = "cc/u/" + recheckSecrets["gitlab"] + "/u-1/" + strings.Repeat("0", 64)
	loc := &wire.Locator{MachineID: "m-1", FileKey: "claude-projects/-p-" + recheckSecrets["hf"] + "/s.jsonl"}
	rules, parts, err = recheck(b.m, &dirty, loc)
	if err != nil || strings.Join(rules, ",") != "ca-github,ca-gitlab,ca-hf,ca-xai" || strings.Join(parts, ",") != "key,loc,body,pieces" {
		t.Fatalf("recheck = %v in %v (%v), want every rule in key,loc,body,pieces", rules, parts, err)
	}
	// The runner's locator carries the file key with its secret-shaped segment hashed: no loc rule.
	r := &runner{b: b}
	fs := &fileState{ref: fileRef{Key: "claude-projects/-p-" + sec + "/s/tool-results/a.txt", Whole: true}}
	if got := r.localRules(&unitRec{fs: fs, b: bl}); strings.Join(got, ",") != "ca-github,ca-xai" {
		t.Fatalf("localRules = %v", got)
	}
	var out strings.Builder
	r.env = &Env{Stdout: &out}
	if err := r.printCheck(&unitRec{fs: fs, b: bl, requeue: &Dead{Key: "f/k"}}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "check f/k rules=ca-github,ca-xai in=body,pieces\n" || strings.Contains(got, sec) {
		t.Fatalf("check line %q", got)
	}
	clean := &built{kind: wire.KindImportFile, key: "f/k", body: []byte(`{"client":"claude-code"}`)}
	if rules, parts, err := recheck(b.m, clean, &wire.Locator{MachineID: "m-1", FileKey: "claude-projects/p/s.jsonl"}); err != nil || rules != nil || parts != nil {
		t.Fatalf("clean envelope: %v %v %v", rules, parts, err)
	}
}

// TestDeadRowKeepsArchiveRules: a mask_violation whose commit result names rules records them on the
// dead row, and dead list prints them (rule ids, never content).
func TestDeadRowKeepsArchiveRules(t *testing.T) {
	x := newH(t)
	x.enroll()
	c := x.claude(gSession)
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), c.UserText("two"))
	bad := keyOfLine(t, x, path, 2)
	x.srv.RejectKeyRules(bad, wire.ReasonMaskViolation, []string{"ca-xai"})
	x.mustRun()
	dead, err := x.state().deadRows("mask_violation", "dead", 0)
	if err != nil || len(dead) != 1 || dead[0].Key != bad || dead[0].RuleIDs != "ca-xai" {
		t.Fatalf("dead rows %+v (%v), want %s with rules ca-xai", dead, err, bad)
	}
	x.out.Reset()
	if rc := Main([]string{"dead", "list"}, x.env); rc != 0 {
		t.Fatal(x.errb.String())
	}
	if dl := x.out.String(); !strings.Contains(dl, "reason=mask_violation rules=ca-xai ") {
		t.Fatalf("dead list:\n%s", dl)
	}
}

// TestRequeueDryRunChecksRules: requeue --dry-run prints, per dead letter, the rules the archive's re-check
// would find in the rebuilt envelope (rules=none: a requeue now passes) and uploads nothing.
func TestRequeueDryRunChecksRules(t *testing.T) {
	x := newH(t)
	x.enroll()
	project := "-home-synth-" + recheckSecrets["xai"] + "-tools"
	data := []byte("saved output " + x.g.Words(8) + "\n")
	fileKey := "claude-projects/" + project + "/" + gSession + "/tool-results/out.txt"
	x.writeRaw(filepath.Join(x.projects(), project, gSession, "tool-results", "out.txt"), data)
	x.mustRun()
	all := x.envelopes()
	if len(all) != 1 {
		t.Fatalf("%d envelopes", len(all))
	}
	key := all[0].Envelope.Key
	// Dead-letter it as an archive without rules in its results would.
	x.srv.RejectKey(key, wire.ReasonMaskViolation)
	st := x.state()
	curs, err := st.cursors()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.tx(func(tx *sql.Tx) error {
		return putDead(tx, Dead{Key: key, Kind: string(wire.KindImportFile), FileKey: fileKey, ScanGen: curs[fileKey].ScanGen,
			ByteLen: int64(len(data)), Reason: "mask_violation", Seq: all[0].Envelope.Seq, DeadAt: x.now.Unix(), State: "dead"})
	}); err != nil {
		t.Fatal(err)
	}
	x.out.Reset()
	if rc := Main([]string{"requeue", "--reason", "mask_violation", "--remask", "--dry-run"}, x.env); rc != 0 {
		t.Fatalf("requeue rc=%d: %s %s", rc, x.out.String(), x.errb.String())
	}
	out := x.out.String()
	if !strings.Contains(out, "check "+key+" rules=none\n") || strings.Contains(out, "xai-") {
		t.Fatalf("dry run output:\n%s", out)
	}
	if n := len(x.envelopes()); n != 1 {
		t.Fatalf("the dry run uploaded: %d envelopes", n)
	}
	if dead, _ := st.deadRows("mask_violation", "dead", 0); len(dead) != 1 {
		t.Fatalf("the dry run changed the dead row: %+v", dead)
	}
}
