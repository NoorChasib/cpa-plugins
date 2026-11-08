package wire_test

// Golden fixtures for protocol v1 in testdata/v1/, which the server's CI runs too. Every value here
// is synthetic. The files are the definition; the Go values below
// reproduce them and are what `go test ./pkg/wire -run Golden -update` writes.
//
//	testdata/v1/envelopes/*.json         one canon/v1 envelope per file, every kind
//	testdata/v1/envelopes/manifest.json  per file: kind, key, body_sha256
//	testdata/v1/tombstone.json           one canon/v1 tombstone
//	testdata/v1/heartbeat.json           one canon/v1 heartbeat
//	testdata/v1/cab1/*.cab1              uncompressed CAB1 streams, valid and invalid
//	testdata/v1/cab1/cases.json          per file: max_bytes and the expected decode (counts, or the error class)

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/v1")

const goldenDir = "testdata/v1"

// canonJSON encodes v the way canon/v1 does for the ASCII-only values used in these fixtures: object
// keys sorted, no whitespace, no HTML escaping, number literals verbatim. pkg/wire's tests do not
// import pkg/canon (canon imports wire, and wire must build and test on its own);
// pkg/client's TestWireGoldensAreCanon cross-checks these bytes against canon.
func canonJSON(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func sum(s string) wire.Hash    { return sha256.Sum256([]byte(s)) }
func hp(h wire.Hash) *wire.Hash { return &h }
func tp(t time.Time) *time.Time { return &t }
func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// itemID reproduces canon.ItemID for these fixtures: sha256(canon({"f":F,"m":M,"p":hex(P)})).
func itemID(t testing.TB, it wire.Item) wire.Hash {
	m := map[string]any{}
	if it.F != nil {
		m["f"] = it.F
	}
	if it.M != nil {
		m["m"] = it.M
	}
	if it.P != nil {
		m["p"] = it.P.String()
	}
	return sha256.Sum256(canonJSON(t, m))
}

func itemIDs(t testing.TB, items []wire.Item) []wire.Hash {
	ids := make([]wire.Hash, len(items))
	for i, it := range items {
		ids[i] = itemID(t, it)
	}
	return ids
}

func listSHA(ids []wire.Hash) wire.Hash {
	var b []byte
	for _, id := range ids {
		b = append(b, id[:]...)
	}
	return sha256.Sum256(b)
}

// Synthetic pieces shared by the envelope and CAB1 fixtures.
var (
	pieceSkeleton = newPiece(wire.PieceJSON, "", []byte(`{"max_tokens":1024,"model":"claude-test-1","stream":true}`))
	pieceTools    = newPiece(wire.PieceJSON, "", []byte(`[{"input_schema":{"type":"object"},"name":"test_tool"}]`))
	pieceSystem   = newPiece(wire.PieceText, "", []byte("You are a synthetic test assistant."))
	pieceUser     = newPiece(wire.PieceText, "", []byte("synthetic user text 0001"))
	pieceImage    = newPiece(wire.PieceAttachment, "image/png", syntheticBytes(256))
	pieceAnswer   = newPiece(wire.PieceText, "", []byte("synthetic assistant text 0001"))
	pieceFailure  = newPiece(wire.PieceText, "", []byte(`{"error":"synthetic overloaded"}`))
	pieceRecord   = newPiece(wire.PieceText, "", []byte("synthetic transcript text 0001"))
	pieceExtra    = newPiece(wire.PieceJSON, "", []byte(`{"cwd":"/synthetic/project","version":"0.0.0-test"}`))
	pieceFile     = newPiece(wire.PieceText, "", []byte("synthetic saved tool output 0001\n"))
)

func newPiece(kind wire.PieceKind, ct string, raw []byte) wire.BatchPiece {
	return wire.BatchPiece{Hash: sha256.Sum256(raw), Kind: kind, ContentType: ct, Raw: raw}
}

func syntheticBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

const (
	outboxA   = "0123456789abcdef0123456789abcdef"
	outboxB   = "fedcba9876543210fedcba9876543210"
	requestID = "req-test-0001"
	sessionID = "00000000-0000-4000-8000-000000000001"
	recordID  = "00000000-0000-4000-8000-0000000000aa"
)

type envFixture struct {
	name string // file name under envelopes/
	env  wire.Envelope
	body any // pointer to the kind's struct, as DecodeBody returns it
}

func envelopeFixtures(t testing.TB) []envFixture {
	created := ts("2026-11-08T05:00:00.123456789Z")
	imageFrame := json.RawMessage(`{"source":{"data":{"$att":{"enc":"base64","len":256,"mime":"image/png","sha256":"` +
		pieceImage.Hash.String() + `"}},"media_type":"image/png","type":"base64"},"type":"image"}`)
	beginItems := []wire.Item{
		{M: json.RawMessage(`{"role":"user"}`)},
		{F: json.RawMessage(`{"type":"text"}`), P: hp(pieceUser.Hash), V: json.RawMessage(`{"cache_control":{"type":"ephemeral"}}`)},
		{F: imageFrame},
	}
	beginIDs := itemIDs(t, beginItems)
	generate := true
	begin := &wire.ExchangeBegin{
		RequestID:      requestID,
		InputSeq:       0,
		TraceID:        "trace-test-0001",
		ObservedAt:     created,
		SourceFormat:   "claude",
		RequestPath:    "/v1/messages",
		Kind:           "chat",
		Transport:      "http",
		RequestedModel: "claude-test-1",
		Model:          "claude-test-1",
		Stream:         true,
		CallerFP:       sum("caller-test").String(),
		MetaSource:     "plugin_host_model_callback",
		ClientEffort:   "high",
		Generate:       &generate,
		IdempotencyFP:  "k1:" + sum("idempotency-test").String(),
		Hints:          wire.ArchiveHints{Machine: "test-host", Origin: "test-origin", Canary: "canary-test-0001", Unknown: 1},
		Headers: []wire.Header{
			{Name: "anthropic-version", Values: []string{"2023-06-01"}},
			{Name: "chatgpt-account-id", FP: "k1:" + sum("account-test").String()},
			{Name: "x-codex-turn-state", SHA256: hp(sum("turn-state-test")), Len: 40},
		},
		Body: wire.Body{
			Format:   "claude.messages",
			RawLen:   2345,
			Skeleton: hp(pieceSkeleton.Hash),
			Tools:    hp(pieceTools.Hash),
			ToolsV:   json.RawMessage(`[{"i":0,"v":{"cache_control":{"type":"ephemeral"}}}]`),
			System:   []wire.Item{{F: json.RawMessage(`{"type":"text"}`), P: hp(pieceSystem.Hash)}},
			Items:    beginItems,
			N:        uint32(len(beginItems)),
			ListSHA:  listSHA(beginIDs),
			Flags:    []string{wire.FlagDupKeys},
		},
	}
	beginRefs := []wire.Hash{pieceSkeleton.Hash, pieceTools.Hash, pieceSystem.Hash, pieceUser.Hash, pieceImage.Hash}

	answerItems := []wire.Item{
		{M: json.RawMessage(`{"role":"assistant"}`)},
		{F: json.RawMessage(`{"type":"text"}`), P: hp(pieceAnswer.Hash)},
	}
	deltaItems := []wire.Item{
		{M: json.RawMessage(`{"role":"user"}`)},
		{F: json.RawMessage(`{"type":"text"}`), P: hp(pieceRecord.Hash)},
	}
	var fullIDs []wire.Hash
	fullIDs = append(fullIDs, beginIDs...)
	fullIDs = append(fullIDs, itemIDs(t, answerItems)...)
	fullIDs = append(fullIDs, itemIDs(t, deltaItems)...)
	delta := &wire.ExchangeBegin{
		RequestID:      "req-test-0002",
		InputSeq:       0,
		ObservedAt:     created.Add(time.Minute),
		SourceFormat:   "claude",
		RequestPath:    "/v1/messages",
		Kind:           "chat",
		Transport:      "http",
		RequestedModel: "claude-test-1",
		Model:          "claude-test-1",
		Stream:         true,
		Hints:          wire.ArchiveHints{},
		Headers:        []wire.Header{},
		Body: wire.Body{
			Format:   "claude.messages",
			RawLen:   3456,
			Skeleton: hp(pieceSkeleton.Hash),
			Tools:    hp(pieceTools.Hash),
			System:   []wire.Item{{F: json.RawMessage(`{"type":"text"}`), P: hp(pieceSystem.Hash)}},
			Base:     &wire.Base{Key: wire.ExchangeBeginKey(requestID, 0), Resp: 0, Kept: uint32(len(beginIDs) + len(answerItems))},
			Items:    deltaItems,
			N:        uint32(len(fullIDs)),
			ListSHA:  listSHA(fullIDs),
		},
	}

	attempt := &wire.ExchangeAttempt{
		RequestID:          requestID,
		Ordinal:            1,
		ObservedAt:         created.Add(5 * time.Millisecond),
		AuthIndex:          "3",
		AuthFP:             "k1:" + sum("auth-id-test").String(),
		ToFormat:           "claude",
		Model:              "claude-test-1",
		CanonicalSessionID: "claude:" + sessionID,
		SelectionID:        "sel-test-0001",
		NodeKind:           "main",
	}

	response := &wire.ExchangeResponse{
		RequestID:      requestID,
		Index:          0,
		ResponseID:     "msg_test0001",
		ObservedAt:     created.Add(2 * time.Second),
		HeaderInitAt:   tp(created.Add(300 * time.Millisecond)),
		FirstChunkAt:   tp(created.Add(400 * time.Millisecond)),
		TerminalAt:     tp(created.Add(2 * time.Second)),
		StatusCode:     200,
		Stream:         true,
		TerminalStatus: "completed",
		Chunks:         12,
		StreamBytes:    4567,
		Events:         []wire.StreamEvent{{Type: "interrupt", At: created.Add(time.Second)}},
		Headers: []wire.Header{
			{Name: "content-type", Values: []string{"text/event-stream"}},
			{Name: "request-id", Values: []string{"req_upstream_test"}},
		},
		Body: wire.Body{
			Format:  "claude.messages",
			RawLen:  678,
			Items:   answerItems,
			N:       uint32(len(answerItems)),
			ListSHA: listSHA(itemIDs(t, answerItems)),
		},
	}

	completion := &wire.ExchangeCompletion{
		RequestID:      requestID,
		TraceID:        "trace-test-0001",
		SourceFormat:   "claude",
		Model:          "claude-test-1",
		RequestedModel: "claude-test-1",
		Stream:         true,
		Outcome:        "succeeded",
		StatusCode:     200,
		StartedAt:      created,
		CompletedAt:    created.Add(2 * time.Second),
		BeginWritten:   true,
		Responses:      1,
		HookMicros:     map[string]uint32{"request.intercept_before": 120, "response.stream_chunk": 15},
	}

	usage := &wire.UsageAttempt{
		UsageRequestID:  "usage-test-0001",
		TraceID:         "trace-test-0001",
		SelectionID:     "sel-test-0001",
		Provider:        "claude",
		BaseURL:         "https://api.example.test/v1",
		ExecutorType:    "claude",
		Model:           "claude-test-1",
		CallerFP:        sum("caller-test").String(),
		SessionID:       "claude:" + sessionID,
		AuthIndex:       "3",
		AuthFP:          "k1:" + sum("auth-id-test").String(),
		AuthType:        "oauth",
		AccountFP:       "k1:" + sum("account-test").String(),
		SourceKind:      "hashed",
		AccountLabel:    "test-account",
		ServiceTier:     "standard",
		Generate:        true,
		Stream:          true,
		RequestedAt:     created,
		LatencyMS:       2000,
		TTFTMS:          400,
		Failed:          true,
		FailureStatus:   529,
		FailureBody:     hp(pieceFailure.Hash),
		Tokens:          wire.Tokens{Input: 100, Output: 20, Reasoning: 5, CacheRead: 80, CacheCreation: 10, Total: 120},
		ResponseHeaders: []wire.Header{{Name: "retry-after", Values: []string{"5"}}},
	}

	recFrame := json.RawMessage(`{"isSidechain":false,"type":"assistant","uuid":"` + recordID + `"}`)
	recItems := []wire.Item{
		{M: json.RawMessage(`{"role":"assistant"}`)},
		{F: json.RawMessage(`{"type":"text"}`), P: hp(pieceRecord.Hash)},
	}
	record := &wire.ImportRecord{
		Client:     "claude-code",
		FileRole:   "main",
		SessionID:  sessionID,
		RecordUUID: recordID,
		RecordType: "assistant",
		RecordTS:   tp(created),
		Mode:       "full",
		Frame:      recFrame,
		Items:      recItems,
		Extra:      hp(pieceExtra.Hash),
	}
	recKey := wire.ClaudeUUIDKey(sessionID, recordID, wire.RecordKeyHash(recFrame, itemIDs(t, recItems), record.Extra))
	transcriptKey := "claude-projects/-synthetic-project/" + sessionID + ".jsonl"

	file := &wire.ImportFile{
		Client:  "claude-code",
		FileKey: "claude-projects/-synthetic-project/" + sessionID + "/tool-results/toolu_test.txt",
		Content: pieceFile.Hash,
		Kind:    "tool_result_file",
	}

	base := func(kind wire.Kind, seq uint64, key string, refs []wire.Hash, body any) wire.Envelope {
		return wire.Envelope{
			V: wire.ProtocolV1, Kind: kind, OutboxID: outboxA, Seq: seq, EpochSeen: 1, Key: key,
			MaskVersion: wire.MaskV1, CanonVersion: wire.CanonV1, ExactRev: 2, CreatedAt: created,
			Refs: refs, Body: canonJSON(t, body),
		}
	}
	recA := base(wire.KindImportRecord, 1, recKey, []wire.Hash{pieceRecord.Hash, pieceExtra.Hash}, record)
	recA.OutboxID = outboxB
	recA.Loc = &wire.Locator{MachineID: "test-machine-a", FileKey: transcriptKey, ScanGen: 0, LineOrdinal: 7, ByteOffset: 4096, ByteLen: 512, EndOffset: 4608}
	recB := recA
	recB.Seq, recB.EpochSeen, recB.ExactRev = 40, 2, 3
	recB.Loc = &wire.Locator{MachineID: "test-machine-b", FileKey: transcriptKey, ScanGen: 1, LineOrdinal: 3, ByteOffset: 1024, ByteLen: 512, EndOffset: 1536}
	fileEnv := base(wire.KindImportFile, 2, wire.ImportFileKey(file.Client, file.FileKey, file.Content), []wire.Hash{pieceFile.Hash}, file)
	fileEnv.OutboxID = outboxB
	fileEnv.Loc = &wire.Locator{MachineID: "test-machine-a", FileKey: file.FileKey, Size: int64(len(pieceFile.Raw)), ModTime: tp(created.Add(-time.Hour))}

	return []envFixture{
		{"exchange.begin.json", base(wire.KindExchangeBegin, 1, wire.ExchangeBeginKey(requestID, 0), beginRefs, begin), begin},
		{"exchange.begin.delta.json", base(wire.KindExchangeBegin, 5, wire.ExchangeBeginKey("req-test-0002", 0),
			[]wire.Hash{pieceSkeleton.Hash, pieceTools.Hash, pieceSystem.Hash, pieceRecord.Hash}, delta), delta},
		{"exchange.attempt.json", base(wire.KindExchangeAttempt, 2, wire.ExchangeAttemptKey(requestID, 1), nil, attempt), attempt},
		{"exchange.response.json", base(wire.KindExchangeResponse, 3, wire.ExchangeResponseKey(requestID, 0), []wire.Hash{pieceAnswer.Hash}, response), response},
		{"exchange.completion.json", base(wire.KindExchangeDone, 4, wire.ExchangeCompletionKey(requestID), nil, completion), completion},
		{"usage.attempt.json", base(wire.KindUsage, 6, wire.UsageKey(usage.UsageRequestID), []wire.Hash{pieceFailure.Hash}, usage), usage},
		{"import.record.json", recA, record},
		{"import.record.relocated.json", recB, record},
		{"import.file.json", fileEnv, file},
	}
}

func tombstoneFixture() wire.Tombstone {
	return wire.Tombstone{V: wire.ProtocolV1, OutboxID: outboxA, Seq: 7, Key: wire.ExchangeBeginKey("req-test-0003", 0), Reason: wire.TombRejectedLocal}
}

func heartbeatFixture() wire.Heartbeat {
	at := ts("2026-11-08T05:10:00.5Z")
	return wire.Heartbeat{
		V: wire.ProtocolV1, OutboxID: outboxA, BootID: "00112233445566778899aabbccddeeff", N: 42,
		SentAt: at, StartedAt: at.Add(-42 * time.Minute), Component: "cpa-plugin", Version: "0.1.0",
		State: "running", MaskVersion: wire.MaskV1, RulesetSHA: sum("ruleset-test").String(), ExactRev: 2,
		HostVersion: "schema:10",
		LastHookAt: map[string]time.Time{
			"before_auth": at.Add(-3 * time.Second), "usage": at.Add(-time.Second), "stream_chunk": at.Add(-2 * time.Second),
		},
		LastUsageAt: tp(at.Add(-time.Second)),
		Calls:       map[string]uint64{"before_auth": 100, "usage": 98},
		HookErrors:  map[string]uint64{"response": 1},
		Panics:      map[string]uint64{},
		Dropped:     map[string]uint64{"body_budget": 2},
		HookP50us:   map[string]uint32{"before_auth": 40},
		HookP99us:   map[string]uint32{"before_auth": 900},
		Outbox: wire.OutboxStats{Pending: 12, PendingBytes: 34567, Dead: 1, AckedRetained: 500, AckedRetainedBytes: 1 << 20,
			OldestPendingAgeS: 9, DiskBytes: 2 << 20, CapBytes: 20 << 30, FreeBytes: 100 << 30, Pressure: 0,
			ReplayWindowH: 168, NextSeq: 613, HW: 600, LastAckAt: at.Add(-5 * time.Second)},
		Uploader: wire.UploaderStats{LastSuccessAt: at.Add(-5 * time.Second), LastErrorClass: "none", ConsecutiveFailures: 0,
			EpochSeen: 1, ArchiveID: "00000000-0000-4000-8000-00000000a001"},
		Runtime: wire.RuntimeStats{HeapInuse: 8 << 20, Sys: 32 << 20, MemLimit: 512 << 20, Goroutines: 14, GCCycles: 30},
		Masker: wire.MaskerStats{Bodies: 200, Bytes: 3 << 20, MemoHits: 150, MemoMisses: 50, P50MS: 1.5, P99MS: 20.25,
			MatchesByRule: map[string]uint64{"ca-archive-token": 1}},
		Faults: []string{},
	}
}

// cab1Case is one entry of testdata/v1/cab1/cases.json.
type cab1Case struct {
	MaxBytes   int64  `json:"max_bytes"`
	Error      string `json:"error,omitempty"` // "framing" | "too_large"; empty = decodes
	Pieces     int    `json:"pieces"`
	Envelopes  int    `json:"envelopes"`
	Tombstones int    `json:"tombstones"`
	Note       string `json:"note"`
}

func cab1Rec(typ byte, body []byte) []byte {
	out := []byte{typ, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(body)))
	return append(out, body...)
}

func cab1PieceRec(h wire.Hash, kind byte, ct string, raw []byte) []byte {
	body := append([]byte{}, h[:]...)
	body = append(body, kind, byte(len(ct)))
	body = append(body, ct...)
	body = append(body, raw...)
	return cab1Rec(wire.RecPiece, body)
}

func writeBatch(t testing.TB, b wire.Batch) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := wire.WriteCAB1(&buf, b); err != nil {
		t.Fatalf("WriteCAB1: %v", err)
	}
	return buf.Bytes()
}

func validBatch(t testing.TB) wire.Batch {
	envs := envelopeFixtures(t)
	return wire.Batch{
		Pieces:     []wire.BatchPiece{pieceSkeleton, pieceTools, pieceSystem, pieceUser, pieceImage, pieceFailure},
		Envelopes:  [][]byte{canonJSON(t, envs[0].env), canonJSON(t, envs[5].env)},
		Tombstones: [][]byte{canonJSON(t, tombstoneFixture())},
	}
}

// cab1Fixtures returns the CAB1 golden streams and their expected decode.
func cab1Fixtures(t testing.TB) (map[string][]byte, map[string]cab1Case) {
	tomb := canonJSON(t, tombstoneFixture())
	validBytes := writeBatch(t, validBatch(t))
	end := cab1Rec(wire.RecEnd, nil)
	magic := []byte(wire.CAB1Magic)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	ctOverrun := sum("x")
	const max = 1 << 20

	files := map[string][]byte{
		"valid.cab1":               validBytes,
		"valid-over-cap.cab1":      validBytes,
		"empty.cab1":               cat(magic, end),
		"tombstone-only.cab1":      writeBatch(t, wire.Batch{Tombstones: [][]byte{tomb}}),
		"bad-magic.cab1":           cat([]byte("CAB2"), end),
		"unknown-type.cab1":        cat(magic, cab1Rec(4, []byte(`{}`)), end),
		"truncated.cab1":           validBytes[:len(validBytes)/2],
		"truncated-header.cab1":    cat(magic, []byte{wire.RecEnvelope, 0, 0}),
		"missing-end.cab1":         validBytes[:len(validBytes)-len(end)],
		"trailing.cab1":            cat(validBytes, []byte{0}),
		"end-nonzero.cab1":         cat(magic, cab1Rec(wire.RecEnd, []byte{0})),
		"tombstone-1025.cab1":      cat(magic, cab1Rec(wire.RecTombstone, bytes.Repeat([]byte(" "), wire.MaxTombstoneJSON+1)), end),
		"hash-mismatch.cab1":       cat(magic, cab1PieceRec(sum("not the bytes"), byte(wire.PieceText), "", []byte("synthetic")), end),
		"bad-piece-kind.cab1":      cat(magic, cab1PieceRec(sum("synthetic"), 3, "", []byte("synthetic")), end),
		"ct-on-text.cab1":          cat(magic, cab1PieceRec(sum("synthetic"), byte(wire.PieceText), "text/plain", []byte("synthetic")), end),
		"short-piece.cab1":         cat(magic, cab1Rec(wire.RecPiece, make([]byte, 10)), end),
		"ct-overrun.cab1":          cat(magic, cab1Rec(wire.RecPiece, append(ctOverrun[:], byte(wire.PieceAttachment), 200)), end),
		"huge-length.cab1":         cat(magic, []byte{wire.RecEnvelope, 0xff, 0xff, 0xff, 0xff}, []byte(`{}`)),
		"valid-at-cap.cab1":        validBytes,
		"zero-length-records.cab1": cat(magic, bytes.Repeat(cab1Rec(wire.RecEnvelope, nil), 1000), end),
	}
	cases := map[string]cab1Case{
		"valid.cab1":            {MaxBytes: max, Pieces: 6, Envelopes: 2, Tombstones: 1, Note: "pieces of every kind (an attachment with a content type), exchange.begin and usage.attempt envelopes, one tombstone"},
		"valid-over-cap.cab1":   {MaxBytes: int64(len(validBytes) + 9*wire.CAB1RecordOverhead - 1), Error: "too_large", Note: "valid.cab1 with max_bytes one below its length plus 128 bytes for each of its 9 records"},
		"empty.cab1":            {MaxBytes: max, Note: "magic and end record only"},
		"tombstone-only.cab1":   {MaxBytes: max, Tombstones: 1, Note: "a batch of one tombstone"},
		"bad-magic.cab1":        {MaxBytes: max, Error: "framing", Note: "magic CAB2"},
		"unknown-type.cab1":     {MaxBytes: max, Error: "framing", Note: "record type 4 is not defined in v1"},
		"truncated.cab1":        {MaxBytes: max, Error: "framing", Note: "valid.cab1 cut in half"},
		"truncated-header.cab1": {MaxBytes: max, Error: "framing", Note: "record header of 3 bytes"},
		"missing-end.cab1":      {MaxBytes: max, Error: "framing", Note: "valid.cab1 without its end record"},
		"trailing.cab1":         {MaxBytes: max, Error: "framing", Note: "one byte after the end record"},
		"end-nonzero.cab1":      {MaxBytes: max, Error: "framing", Note: "end record with length 1"},
		"tombstone-1025.cab1":   {MaxBytes: max, Error: "framing", Note: "tombstone record over 1 KiB"},
		"hash-mismatch.cab1":    {MaxBytes: max, Error: "framing", Note: "piece bytes whose sha256 is not the record hash"},
		"bad-piece-kind.cab1":   {MaxBytes: max, Error: "framing", Note: "piece kind 3"},
		"ct-on-text.cab1":       {MaxBytes: max, Error: "framing", Note: "content type on a text piece"},
		"short-piece.cab1":      {MaxBytes: max, Error: "framing", Note: "piece record shorter than hash, kind and content-type length"},
		"ct-overrun.cab1":       {MaxBytes: max, Error: "framing", Note: "content-type length past the record end"},
		"huge-length.cab1":      {MaxBytes: max, Error: "too_large", Note: "record declaring 4 GiB - 1 bytes, past the cap"},
		"valid-at-cap.cab1": {MaxBytes: int64(len(validBytes) + 9*wire.CAB1RecordOverhead), Pieces: 6, Envelopes: 2, Tombstones: 1,
			Note: "valid.cab1 with max_bytes exactly its length plus 128 bytes for each of its 9 records"},
		"zero-length-records.cab1": {MaxBytes: 64 << 10, Error: "too_large",
			Note: "1000 zero-length envelope records: about 5 KB of stream, past 64 KiB once each record is charged 128 bytes of decoded overhead"},
	}
	return files, cases
}

func writeOrCompare(t *testing.T, path string, want []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the fixture values (run with -update after an intended change)", path)
	}
}

type manifestEntry struct {
	Kind       wire.Kind `json:"kind"`
	Key        string    `json:"key"`
	BodySHA256 wire.Hash `json:"body_sha256"`
}

func readGolden(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{goldenDir}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenEnvelopes(t *testing.T) {
	fx := envelopeFixtures(t)
	manifest := map[string]manifestEntry{}
	for _, f := range fx {
		writeOrCompare(t, filepath.Join(goldenDir, "envelopes", f.name), canonJSON(t, f.env))
		manifest[f.name] = manifestEntry{Kind: f.env.Kind, Key: f.env.Key, BodySHA256: sha256.Sum256(f.env.Body)}
	}
	writeOrCompare(t, filepath.Join(goldenDir, "envelopes", "manifest.json"), canonJSON(t, manifest))

	// Decode every file through the v1 decoder: envelope checks, the kind's body struct, and the key
	// and body_sha256 the manifest records.
	var onDisk map[string]manifestEntry
	if err := json.Unmarshal(readGolden(t, "envelopes", "manifest.json"), &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != len(fx) {
		t.Fatalf("manifest has %d entries, want %d", len(onDisk), len(fx))
	}
	kinds := map[wire.Kind]bool{}
	for _, f := range fx {
		t.Run(f.name, func(t *testing.T) {
			env, err := wire.DecodeEnvelope(readGolden(t, "envelopes", f.name))
			if err != nil {
				t.Fatalf("DecodeEnvelope: %v", err)
			}
			m := onDisk[f.name]
			if env.Kind != m.Kind || env.Key != m.Key || env.BodySHA256() != m.BodySHA256 {
				t.Errorf("manifest mismatch: got %s %s %s", env.Kind, env.Key, env.BodySHA256())
			}
			body, err := wire.DecodeBody(env.Kind, env.Body)
			if err != nil {
				t.Fatalf("DecodeBody: %v", err)
			}
			if !reflect.DeepEqual(body, f.body) {
				t.Errorf("decoded body differs:\n got %#v\nwant %#v", body, f.body)
			}
			if !reflect.DeepEqual(env, f.env) {
				t.Errorf("decoded envelope differs")
			}
			// Loc is never part of Body.
			for _, field := range []string{`"loc"`, `"machine_id"`, `"scan_gen"`, `"byte_offset"`, `"line_ordinal"`} {
				if bytes.Contains(env.Body, []byte(field)) {
					t.Errorf("body carries locator field %s", field)
				}
			}
			kinds[env.Kind] = true
		})
	}
	for _, k := range []wire.Kind{wire.KindExchangeBegin, wire.KindExchangeAttempt, wire.KindExchangeResponse,
		wire.KindExchangeDone, wire.KindUsage, wire.KindImportRecord, wire.KindImportFile} {
		if !kinds[k] {
			t.Errorf("no golden for kind %s", k)
		}
	}
}

// The same Body at two locators (another machine, offset, rescan, seq, epoch and exact_rev) has the
// same body_sha256, so the server answers duplicate, never conflict.
func TestGoldenSameBodyTwoLocators(t *testing.T) {
	a, err := wire.DecodeEnvelope(readGolden(t, "envelopes", "import.record.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := wire.DecodeEnvelope(readGolden(t, "envelopes", "import.record.relocated.json"))
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a.Loc, b.Loc) || a.Seq == b.Seq || a.ExactRev == b.ExactRev {
		t.Fatal("fixtures must differ in locator, seq and exact_rev")
	}
	if a.Key != b.Key || !bytes.Equal(a.Body, b.Body) || a.BodySHA256() != b.BodySHA256() {
		t.Errorf("same content at two locators must share key and body_sha256")
	}
	if a.BodySHA256() != sha256.Sum256(a.Body) {
		t.Errorf("BodySHA256 is not sha256(Body)")
	}
}

func TestGoldenTombstoneAndHeartbeat(t *testing.T) {
	tomb := tombstoneFixture()
	writeOrCompare(t, filepath.Join(goldenDir, "tombstone.json"), canonJSON(t, tomb))
	hb := heartbeatFixture()
	writeOrCompare(t, filepath.Join(goldenDir, "heartbeat.json"), canonJSON(t, hb))

	b := readGolden(t, "tombstone.json")
	got, err := wire.DecodeTombstone(b)
	if err != nil || got != tomb {
		t.Errorf("DecodeTombstone = %+v, %v", got, err)
	}
	if len(b) > wire.MaxTombstoneJSON {
		t.Errorf("tombstone golden is %d bytes", len(b))
	}
	b = readGolden(t, "heartbeat.json")
	var hb2 wire.Heartbeat
	if err := json.Unmarshal(b, &hb2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hb2, hb) {
		t.Errorf("heartbeat golden does not round-trip")
	}
	if int64(len(b)) > wire.DefaultLimits.Heartbeat {
		t.Errorf("heartbeat golden is %d bytes", len(b))
	}
}

func TestGoldenCAB1(t *testing.T) {
	files, cases := cab1Fixtures(t)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
		writeOrCompare(t, filepath.Join(goldenDir, "cab1", name), files[name])
	}
	sort.Strings(names)
	writeOrCompare(t, filepath.Join(goldenDir, "cab1", "cases.json"), canonJSON(t, cases))

	var onDisk map[string]cab1Case
	if err := json.Unmarshal(readGolden(t, "cab1", "cases.json"), &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != len(names) {
		t.Fatalf("cases.json has %d entries, want %d", len(onDisk), len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			c := onDisk[name]
			b := readGolden(t, "cab1", name)
			got, err := wire.ReadCAB1(bytes.NewReader(b), c.MaxBytes)
			switch c.Error {
			case "":
				if err != nil {
					t.Fatalf("ReadCAB1: %v", err)
				}
				if len(got.Pieces) != c.Pieces || len(got.Envelopes) != c.Envelopes || len(got.Tombstones) != c.Tombstones {
					t.Errorf("counts %d/%d/%d, want %d/%d/%d", len(got.Pieces), len(got.Envelopes), len(got.Tombstones), c.Pieces, c.Envelopes, c.Tombstones)
				}
				if again := writeBatch(t, got); !bytes.Equal(again, b) {
					t.Errorf("re-encoding differs")
				}
			case "framing":
				if !errors.Is(err, wire.ErrCAB1Framing) || errors.Is(err, wire.ErrCAB1TooLarge) {
					t.Errorf("err = %v, want ErrCAB1Framing", err)
				}
			case "too_large":
				if !errors.Is(err, wire.ErrCAB1TooLarge) || errors.Is(err, wire.ErrCAB1Framing) {
					t.Errorf("err = %v, want ErrCAB1TooLarge", err)
				}
			default:
				t.Fatalf("unknown error class %q", c.Error)
			}
		})
	}

	// The valid batch decodes to exactly what was written, in order, with its JSON records verbatim,
	// and every ref of its envelopes is a piece of the batch (pieces precede their envelopes).
	got, err := wire.ReadCAB1(bytes.NewReader(readGolden(t, "cab1", "valid.cab1")), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, validBatch(t)) {
		t.Errorf("valid.cab1 decodes to a different batch")
	}
	have := map[wire.Hash]bool{}
	for _, p := range got.Pieces {
		have[p.Hash] = true
	}
	for _, e := range got.Envelopes {
		env, err := wire.DecodeEnvelope(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range env.Refs {
			if !have[r] {
				t.Errorf("%s ref %s not in the batch", env.Key, r)
			}
		}
	}
}
