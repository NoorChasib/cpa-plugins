package wire_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func TestHashText(t *testing.T) {
	h := sum("synthetic")
	hexs := hex.EncodeToString(h[:])
	if h.String() != hexs {
		t.Fatalf("String = %s", h.String())
	}
	b, err := json.Marshal(struct {
		H  wire.Hash   `json:"h"`
		P  *wire.Hash  `json:"p"`
		L  []wire.Hash `json:"l"`
		NP *wire.Hash  `json:"np,omitempty"`
	}{H: h, P: &h, L: []wire.Hash{h}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"h":"` + hexs + `","p":"` + hexs + `","l":["` + hexs + `"]}`
	if string(b) != want {
		t.Fatalf("json = %s", b)
	}
	got, err := wire.ParseHash(hexs)
	if err != nil || got != h {
		t.Fatalf("ParseHash = %v, %v", got, err)
	}
	for _, bad := range []string{"", hexs[:63], hexs + "0", strings.ToUpper(hexs), "g" + hexs[1:], " " + hexs[1:]} {
		if _, err := wire.ParseHash(bad); err == nil {
			t.Errorf("ParseHash(%q) accepted", bad)
		}
		var x wire.Hash
		if err := json.Unmarshal([]byte(`"`+bad+`"`), &x); err == nil {
			t.Errorf("json %q accepted", bad)
		}
	}
	if err := json.Unmarshal([]byte(`123`), &got); err == nil {
		t.Errorf("number accepted as a hash")
	}
}

func TestPieceKind(t *testing.T) {
	for k, s := range map[wire.PieceKind]string{wire.PieceJSON: "json", wire.PieceText: "text", wire.PieceAttachment: "attachment"} {
		if k.String() != s {
			t.Errorf("%d.String() = %s", k, k.String())
		}
		got, err := wire.ParsePieceKind(s)
		if err != nil || got != k {
			t.Errorf("ParsePieceKind(%s) = %v, %v", s, got, err)
		}
	}
	if wire.PieceKind(3).String() != "PieceKind(3)" {
		t.Errorf("unknown kind string %s", wire.PieceKind(3))
	}
	for _, bad := range []string{"", "JSON", "blob", "0"} {
		if _, err := wire.ParsePieceKind(bad); err == nil {
			t.Errorf("ParsePieceKind(%q) accepted", bad)
		}
	}
}

func TestKindSourceScoped(t *testing.T) {
	for k, want := range map[wire.Kind]bool{
		wire.KindExchangeBegin: true, wire.KindExchangeAttempt: true, wire.KindExchangeResponse: true,
		wire.KindExchangeDone: true, wire.KindUsage: true, wire.KindImportRecord: false, wire.KindImportFile: false,
		"exchange.unknown": false,
	} {
		if k.SourceScoped() != want {
			t.Errorf("%s.SourceScoped() = %v", k, !want)
		}
	}
}

func TestValidKey(t *testing.T) {
	good := []string{"a", "ex/req_1/begin/0", "cc/u/x:y@z.-_/1", strings.Repeat("k", wire.MaxKeyLen)}
	bad := []string{"", strings.Repeat("k", wire.MaxKeyLen+1), "a b", "a\x00", "é", "a?b", "a#b", "a%20", "a\\b", "a+b", "a=b"}
	for _, k := range good {
		if !wire.ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range bad {
		if wire.ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
	}
}

func TestKeyBuilders(t *testing.T) {
	sha := sum("record").String()
	content := sum("content")
	fk := sha256.Sum256([]byte("claude-projects/p/s.jsonl"))
	for got, want := range map[string]string{
		wire.ExchangeBeginKey("r1", 0):                                    "ex/r1/begin/0",
		wire.ExchangeBeginKey("r1", 4294967295):                           "ex/r1/begin/4294967295",
		wire.ExchangeAttemptKey("r1", 2):                                  "ex/r1/attempt/2",
		wire.ExchangeResponseKey("r1", 1):                                 "ex/r1/response/1",
		wire.ExchangeCompletionKey("r1"):                                  "ex/r1/completion",
		wire.UsageKey("u1"):                                               "usage/u1",
		wire.ClaudeUUIDKey("s1", "u1", sha):                               "cc/u/s1/u1/" + sha,
		wire.ClaudeNoUUIDKey("s1", "custom-title", sha):                   "cc/n/s1/custom-title/" + sha,
		wire.JournalKey("run1", "", "step", sha):                          "cc/j/run1/-/step/" + sha,
		wire.JournalKey("run1", "a1", "step", sha):                        "cc/j/run1/a1/step/" + sha,
		wire.CodexKey("t1", 17, sha):                                      "cx/t1/17/" + sha,
		wire.ImportFileKey("codex", "claude-projects/p/s.jsonl", content): "f/codex/" + hex.EncodeToString(fk[:])[:32] + "/" + content.String(),
	} {
		if got != want {
			t.Errorf("key %q, want %q", got, want)
		}
		if !wire.ValidKey(got) {
			t.Errorf("key %q is not valid", got)
		}
	}
}

func TestRecordKeyHash(t *testing.T) {
	frame := json.RawMessage(`{"type":"user","uuid":"u1"}`)
	ids := []wire.Hash{sum("i1"), sum("i2")}
	extra := sum("extra")
	check := func(got string, arr []any) {
		t.Helper()
		want := sha256.Sum256(canonJSON(t, arr))
		if got != hex.EncodeToString(want[:]) {
			t.Errorf("RecordKeyHash = %s, want sha256(%s)", got, canonJSON(t, arr))
		}
	}
	check(wire.RecordKeyHash(frame, ids, &extra), []any{frame, ids[0].String(), ids[1].String(), extra.String()})
	check(wire.RecordKeyHash(frame, ids, nil), []any{frame, ids[0].String(), ids[1].String()})
	check(wire.RecordKeyHash(frame, nil, nil), []any{frame})
	check(wire.RecordKeyHash(frame, nil, &extra), []any{frame, extra.String()})
	check(wire.RecordKeyHash(nil, nil, nil), []any{nil})
	// The formula is list concatenation, so [frame,id0]+extra id1 hashes the
	// same array as [frame,id0,id1].
	if wire.RecordKeyHash(frame, ids[:1], &ids[1]) != wire.RecordKeyHash(frame, ids, nil) {
		t.Errorf("RecordKeyHash is not the concatenation of frame, ids and extra")
	}
	if len(wire.RecordKeyHash(frame, ids, nil)) != 64 {
		t.Errorf("RecordKeyHash length")
	}
}

func TestParseServerState(t *testing.T) {
	h := http.Header{}
	if _, ok := wire.ParseServerState(h); ok {
		t.Fatal("empty header ok")
	}
	h.Set(wire.HeaderEpoch, "3")
	if _, ok := wire.ParseServerState(h); ok {
		t.Fatal("missing archive id ok")
	}
	h.Set(wire.HeaderArchiveID, "00000000-0000-4000-8000-00000000a001")
	st, ok := wire.ParseServerState(h)
	if !ok || st.Epoch != 3 || st.ArchiveID != "00000000-0000-4000-8000-00000000a001" || !st.ServerTime.IsZero() {
		t.Fatalf("got %+v %v", st, ok)
	}
	h.Set(wire.HeaderServerTime, "2026-11-08T05:00:00.123456789Z")
	st, ok = wire.ParseServerState(h)
	if !ok || !st.ServerTime.Equal(ts("2026-11-08T05:00:00.123456789Z")) {
		t.Fatalf("server time %v", st.ServerTime)
	}
	h.Set(wire.HeaderEpoch, "-1")
	if _, ok := wire.ParseServerState(h); ok {
		t.Fatal("negative epoch ok")
	}
}

func goodEnvelope() map[string]any {
	return map[string]any{
		"v": 1, "kind": "exchange.completion", "outbox_id": outboxA, "seq": 1, "epoch_seen": 1,
		"key": "ex/r1/completion", "mask_version": "mask/v1", "canon_version": "canon/v1", "exact_rev": 0,
		"created_at": "2026-11-08T05:00:00Z", "body": map[string]any{"request_id": "r1"},
	}
}

func encodeMap(t *testing.T, m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeEnvelope(t *testing.T) {
	if _, err := wire.DecodeEnvelope(encodeMap(t, goodEnvelope())); err != nil {
		t.Fatalf("good envelope: %v", err)
	}
	loc := map[string]any{"machine_id": "m", "file_key": "claude-projects/p/s.jsonl", "scan_gen": 0}
	imp := goodEnvelope()
	imp["kind"], imp["key"], imp["loc"] = "import.record", "cc/u/s/u/"+sum("x").String(), loc
	if _, err := wire.DecodeEnvelope(encodeMap(t, imp)); err != nil {
		t.Fatalf("import envelope: %v", err)
	}
	// Body bytes are kept exactly as sent, whitespace included.
	raw := []byte(`{"v":1,"kind":"usage.attempt","outbox_id":"` + outboxA + `","seq":2,"key":"usage/u1","body": { "a" : 1.50 }, "unknown_field":true}`)
	env, err := wire.DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(env.Body) != `{ "a" : 1.50 }` || env.BodySHA256() != sha256.Sum256([]byte(`{ "a" : 1.50 }`)) {
		t.Errorf("body not verbatim: %q", env.Body)
	}

	cases := []struct {
		name string
		edit func(m map[string]any)
		raw  string
		want error
	}{
		{name: "v2", edit: func(m map[string]any) { m["v"] = 2 }, want: wire.ErrUnsupportedProtocol},
		{name: "v2 with a v2-only shape", raw: `{"v":2,"seq":"not a number","body":[1]}`, want: wire.ErrUnsupportedProtocol},
		{name: "v0", edit: func(m map[string]any) { m["v"] = 0 }, want: wire.ErrSchema},
		{name: "v missing", edit: func(m map[string]any) { delete(m, "v") }, want: wire.ErrSchema},
		{name: "v float", edit: func(m map[string]any) { m["v"] = 1.5 }, want: wire.ErrSchema},
		{name: "v string", edit: func(m map[string]any) { m["v"] = "1" }, want: wire.ErrSchema},
		{name: "unknown kind", edit: func(m map[string]any) { m["kind"] = "exchange.other" }, want: wire.ErrSchema},
		{name: "bad key", edit: func(m map[string]any) { m["key"] = "ex/r 1" }, want: wire.ErrSchema},
		{name: "long key", edit: func(m map[string]any) { m["key"] = strings.Repeat("k", 201) }, want: wire.ErrSchema},
		{name: "seq 0", edit: func(m map[string]any) { m["seq"] = 0 }, want: wire.ErrSchema},
		{name: "seq negative", edit: func(m map[string]any) { m["seq"] = -1 }, want: wire.ErrSchema},
		{name: "outbox upper", edit: func(m map[string]any) { m["outbox_id"] = strings.ToUpper(outboxA) }, want: wire.ErrSchema},
		{name: "outbox short", edit: func(m map[string]any) { m["outbox_id"] = outboxA[:31] }, want: wire.ErrSchema},
		{name: "loc on exchange", edit: func(m map[string]any) { m["loc"] = loc }, want: wire.ErrSchema},
		{name: "body array", edit: func(m map[string]any) { m["body"] = []int{1} }, want: wire.ErrSchema},
		{name: "body null", edit: func(m map[string]any) { m["body"] = nil }, want: wire.ErrSchema},
		{name: "body missing", edit: func(m map[string]any) { delete(m, "body") }, want: wire.ErrSchema},
		{name: "bad ref", edit: func(m map[string]any) { m["refs"] = []string{"zz"} }, want: wire.ErrSchema},
		{name: "bad created_at", edit: func(m map[string]any) { m["created_at"] = "yesterday" }, want: wire.ErrSchema},
		{name: "not json", raw: `{"v":1,`, want: wire.ErrSchema},
		{name: "not an object", raw: `[1]`, want: wire.ErrSchema},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := []byte(c.raw)
			if c.edit != nil {
				m := goodEnvelope()
				c.edit(m)
				raw = encodeMap(t, m)
			}
			_, err := wire.DecodeEnvelope(raw)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	// An import kind without loc.
	imp = goodEnvelope()
	imp["kind"] = "import.file"
	if _, err := wire.DecodeEnvelope(encodeMap(t, imp)); !errors.Is(err, wire.ErrSchema) {
		t.Fatalf("import without loc: %v", err)
	}
}

func TestDecodeBody(t *testing.T) {
	for kind, want := range map[wire.Kind]any{
		wire.KindExchangeBegin: &wire.ExchangeBegin{}, wire.KindExchangeAttempt: &wire.ExchangeAttempt{},
		wire.KindExchangeResponse: &wire.ExchangeResponse{}, wire.KindExchangeDone: &wire.ExchangeCompletion{},
		wire.KindUsage: &wire.UsageAttempt{}, wire.KindImportRecord: &wire.ImportRecord{}, wire.KindImportFile: &wire.ImportFile{},
	} {
		got, err := wire.DecodeBody(kind, json.RawMessage(`{"unknown_field":1}`))
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		if reflect.TypeOf(got) != reflect.TypeOf(want) {
			t.Errorf("%s decodes to %T", kind, got)
		}
	}
	for _, c := range []struct {
		kind wire.Kind
		body string
	}{
		{"nope", `{}`},
		{wire.KindUsage, `[]`},
		{wire.KindUsage, `null`},
		{wire.KindUsage, `{"latency_ms":"slow"}`},
		{wire.KindImportFile, `{"content":"not hex"}`},
		{wire.KindExchangeBegin, `{"observed_at":"never"}`},
		{wire.KindExchangeBegin, `{"body":{"list_sha":"00"}}`},
	} {
		if _, err := wire.DecodeBody(c.kind, json.RawMessage(c.body)); !errors.Is(err, wire.ErrSchema) {
			t.Errorf("DecodeBody(%s, %s) err = %v", c.kind, c.body, err)
		}
	}
}

func TestDecodeTombstone(t *testing.T) {
	good := tombstoneFixture()
	for _, c := range []struct {
		name string
		edit func(*wire.Tombstone)
		want error
	}{
		{"v2", func(x *wire.Tombstone) { x.V = 2 }, wire.ErrUnsupportedProtocol},
		{"v0", func(x *wire.Tombstone) { x.V = 0 }, wire.ErrSchema},
		{"seq0", func(x *wire.Tombstone) { x.Seq = 0 }, wire.ErrSchema},
		{"outbox", func(x *wire.Tombstone) { x.OutboxID = "x" }, wire.ErrSchema},
		{"key", func(x *wire.Tombstone) { x.Key = "" }, wire.ErrSchema},
		{"reason", func(x *wire.Tombstone) { x.Reason = "conflict" }, wire.ErrSchema},
	} {
		x := good
		c.edit(&x)
		b, _ := json.Marshal(x)
		if _, err := wire.DecodeTombstone(b); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	for _, r := range []string{wire.TombRejectedLocal, wire.TombRejectedTooLarge, wire.TombMissingRef} {
		x := good
		x.Reason = r
		b, _ := json.Marshal(x)
		if _, err := wire.DecodeTombstone(b); err != nil {
			t.Errorf("reason %s: %v", r, err)
		}
	}
	if _, err := wire.DecodeTombstone([]byte(`nope`)); !errors.Is(err, wire.ErrSchema) {
		t.Errorf("bad json: %v", err)
	}
}

func TestHeartbeatRow(t *testing.T) {
	hb := heartbeatFixture()
	r := hb.Row()
	at := ts("2026-11-08T05:10:00.5Z")
	if r.EpochSeen != 1 || r.ComponentVersion != "cpa-plugin/0.1.0" || r.HostVersion != "schema:10" || r.BootID != hb.BootID {
		t.Errorf("scalars: %+v", r)
	}
	if !r.LastHookAt.Equal(at.Add(-time.Second)) || !r.LastUsageAt.Equal(at.Add(-time.Second)) {
		t.Errorf("times: %v %v", r.LastHookAt, r.LastUsageAt)
	}
	if r.OutboxDepth != 12 || r.OutboxBytes != 34567 || r.OldestPendingS != 9 {
		t.Errorf("outbox: %+v", r)
	}
	want := map[string]uint64{
		"calls.before_auth": 100, "calls.usage": 98, "hook_errors.response": 1, "dropped.body_budget": 2,
		"outbox.dead": 1, "uploader.consecutive_failures": 0,
	}
	if len(r.Counters) != len(want) {
		t.Errorf("counters %v", r.Counters)
	}
	for k, v := range want {
		if got, ok := r.Counters[k]; !ok || got != v {
			t.Errorf("counter %s = %d (%v), want %d", k, got, ok, v)
		}
	}

	var empty wire.Heartbeat
	empty.Outbox.OldestPendingAgeS = -5
	empty.Outbox.Pending = math.MaxUint32 + 10
	empty.Uploader.EpochSeen = math.MaxUint64
	r = empty.Row()
	if !r.LastHookAt.Equal(time.Unix(0, 0)) || !r.LastUsageAt.Equal(time.Unix(0, 0)) {
		t.Errorf("empty times %v %v", r.LastHookAt, r.LastUsageAt)
	}
	if r.OldestPendingS != 0 || r.OutboxDepth != math.MaxUint32 || r.EpochSeen != math.MaxUint32 {
		t.Errorf("saturation %+v", r)
	}
	if r.ComponentVersion != "/" || len(r.Counters) != 2 {
		t.Errorf("empty row %+v", r)
	}
}

func TestWriteCAB1Rejects(t *testing.T) {
	for name, b := range map[string]wire.Batch{
		"bad kind":     {Pieces: []wire.BatchPiece{{Kind: 3}}},
		"ct on json":   {Pieces: []wire.BatchPiece{{Kind: wire.PieceJSON, ContentType: "application/json"}}},
		"ct too long":  {Pieces: []wire.BatchPiece{{Kind: wire.PieceAttachment, ContentType: strings.Repeat("a", 256)}}},
		"big tombsone": {Tombstones: [][]byte{bytes.Repeat([]byte(" "), wire.MaxTombstoneJSON+1)}},
	} {
		if err := wire.WriteCAB1(io.Discard, b); err == nil {
			t.Errorf("%s: WriteCAB1 accepted", name)
		}
	}
}

// TestCAB1Size: CAB1Size is exactly what WriteCAB1 writes, for an empty batch and for one with every
// record type.
func TestCAB1Size(t *testing.T) {
	for name, b := range map[string]wire.Batch{
		"empty": {},
		"all": {
			Pieces: []wire.BatchPiece{
				newPiece(wire.PieceJSON, "", []byte(`{"synthetic":true}`)),
				newPiece(wire.PieceText, "", []byte("synthetic text")),
				newPiece(wire.PieceAttachment, "image/png", bytes.Repeat([]byte{0x89}, 3000)),
				newPiece(wire.PieceText, "", nil),
			},
			Envelopes:  [][]byte{[]byte(`{"v":1}`), {}},
			Tombstones: [][]byte{[]byte(`{"seq":7}`)},
		},
	} {
		var buf bytes.Buffer
		if err := wire.WriteCAB1(&buf, b); err != nil {
			t.Fatal(err)
		}
		if got := wire.CAB1Size(b); got != buf.Len() {
			t.Errorf("%s: CAB1Size = %d, WriteCAB1 wrote %d", name, got, buf.Len())
		}
	}
}

func TestReadCAB1LargeRecordAndReaders(t *testing.T) {
	// A piece bigger than the 1 MiB read chunk, through a reader that returns one byte at a time.
	big := newPiece(wire.PieceAttachment, "application/octet-stream", bytes.Repeat([]byte("synthetic"), 300_000))
	stream := writeBatch(t, wire.Batch{Pieces: []wire.BatchPiece{big}, Envelopes: [][]byte{[]byte(`{}`)}})
	capFor := int64(len(stream) + 2*wire.CAB1RecordOverhead) // two records, each charged the overhead
	got, err := wire.ReadCAB1(iotest.OneByteReader(bytes.NewReader(stream)), capFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pieces) != 1 || !bytes.Equal(got.Pieces[0].Raw, big.Raw) || got.Pieces[0].ContentType != big.ContentType {
		t.Fatalf("big piece did not round-trip")
	}
	// Truncated inside the big record: framing.
	if _, err := wire.ReadCAB1(bytes.NewReader(stream[:2<<20]), capFor); !errors.Is(err, wire.ErrCAB1Framing) {
		t.Fatalf("truncated big record: %v", err)
	}
	// Read errors pass through unchanged.
	boom := errors.New("synthetic read error")
	if _, err := wire.ReadCAB1(io.MultiReader(bytes.NewReader(stream[:100]), iotest.ErrReader(boom)), 1<<30); !errors.Is(err, boom) {
		t.Fatalf("read error: %v", err)
	}
	if _, err := wire.ReadCAB1(iotest.ErrReader(boom), 1<<30); !errors.Is(err, boom) {
		t.Fatalf("read error on magic: %v", err)
	}
	// Cap of zero.
	if _, err := wire.ReadCAB1(bytes.NewReader(stream), 0); !errors.Is(err, wire.ErrCAB1TooLarge) {
		t.Fatalf("zero cap: %v", err)
	}
	// Empty input.
	if _, err := wire.ReadCAB1(bytes.NewReader(nil), 100); !errors.Is(err, wire.ErrCAB1Framing) {
		t.Fatalf("empty input: %v", err)
	}
	// Exactly at the cap is fine; one byte less is too large.
	if _, err := wire.ReadCAB1(bytes.NewReader(stream), capFor); err != nil {
		t.Fatalf("at cap: %v", err)
	}
	if _, err := wire.ReadCAB1(bytes.NewReader(stream), capFor-1); !errors.Is(err, wire.ErrCAB1TooLarge) {
		t.Fatalf("one below cap: %v", err)
	}
}

// TestReadCAB1RecordOverhead: a stream of tiny records cannot decode to many times maxBytes; each
// record is charged CAB1RecordOverhead (the zstd-bomb shape of zero-length envelope records).
func TestReadCAB1RecordOverhead(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(wire.CAB1Magic)
	rec := []byte{wire.RecEnvelope, 0, 0, 0, 0}
	for i := 0; i < 1_000_000; i++ {
		b.Write(rec)
	}
	b.Write([]byte{wire.RecEnd, 0, 0, 0, 0})
	const maxBytes = 32 << 20
	if int64(b.Len()) > maxBytes {
		t.Fatal("stream must fit the cap by its bytes")
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := wire.ReadCAB1(bytes.NewReader(b.Bytes()), maxBytes)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, wire.ErrCAB1TooLarge) {
		t.Fatalf("err = %v, want ErrCAB1TooLarge", err)
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d-byte stream of empty records: %.1f MiB allocated before ErrCAB1TooLarge", b.Len(), float64(alloc)/(1<<20))
	if alloc > 2*maxBytes {
		t.Errorf("decoding allocated %d MiB for a %d MiB cap", alloc>>20, maxBytes>>20)
	}
}

func TestStatusAndReasonValues(t *testing.T) {
	// The enum values are the protocol's wire strings, and a result marshals reason and missing only when set.
	b, _ := json.Marshal(wire.CommitResult{Seq: 3, Status: wire.StatusOK})
	if string(b) != `{"seq":3,"status":"ok"}` {
		t.Errorf("ok result %s", b)
	}
	h := sum("missing")
	b, _ = json.Marshal(wire.CommitResult{Seq: 4, Status: wire.StatusRetry, Reason: wire.ReasonMissingRef, Missing: []wire.Hash{h}})
	if string(b) != `{"seq":4,"status":"retry","reason":"missing_ref","missing":["`+h.String()+`"]}` {
		t.Errorf("retry result %s", b)
	}
	for s, want := range map[wire.Status]string{wire.StatusOK: "ok", wire.StatusDuplicate: "duplicate",
		wire.StatusConflict: "conflict", wire.StatusRejected: "rejected", wire.StatusRetry: "retry"} {
		if string(s) != want {
			t.Errorf("status %s", s)
		}
	}
	b, _ = json.Marshal(wire.SourcesSelf{Accepted: wire.Accepted{Mask: map[string]wire.MaskAcceptance{wire.MaskV1: {}}}})
	if !bytes.Contains(b, []byte(`"mask":{"mask/v1":{"created_before":null}}`)) {
		t.Errorf("created_before must marshal as null: %s", b)
	}
}
