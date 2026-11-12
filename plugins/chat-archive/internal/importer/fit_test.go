package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// The fit tests use synthetic MCP tool-call arguments whose bytes sit in many short strings inside
// nested objects and arrays: a batch of requests, each an update of rows of cells. No string reaches
// leafMin, so the 1 KiB leaf rule moves nothing and only a whole subtree can bring the record under
// limits.envelope_json.

// cellList is n cells of one short fixed-width string each.
func cellList(tag string, n int) []any {
	cs := make([]any, n)
	for c := range cs {
		cs[c] = M{"value": M{"text": fmt.Sprintf("%s cell %04d of the batch", tag, c)}}
	}
	return cs
}

// gridRequest is one update request of rows × cells short strings (about 55 bytes a cell). Every string
// has a fixed width, so two requests with the same counts and tags of the same length encode to the
// same size.
func gridRequest(tag string, rows, cells int) M {
	rs := make([]any, rows)
	for r := range rs {
		rs[r] = M{"cells": cellList(fmt.Sprintf("%s row %04d", tag, r), cells)}
	}
	return M{"update": M{"range": M{"table": tag, "start": 0}, "fields": "value", "rows": rs}}
}

func smallRequest(tag string) M {
	return M{"note": M{"text": tag + " small request", "at": 1}}
}

// dominantArgs is one request of about 2 MB between two small ones. Row 7 also holds a 1.5 KiB string,
// which the leaf rule moves to a text piece first, so reassembly has to put it back inside the rows
// the fit moved.
func dominantArgs() M {
	big := gridRequest("t1", 150, 240)
	big["update"].(M)["rows"].([]any)[7].(M)["note"] = synth.Text(1500, 3)
	return M{"target": "doc-1", "requests": []any{smallRequest("t0"), big, smallRequest("t2")}}
}

// manyArgs is 60 requests of about 17 to 54 KB each, about 2 MB in all: no single request covers the
// overflow, so the whole requests array moves.
func manyArgs() M {
	reqs := make([]any, 60)
	for i := range reqs {
		reqs[i] = gridRequest(fmt.Sprintf("m%02d", i), 5+i*7%12, 60)
	}
	return M{"target": "doc-2", "requests": reqs}
}

const fitRolloutRel = "2026/10/01/rollout-2026-10-01T09-00-00-" + gThread + ".jsonl"

func (x *h) rollout() string { return filepath.Join(x.codex(), filepath.FromSlash(fitRolloutRel)) }

// toolCallLines returns a rollout's session_meta, an MCP tool call with args (line 2) and a user
// message.
func toolCallLines(g *synth.Gen, args M) (meta, call, after string) {
	cx := &synth.Codex{G: g, ThreadID: gThread, Session: gThread}
	meta = cx.SessionMeta("openai", "", 0)
	call = cx.McpToolCall("example", "batch_update", args)
	after = cx.UserMessage("after the call")
	return meta, call, after
}

// buildCodexLine builds one rollout line (line number n) with b, as a run of that rollout would.
func buildCodexLine(t *testing.T, b *builder, line string, n int64) *built {
	t.Helper()
	f, ok := classify(RootCodexSessions, fitRolloutRel)
	if !ok {
		t.Fatal("rollout not classified")
	}
	out, err := b.codexRecord(f, &codexFile{threadID: gThread, sessionID: gThread, histStart: -1}, []byte(line), n)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func limitBuilder(t *testing.T, lim wire.Limits) *builder {
	t.Helper()
	b := testBuilder(t)
	b.limits = lim
	return b
}

// noFitLimits have an envelope limit no record in these tests reaches: a build under them is the
// record exactly as it was before the fit existed, which is also how importer 0.1.0 keyed it.
func noFitLimits() wire.Limits {
	lim := wire.DefaultLimits
	lim.EnvelopeJSON = 1 << 40
	return lim
}

func smallEnvelope() wire.Limits {
	lim := wire.DefaultLimits
	lim.EnvelopeJSON = 32 << 10
	return lim
}

func estimate(o *built) int64 { return int64(len(o.body)) + 67*int64(len(o.refs)) + 1024 }

func bodyRecord(t *testing.T, body []byte) wire.ImportRecord {
	t.Helper()
	var r wire.ImportRecord
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// itemPath returns the JSON pointer of a path item ({"f":{"path":…},"p":…}).
func itemPath(it wire.Item) (string, bool) {
	var f map[string]any
	if it.P == nil || len(it.M) > 0 || json.Unmarshal(it.F, &f) != nil || len(f) != 1 {
		return "", false
	}
	p, ok := f["path"].(string)
	return p, ok
}

func itemPaths(r wire.ImportRecord) []string {
	var out []string
	for _, it := range r.Items {
		if p, ok := itemPath(it); ok {
			out = append(out, p)
		}
	}
	return out
}

// reassemble applies a record's path items to its frame in reverse order and returns the canon/v1
// result. A member must be absent and an element null where a piece goes back.
func reassemble(t *testing.T, r wire.ImportRecord, piece func(wire.Hash) (wire.PieceKind, []byte, bool)) []byte {
	t.Helper()
	root, err := decodeAny(r.Frame)
	if err != nil {
		t.Fatal(err)
	}
	unescape := strings.NewReplacer("~1", "/", "~0", "~")
	for i := len(r.Items) - 1; i >= 0; i-- {
		ptr, ok := itemPath(r.Items[i])
		if !ok {
			continue
		}
		kind, raw, ok := piece(*r.Items[i].P)
		if !ok {
			t.Fatalf("%s: piece missing", ptr)
		}
		var v any = string(raw)
		if kind == wire.PieceJSON {
			if v, err = decodeAny(raw); err != nil {
				t.Fatal(err)
			}
		}
		segs := strings.Split(ptr, "/")[1:]
		n := root
		for j, s := range segs {
			s = unescape.Replace(s)
			last := j == len(segs)-1
			switch c := n.(type) {
			case map[string]any:
				if last {
					if _, dup := c[s]; dup {
						t.Fatalf("%s: member still in the frame", ptr)
					}
					c[s] = v
				}
				n = c[s]
			case []any:
				k, err := strconv.Atoi(s)
				if err != nil || k >= len(c) {
					t.Fatalf("%s: no element %q", ptr, s)
				}
				if last {
					if c[k] != nil {
						t.Fatalf("%s: element not null in the frame", ptr)
					}
					c[k] = v
				}
				n = c[k]
			default:
				t.Fatalf("%s: no container at %q", ptr, s)
			}
		}
	}
	out, err := canon.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func builtPieces(o *built) func(wire.Hash) (wire.PieceKind, []byte, bool) {
	return func(h wire.Hash) (wire.PieceKind, []byte, bool) {
		for _, p := range o.pieces {
			if p.Hash == h {
				return p.Kind, p.Bytes, true
			}
		}
		return 0, nil, false
	}
}

// maskedLine is the whole line masked and canonical: what a frame and its path items stand for.
func maskedLine(t *testing.T, line string) []byte {
	t.Helper()
	m, err := mask.New(mask.Config{})
	if err != nil {
		t.Fatal(err)
	}
	out, _, _, err := canon.CanonicalizeMasked([]byte(line), m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFitOversizedToolCallSent: an MCP tool call of about 2 MB of short strings in nested objects and
// arrays is sent, not dead-lettered. The deepest subtree that alone covers the overflow becomes a json
// piece; the envelope fits; the frame plus its path items, applied in reverse order, give back the
// masked line; and a direct build gives the same key and body as the run sent.
func TestFitOversizedToolCallSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		args M
		want []string
	}{
		{"one dominant request", dominantArgs(), []string{
			"/payload/item/arguments/requests/1/update/rows/7/note", // the 1.5 KiB string: a text piece, as before
			"/payload/item/arguments/requests/1/update/rows",        // the fit
		}},
		{"many medium requests", manyArgs(), []string{"/payload/item/arguments/requests"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newH(t)
			x.enroll()
			meta, call, after := toolCallLines(x.g, tc.args)
			if n := len(call); n < 1900<<10 || n > 2400<<10 {
				t.Fatalf("line of %d bytes, want about 2 MB", n)
			}
			x.write(x.rollout(), meta, call, after)
			x.mustRun()
			if !strings.Contains(x.out.String(), " fitted=1") {
				t.Errorf("run output %q, want fitted=1", x.out.String())
			}
			if dead, _ := x.state().deadRows("", "", 0); len(dead) != 0 {
				t.Fatalf("dead rows %+v", dead)
			}
			if tombs := x.srv.Tombstones(x.sourceID); len(tombs) != 0 {
				t.Fatalf("tombstones %+v", tombs)
			}
			all := x.envelopes()
			if got := statuses(all); len(all) != 3 || got[wire.StatusOK] != 3 {
				t.Fatalf("%d envelopes %v, want 3 ok", len(all), got)
			}
			e := all[1]
			r := decodeRecord(t, e.Envelope)
			if r.RecordType != "event_msg/item_completed" {
				t.Fatalf("second envelope is %s", r.RecordType)
			}
			if lim := wire.DefaultLimits.EnvelopeJSON; int64(len(e.Raw)) > lim {
				t.Fatalf("envelope of %d bytes, over %d", len(e.Raw), lim)
			}
			if got := itemPaths(r); !reflect.DeepEqual(got, tc.want) || len(r.Items) != len(tc.want) {
				t.Fatalf("path items %q (%d items), want %q", got, len(r.Items), tc.want)
			}
			fromServer := func(h wire.Hash) (wire.PieceKind, []byte, bool) { return x.srv.Piece(h) }
			if !bytes.Equal(reassemble(t, r, fromServer), maskedLine(t, call)) {
				t.Fatal("frame plus path items in reverse order is not the masked line")
			}
			direct := buildCodexLine(t, testBuilder(t), call, 2)
			if direct.key != e.Envelope.Key || !bytes.Equal(direct.body, e.Envelope.Body) {
				t.Fatal("a direct build differs from the envelope the run sent")
			}
			if before := buildCodexLine(t, limitBuilder(t, noFitLimits()), call, 2); before.key == direct.key {
				t.Fatal("the fitted key equals the key before the fit")
			}
		})
	}
}

// TestFitDescentTieBreak: where two siblings encode to the same size and either alone covers the
// overflow, the fit takes the first key in byte order or the lowest index, never map order. The same
// line, masker and limits give the same key and body every time, and reassembly gives the masked line.
func TestFitDescentTieBreak(t *testing.T) {
	for _, tc := range []struct {
		name string
		args M
		want string
	}{
		{"object", M{"beta": gridRequest("bb", 6, 60), "alpha": gridRequest("aa", 6, 60), "target": "doc-5"},
			"/payload/item/arguments/alpha/update/rows"},
		{"array", M{"requests": []any{gridRequest("aa", 6, 60), gridRequest("bb", 6, 60)}, "target": "doc-6"},
			"/payload/item/arguments/requests/0/update/rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := canon.Marshal(gridRequest("aa", 6, 60))
			b, _ := canon.Marshal(gridRequest("bb", 6, 60))
			if len(a) != len(b) {
				t.Fatalf("siblings encode to %d and %d bytes, want a tie", len(a), len(b))
			}
			_, call, _ := toolCallLines(synth.New(5), tc.args)
			lim := smallEnvelope()
			first := buildCodexLine(t, limitBuilder(t, lim), call, 2)
			if first.tooLarge {
				t.Fatal("dead-lettered, want fitted")
			}
			if est := estimate(first); est > lim.EnvelopeJSON {
				t.Fatalf("estimate %d over %d", est, lim.EnvelopeJSON)
			}
			r := bodyRecord(t, first.body)
			if got := itemPaths(r); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("path items %q, want [%s]", got, tc.want)
			}
			if !bytes.Equal(reassemble(t, r, builtPieces(first)), maskedLine(t, call)) {
				t.Fatal("frame plus path items in reverse order is not the masked line")
			}
			warm := limitBuilder(t, lim)
			for i := range 20 {
				bl := warm
				if i%2 == 0 {
					bl = limitBuilder(t, lim)
				}
				got := buildCodexLine(t, bl, call, 2)
				if got.key != first.key || !bytes.Equal(got.body, first.body) {
					t.Fatalf("build %d differs: key %s, want %s", i, got.key, first.key)
				}
			}
		})
	}
}

// spread is n objects ("a", "b", …) of 220 cells (about 10 KB) each: under a 32 KiB envelope none of
// them covers the overflow alone, so the descent stops on the node that holds them.
func spread(tag string, n int) M {
	out := M{}
	for i := range n {
		k := string(rune('a' + i))
		out[k] = M{"cells": cellList(tag+k, 220)}
	}
	return out
}

// fitLine builds a rollout line (an MCP tool call) under lim and checks that it was fitted: not too
// large, its estimate under the limit, and its frame plus path items in reverse order the masked line.
// It returns the path items.
func fitLine(t *testing.T, call string, lim wire.Limits) []string {
	t.Helper()
	bl := buildCodexLine(t, limitBuilder(t, lim), call, 2)
	if bl.tooLarge || bl.fitFrom == "" {
		t.Fatalf("too large %v, fitted %v: want fitted", bl.tooLarge, bl.fitFrom != "")
	}
	if est := estimate(bl); est > lim.EnvelopeJSON {
		t.Fatalf("estimate %d over %d", est, lim.EnvelopeJSON)
	}
	r := bodyRecord(t, bl.body)
	if !bytes.Equal(reassemble(t, r, builtPieces(bl)), maskedLine(t, call)) {
		t.Fatal("frame plus path items in reverse order is not the masked line")
	}
	return itemPaths(r)
}

// TestFitDescentStops: the descent stops on an array element (which becomes null in the frame) and on
// an object member whose key needs JSON-pointer escaping (~ as ~0, / as ~1), including a 1.5 KiB
// string the leaf rule moved first from inside the moved subtree.
func TestFitDescentStops(t *testing.T) {
	note := spread("fn", 5)
	note["note~1/x"] = synth.Text(1500, 3)
	for _, tc := range []struct {
		name string
		args M
		want []string
	}{
		{"array element", M{"target": "doc-7", "requests": []any{spread("q", 5), smallRequest("t1")}},
			[]string{"/payload/item/arguments/requests/0"}},
		{"escaped key", M{"target": "doc-8", "files": M{"src/a~b.go": note, "README": smallRequest("t1")}},
			[]string{"/payload/item/arguments/files/src~1a~0b.go/note~01~1x", "/payload/item/arguments/files/src~1a~0b.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, call, _ := toolCallLines(synth.New(7), tc.args)
			if got := fitLine(t, call, smallEnvelope()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("path items %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFitNeedHeadroom pins how far the descent goes. A child is taken when its size is at least the
// overflow plus leafMin (the room the path item and its ref need); a child even one byte smaller
// stays, and its parent moves whole. Either way one pass is enough. The envelope limit is set from
// the record's own sizes so that the child lands exactly on each side of that boundary.
func TestFitNeedHeadroom(t *testing.T) {
	_, call, _ := toolCallLines(synth.New(8), M{"target": "doc-9", "P": M{"C": cellList("pc", 360), "D": cellList("pd", 300)}})
	before := buildCodexLine(t, limitBuilder(t, noFitLimits()), call, 2)
	frame, err := decodeAny(bodyRecord(t, before.body).Frame)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := canon.Marshal(frame.(map[string]any)["payload"].(map[string]any)["item"].(map[string]any)["arguments"].(map[string]any)["P"].(map[string]any)["C"])
	size := int64(len(c))
	for _, tc := range []struct {
		name string
		room int64 // the child's size minus the overflow
		want string
	}{
		{"parent moves", leafMin - 1, "/payload/item/arguments/P"},
		{"child moves", leafMin, "/payload/item/arguments/P/C"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lim := wire.DefaultLimits
			lim.EnvelopeJSON = estimate(before) - (size - tc.room)
			if got := fitLine(t, call, lim); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("path items %q, want [%s]", got, tc.want)
			}
		})
	}
}

// TestFitRootSpread: when no child of the root covers the overflow alone, the fit moves the root's
// heaviest child and measures again; the second pass descends. Four root objects of about 12 KB put a
// 32 KiB envelope about 17 KB over, so the first pass moves /d whole; the rest is then about 5 KB over,
// which /c's cells cover. A Claude record takes the fit too.
func TestFitRootSpread(t *testing.T) {
	x := newH(t)
	x.tooLargeServer(smallEnvelope())
	x.enroll()
	c := x.claude(gSession)
	fields := M{"totalCostUSD": 0.5}
	for i, k := range []string{"a", "b", "c", "d"} {
		fields[k] = M{"cells": cellList("s"+k, 250+i)}
	}
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), c.State("cost-state", fields), c.UserText("three"))
	x.mustRun()
	all := x.envelopes()
	if got := statuses(all); len(all) != 3 || got[wire.StatusOK] != 3 {
		t.Fatalf("%d envelopes %v, want 3 ok", len(all), got)
	}
	r := decodeRecord(t, all[1].Envelope)
	if got, want := itemPaths(r), []string{"/d", "/c/cells"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("path items %q, want %q", got, want)
	}
	if !strings.Contains(x.out.String(), " fitted=1") {
		t.Errorf("run output %q, want fitted=1", x.out.String())
	}
	data, _ := os.ReadFile(path)
	line := strings.Split(string(data), "\n")[1]
	fromServer := func(h wire.Hash) (wire.PieceKind, []byte, bool) { return x.srv.Piece(h) }
	if !bytes.Equal(reassemble(t, r, fromServer), maskedLine(t, line)) {
		t.Fatal("frame plus path items in reverse order is not the masked line")
	}
	x.checkHW()
}

// TestFitLeavesRecordsUnderTheLimit: a record just under limits.envelope_json is built exactly as
// before the fit existed (same key, body and refs). One row more puts it over, and then it is fitted.
func TestFitLeavesRecordsUnderTheLimit(t *testing.T) {
	limit := wire.DefaultLimits.EnvelopeJSON
	line := func(rows int) string {
		_, call, _ := toolCallLines(synth.New(9), M{"target": "doc-3", "requests": []any{gridRequest("t1", rows, 60)}})
		return call
	}
	nb := limitBuilder(t, noFitLimits())
	e1, e2 := estimate(buildCodexLine(t, nb, line(1), 2)), estimate(buildCodexLine(t, nb, line(2), 2))
	rows := int((limit-e1)/(e2-e1)) + 1
	under := line(rows)
	before := buildCodexLine(t, nb, under, 2)
	if e := estimate(before); e > limit || e <= limit-(e2-e1) {
		t.Fatalf("estimate %d, want just under %d", e, limit)
	}
	got := buildCodexLine(t, testBuilder(t), under, 2)
	if got.tooLarge || got.key != before.key || !bytes.Equal(got.body, before.body) || !reflect.DeepEqual(got.refs, before.refs) {
		t.Fatalf("a record of %d bytes was not built as before the fit", len(under))
	}
	over := line(rows + 1)
	fitted := buildCodexLine(t, testBuilder(t), over, 2)
	if fitted.tooLarge || fitted.key == buildCodexLine(t, nb, over, 2).key {
		t.Fatalf("one row over the limit: too large %v, want fitted", fitted.tooLarge)
	}
}

// TestFitUnfittableKeepsTodaysKey: a record the fit cannot bring under the limits is dead-lettered
// under the key it had before the fit: bulk in one root-level string (no object or array to move),
// and a subtree whose piece would be over limits.text_piece.
func TestFitUnfittableKeepsTodaysKey(t *testing.T) {
	t.Run("root-level string", func(t *testing.T) {
		x := newH(t)
		x.tooLargeServer(smallEnvelope())
		x.enroll()
		c := x.claude(gSession)
		path := x.mainFile(gProject, gSession)
		x.write(path, c.UserText("one"), c.System("local_command", synth.Text(64<<10, 3)))
		x.mustRun()
		want := keyOfLine(t, x, path, 2)
		if dead, _ := x.state().deadRows("rejected_too_large", "dead", 0); len(dead) != 1 || dead[0].Key != want {
			t.Fatalf("dead rows %+v, want one under today's key", dead)
		}
		if strings.Contains(x.out.String(), "fitted=") {
			t.Errorf("run output %q counts a fit", x.out.String())
		}
	})
	t.Run("objects under 1 KiB", func(t *testing.T) {
		x := newH(t)
		x.tooLargeServer(smallEnvelope())
		x.enroll()
		c := x.claude(gSession)
		fields := M{}
		for i := range 60 {
			fields[fmt.Sprintf("f%02d", i)] = M{"cells": cellList(fmt.Sprintf("u%02d", i), 18)}
		}
		if raw, _ := canon.Marshal(fields["f00"]); len(raw) >= leafMin {
			t.Fatalf("an object of %d bytes, want under %d", len(raw), leafMin)
		}
		path := x.mainFile(gProject, gSession)
		x.write(path, c.UserText("one"), c.State("cost-state", fields))
		x.mustRun()
		want := keyOfLine(t, x, path, 2)
		if dead, _ := x.state().deadRows("rejected_too_large", "dead", 0); len(dead) != 1 || dead[0].Key != want {
			t.Fatalf("dead rows %+v, want one under today's key", dead)
		}
		if strings.Contains(x.out.String(), "fitted=") {
			t.Errorf("run output %q counts a fit", x.out.String())
		}
	})
	t.Run("piece over text_piece", func(t *testing.T) {
		x := newH(t)
		lim := smallEnvelope()
		lim.TextPiece = 16 << 10
		x.tooLargeServer(lim)
		x.enroll()
		meta, call, after := toolCallLines(x.g, M{"target": "doc-4", "requests": []any{smallRequest("t0"), gridRequest("t1", 12, 60)}})
		x.write(x.rollout(), meta, call, after)
		x.mustRun()
		want := buildCodexLine(t, limitBuilder(t, noFitLimits()), call, 2).key
		if dead, _ := x.state().deadRows("rejected_too_large", "dead", 0); len(dead) != 1 || dead[0].Key != want {
			t.Fatalf("dead rows %+v, want one under today's key", dead)
		}
		if strings.Contains(x.out.String(), "fitted=") {
			t.Errorf("run output %q counts a fit", x.out.String())
		}
	})
}

// TestFitNeverOnResponseItem: a Codex response_item over the limit is never fitted, even when its
// frame holds a subtree that would make it fit; it stays a dead letter under today's key.
func TestFitNeverOnResponseItem(t *testing.T) {
	x := newH(t)
	x.tooLargeServer(smallEnvelope())
	x.enroll()
	cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
	meta := cx.SessionMeta("openai", "", 0)
	var m map[string]any
	if err := json.Unmarshal([]byte(cx.UserMessage("question")), &m); err != nil {
		t.Fatal(err)
	}
	m["context"] = M{"cells": cellList("rc", 700)}
	raw, _ := json.Marshal(m)
	item := string(raw)
	x.write(x.rollout(), meta, item)
	if bl := buildCodexLine(t, limitBuilder(t, smallEnvelope()), item, 2); !bl.tooLarge {
		t.Fatal("a response_item over the limit was fitted")
	}
	x.mustRun()
	want := buildCodexLine(t, limitBuilder(t, noFitLimits()), item, 2).key
	if dead, _ := x.state().deadRows("rejected_too_large", "dead", 0); len(dead) != 1 || dead[0].Key != want {
		t.Fatalf("dead rows %+v, want the response_item under today's key", dead)
	}
	if strings.Contains(x.out.String(), "fitted=") {
		t.Errorf("run output %q counts a fit", x.out.String())
	}
}

// TestFitRequeuePreFitKey: a dead row keyed before the fit (as importer 0.1.0 wrote it; here the fit
// fails at first because its piece is over limits.text_piece) is rebuilt by a plain requeue: the dry
// run finds it with gone=0 and counts the fit, and the real requeue sends it under the fitted key and
// marks the old row requeued. A row whose bytes changed since is still gone.
func TestFitRequeuePreFitKey(t *testing.T) {
	args := M{"target": "doc-4", "requests": []any{smallRequest("t0"), gridRequest("t1", 12, 60)}}
	setup := func(t *testing.T) (*h, wire.Limits, string) {
		x := newH(t)
		lim := smallEnvelope()
		lim.TextPiece = 16 << 10
		x.tooLargeServer(lim)
		x.enroll()
		meta, call, after := toolCallLines(x.g, args)
		x.write(x.rollout(), meta, call, after)
		x.mustRun()
		pre := buildCodexLine(t, limitBuilder(t, noFitLimits()), call, 2).key
		dead, _ := x.state().deadRows("rejected_too_large", "dead", 0)
		if len(dead) != 1 || dead[0].Key != pre {
			t.Fatalf("dead rows %+v, want one under the pre-fit key", dead)
		}
		lim.TextPiece = wire.DefaultLimits.TextPiece
		x.srv.SetLimits(lim)
		return x, lim, pre
	}
	requeue := func(t *testing.T, x *h, args ...string) string {
		t.Helper()
		x.out.Reset()
		if rc := Main(append([]string{"requeue", "--reason", "rejected_too_large"}, args...), x.env); rc != 0 {
			t.Fatalf("requeue %v rc=%d: %s %s", args, rc, x.out.String(), x.errb.String())
		}
		return x.out.String()
	}

	t.Run("plain requeue", func(t *testing.T) {
		x, lim, pre := setup(t)
		sent := len(x.envelopes())
		out := requeue(t, x, "--dry-run")
		if !strings.Contains(out, "found=1 gone=0") || !strings.Contains(out, " fitted=1") {
			t.Fatalf("dry run: %q, want found=1 gone=0 and fitted=1", out)
		}
		if n := len(x.envelopes()); n != sent {
			t.Fatalf("the dry run sent %d envelopes", n-sent)
		}
		if dead, _ := x.state().deadRows("rejected_too_large", "", 0); len(dead) != 1 || dead[0].State != "dead" {
			t.Fatalf("dead rows after the dry run %+v", dead)
		}
		out = requeue(t, x)
		if !strings.Contains(out, "found=1 gone=0") || !strings.Contains(out, " fitted=1") {
			t.Fatalf("requeue: %q, want found=1 gone=0 and fitted=1", out)
		}
		all := x.envelopes()
		if len(all) != sent+1 {
			t.Fatalf("requeue sent %d envelopes, want 1", len(all)-sent)
		}
		last := all[len(all)-1]
		if last.Status != wire.StatusOK || last.Envelope.Key == pre || int64(len(last.Raw)) > lim.EnvelopeJSON {
			t.Fatalf("requeued envelope: %s, key changed %v, %d bytes", last.Status, last.Envelope.Key != pre, len(last.Raw))
		}
		if got := itemPaths(decodeRecord(t, last.Envelope)); len(got) != 1 || got[0] != "/payload/item/arguments/requests/1/update/rows" {
			t.Fatalf("path items %q", got)
		}
		if dead, _ := x.state().deadRows("rejected_too_large", "", 0); len(dead) != 1 || dead[0].Key != pre || dead[0].State != "requeued" {
			t.Fatalf("dead rows after requeue %+v", dead)
		}
		x.checkHW()
	})

	t.Run("bytes changed", func(t *testing.T) {
		x, _, _ := setup(t)
		data, err := os.ReadFile(x.rollout())
		if err != nil {
			t.Fatal(err)
		}
		changed := bytes.Replace(data, []byte("t1 row 0003 cell 0007"), []byte("t1 row 0003 cell 000x"), 1)
		if bytes.Equal(changed, data) {
			t.Fatal("nothing to change")
		}
		x.writeRaw(x.rollout(), changed)
		sent := len(x.envelopes())
		if out := requeue(t, x, "--dry-run"); !strings.Contains(out, "found=0 gone=1") {
			t.Fatalf("dry run: %q, want found=0 gone=1", out)
		}
		if out := requeue(t, x); !strings.Contains(out, "found=0 gone=1") {
			t.Fatalf("requeue: %q, want found=0 gone=1", out)
		}
		if n := len(x.envelopes()); n != sent {
			t.Fatalf("requeue sent %d envelopes for changed bytes", n-sent)
		}
		if dead, _ := x.state().deadRows("rejected_too_large", "", 0); len(dead) != 1 || dead[0].State != "gone" {
			t.Fatalf("dead rows %+v, want one gone", dead)
		}
	})
}

// TestFitFrameOnlyClaudeRecord: a Claude record with no content items (file-history-snapshot) whose
// frame is over the limit is fitted: its tracked-file map becomes one json piece. On the archive a
// frame-only record that gains a path item becomes a system turn, as `requeue --split` does today.
func TestFitFrameOnlyClaudeRecord(t *testing.T) {
	x := newH(t)
	x.tooLargeServer(smallEnvelope())
	x.enroll()
	c := x.claude(gSession)
	backups := M{}
	for i := range 400 {
		backups[fmt.Sprintf("pkg/mod%03d/file.go", i)] = M{"backupFileName": fmt.Sprintf("file-%03d@v2", i), "version": 2, "backupTime": "2026-10-01T09:00:00.000Z"}
	}
	snap := c.State("file-history-snapshot", M{"messageId": x.g.UUID(), "isSnapshotUpdate": false,
		"snapshot": M{"messageId": x.g.UUID(), "timestamp": x.g.TS(), "trackedFileBackups": backups}})
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), snap)
	x.mustRun()
	all := x.envelopes()
	if got := statuses(all); len(all) != 2 || got[wire.StatusOK] != 2 {
		t.Fatalf("%d envelopes %v, want 2 ok", len(all), got)
	}
	r := decodeRecord(t, all[1].Envelope)
	if r.RecordType != "file-history-snapshot" {
		t.Fatalf("second envelope is %s", r.RecordType)
	}
	if got := itemPaths(r); len(got) != 1 || got[0] != "/snapshot/trackedFileBackups" || len(r.Items) != 1 {
		t.Fatalf("path items %q (%d items), want [/snapshot/trackedFileBackups]", got, len(r.Items))
	}
	if !strings.Contains(x.out.String(), " fitted=1") {
		t.Errorf("run output %q, want fitted=1", x.out.String())
	}
	fromServer := func(h wire.Hash) (wire.PieceKind, []byte, bool) { return x.srv.Piece(h) }
	if !bytes.Equal(reassemble(t, r, fromServer), maskedLine(t, snap)) {
		t.Fatal("frame plus path items in reverse order is not the masked line")
	}
	x.checkHW()
}

// TestFitClaudeToolResultRecord: a Claude user record with content items (a tool result) whose frame
// is over the limit keeps its items as built, and the fit's path item comes after them. On the archive
// that path item is one more (opaque) block of the user message, so the turn derives as a message
// rather than a tool result, as with `requeue --split`.
func TestFitClaudeToolResultRecord(t *testing.T) {
	x := newH(t)
	x.tooLargeServer(smallEnvelope())
	x.enroll()
	c := x.claude(gSession)
	var m map[string]any
	if err := json.Unmarshal([]byte(c.ToolResult("toolu_"+x.g.Alnum(12), "updated", nil)), &m); err != nil {
		t.Fatal(err)
	}
	m["mcpMeta"] = M{"structuredContent": M{"rows": cellList("tr", 700)}}
	raw, _ := json.Marshal(m)
	result := string(raw)
	path := x.mainFile(gProject, gSession)
	x.write(path, c.UserText("one"), result)
	x.mustRun()
	all := x.envelopes()
	if got := statuses(all); len(all) != 2 || got[wire.StatusOK] != 2 {
		t.Fatalf("%d envelopes %v, want 2 ok", len(all), got)
	}
	r := decodeRecord(t, all[1].Envelope)
	f, _ := classify(RootClaudeProjects, strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(path, x.projects())), "/"))
	f.Path = path
	before, err := limitBuilder(t, noFitLimits()).claudeRecord(f, raw)
	if err != nil {
		t.Fatal(err)
	}
	pre := bodyRecord(t, before.body)
	items := pre.Items
	if len(items) == 0 || len(r.Items) != len(items)+1 || !reflect.DeepEqual(r.Items[:len(items)], items) {
		t.Fatalf("items %+v, want the items as built (%d) and then the path item", r.Items, len(items))
	}
	if p, ok := itemPath(r.Items[len(items)]); !ok || p != "/mcpMeta/structuredContent/rows" {
		t.Fatalf("last item %+v, want the path item /mcpMeta/structuredContent/rows", r.Items[len(items)])
	}
	fromServer := func(h wire.Hash) (wire.PieceKind, []byte, bool) { return x.srv.Piece(h) }
	if !bytes.Equal(reassemble(t, r, fromServer), pre.Frame) {
		t.Fatal("frame plus path items in reverse order is not the frame as built")
	}
}

// TestFrameReencodesToItself: a frame decoded with decodeAny and encoded again with canon.Marshal is
// byte-identical, for every import record of the golden corpus. The fit relies on it: a moved subtree's
// piece is exactly the bytes it had inside the frame.
func TestFrameReencodesToItself(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "envelopes.golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	n := 0
	for sc.Scan() {
		var e struct {
			Kind wire.Kind       `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != wire.KindImportRecord {
			continue
		}
		r := bodyRecord(t, e.Body)
		v, err := decodeAny(r.Frame)
		if err != nil {
			t.Fatal(err)
		}
		again, err := canon.Marshal(v)
		if err != nil || !bytes.Equal(again, r.Frame) {
			t.Fatalf("%s frame re-encodes differently (%v)", r.RecordType, err)
		}
		n++
	}
	if err := sc.Err(); err != nil || n == 0 {
		t.Fatalf("%d frames read (%v)", n, err)
	}
}
