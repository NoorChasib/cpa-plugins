package client_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// All data below is synthetic.

const outbox = "0123456789abcdef0123456789abcdef"

var ctx = context.Background()

func piece(kind wire.PieceKind, ct, s string) wire.BatchPiece {
	return wire.BatchPiece{Hash: sha256.Sum256([]byte(s)), Kind: kind, ContentType: ct, Raw: []byte(s)}
}

// env returns the JSON bytes of a valid exchange.completion envelope. The fake does not require
// canonical bytes, so encoding/json is enough here.
func env(t *testing.T, seq uint64, key string, body string, refs ...wire.Hash) []byte {
	t.Helper()
	e := wire.Envelope{
		V: wire.ProtocolV1, Kind: wire.KindExchangeDone, OutboxID: outbox, Seq: seq, EpochSeen: 1, Key: key,
		MaskVersion: wire.MaskV1, CanonVersion: wire.CanonV1, CreatedAt: time.Date(2026, 11, 8, 5, 0, 0, 0, time.UTC),
		Refs: refs, Body: json.RawMessage(body),
	}
	return mustJSON(t, e)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// countingTransport counts round trips, to prove the client never retries.
type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

type stateLog struct {
	mu sync.Mutex
	st []wire.ServerState
}

func (l *stateLog) add(st wire.ServerState) { l.mu.Lock(); l.st = append(l.st, st); l.mu.Unlock() }
func (l *stateLog) last() wire.ServerState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.st) == 0 {
		return wire.ServerState{}
	}
	return l.st[len(l.st)-1]
}

func newPair(t *testing.T, opts clienttest.Options) (*clienttest.Server, string, *client.Client, *stateLog) {
	t.Helper()
	srv := clienttest.NewServer(opts)
	t.Cleanup(srv.Close)
	id, tok := srv.AddSource("cpa-plugin", "test-plugin")
	log := &stateLog{}
	c := client.New(srv.URL+"/", tok, client.Options{UserAgent: "chat-archive-test/0", OnServerState: log.add})
	return srv, id, c, log
}

func TestEnrollAndSourcesSelf(t *testing.T) {
	srv := clienttest.NewServer(clienttest.Options{Epoch: 3})
	defer srv.Close()
	code := srv.NewEnrollCode()
	req := wire.EnrollRequest{Code: code, Kind: "importer", MachineID: "machine-test", Hostname: "host-test", Label: "laptop-test",
		OS: "linux", Arch: "amd64", ClientVersion: "0.0.0-test"}
	got, err := client.Enroll(ctx, srv.URL, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.SourceID, "src_") || len(got.SourceID) != 30 || !strings.HasPrefix(got.Token, "cha_src_") || len(got.Token) != 51 || got.Epoch != 3 || got.ArchiveID == "" {
		t.Fatalf("enroll response %+v", got)
	}
	if _, err := client.Enroll(ctx, srv.URL, req); !errors.Is(err, client.ErrUnauthorized) {
		t.Fatalf("reused code: %v", err)
	}
	if _, err := client.Enroll(ctx, srv.URL, wire.EnrollRequest{Code: "cha_enr_nope", Kind: "importer"}); !errors.Is(err, client.ErrUnauthorized) {
		t.Fatalf("unknown code: %v", err)
	}

	var log stateLog
	c := client.New(srv.URL, got.Token, client.Options{OnServerState: log.add})
	self, err := c.SourcesSelf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if self.SourceID != got.SourceID || self.Kind != "importer" || self.Label != "laptop-test" || self.Epoch != 3 || self.ArchiveID != got.ArchiveID {
		t.Errorf("self %+v", self)
	}
	if self.Limits != wire.DefaultLimits || len(self.Accepted.Protocol) != 1 || self.Accepted.Protocol[0] != 1 ||
		self.Accepted.Canon[0] != wire.CanonV1 || self.Accepted.Mask[wire.MaskV1].CreatedBefore != nil {
		t.Errorf("self limits/accepted %+v %+v", self.Limits, self.Accepted)
	}
	if st := log.last(); st.Epoch != 3 || st.ArchiveID != got.ArchiveID || st.ServerTime.IsZero() {
		t.Errorf("server state %+v", st)
	}
	srv.SetExact(7, []wire.Hash{sha256.Sum256([]byte("fp"))}, []wire.Hash{sha256.Sum256([]byte("caller"))})
	self, err = c.SourcesSelf(ctx)
	if err != nil || self.Exact.Rev != 7 || len(self.Exact.FPs) != 1 || len(self.Exact.CallerFPs) != 1 {
		t.Errorf("exact %+v %v", self.Exact, err)
	}
}

func TestNeedPutCommitReceipts(t *testing.T) {
	srv, id, c, log := newPair(t, clienttest.Options{})
	big := piece(wire.PieceAttachment, "image/png", strings.Repeat("synthetic image bytes ", 1000))
	small := piece(wire.PieceText, "", "synthetic text 1")

	need, staged, st, err := c.Need(ctx, []wire.Hash{big.Hash, small.Hash, big.Hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 2 || need[0] != big.Hash || need[1] != small.Hash || len(staged) != 0 || st.Epoch != 1 {
		t.Fatalf("need %v staged %v st %+v", need, staged, st)
	}
	if err := c.PutPiece(ctx, big.Hash, big.Kind, big.ContentType, big.Raw); err != nil {
		t.Fatal(err)
	}
	if err := c.PutPiece(ctx, big.Hash, big.Kind, big.ContentType, big.Raw); err != nil {
		t.Fatalf("second put (200 present): %v", err)
	}
	if kind, raw, ok := srv.Piece(big.Hash); !ok || kind != wire.PieceAttachment || !bytes.Equal(raw, big.Raw) {
		t.Fatalf("stored piece %v %v", kind, ok)
	}
	need, _, _, err = c.Need(ctx, []wire.Hash{big.Hash, small.Hash})
	if err != nil || len(need) != 1 || need[0] != small.Hash {
		t.Fatalf("need after put %v %v", need, err)
	}

	e1 := env(t, 1, "ex/req-1/completion", `{"request_id":"req-1"}`, big.Hash, small.Hash)
	resp, err := c.Commit(ctx, 1, clienttest.Options{}.ArchiveID+log.last().ArchiveID, []wire.BatchPiece{small}, [][]byte{e1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || !reflect.DeepEqual(resp.Results[0], wire.CommitResult{Seq: 1, Status: wire.StatusOK}) || resp.HW[outbox] != 1 || resp.Epoch != 1 {
		t.Fatalf("commit %+v", resp)
	}
	if _, raw, ok := srv.Piece(small.Hash); !ok || !bytes.Equal(raw, small.Raw) {
		t.Fatal("inline piece not stored")
	}
	archiveID := resp.ArchiveID

	// Same key and body under another seq: duplicate (a receipt, so hw moves). Same key, other body: conflict.
	e2 := env(t, 2, "ex/req-1/completion", `{"request_id":"req-1"}`)
	e3 := env(t, 3, "ex/req-1/completion", `{"request_id":"req-1","outcome":"failed"}`)
	resp, err = c.Commit(ctx, 1, archiveID, nil, [][]byte{e3, e2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.CommitResult{{Seq: 2, Status: wire.StatusDuplicate}, {Seq: 3, Status: wire.StatusConflict}}
	if len(resp.Results) != 2 || !reflect.DeepEqual(resp.Results, want) || resp.HW[outbox] != 3 {
		t.Fatalf("results %+v hw %v", resp.Results, resp.HW)
	}
	if srv.HW(id, outbox) != 3 {
		t.Errorf("HW = %d", srv.HW(id, outbox))
	}
	got := srv.Envelopes(id)
	if len(got) != 3 || got[0].Status != wire.StatusOK || got[1].Status != wire.StatusDuplicate || got[2].Status != wire.StatusConflict ||
		!bytes.Equal(got[0].Raw, e1) || got[0].Envelope.Key != "ex/req-1/completion" {
		t.Errorf("received %+v", got)
	}
	self, err := c.SourcesSelf(ctx)
	if err != nil || self.Outboxes[outbox] != 3 {
		t.Errorf("self outboxes %v %v", self.Outboxes, err)
	}
	// The fake keeps decompressed bodies: the commit arrived as CAB1.
	bodies := srv.RequestBodies()
	var cab1 int
	for _, b := range bodies {
		if bytes.HasPrefix(b, []byte(wire.CAB1Magic)) {
			cab1++
		}
	}
	if cab1 != 2 {
		t.Errorf("%d CAB1 bodies recorded", cab1)
	}
}

func TestCommitPerSeqStatuses(t *testing.T) {
	srv, id, c, log := newPair(t, clienttest.Options{})
	if _, err := c.SourcesSelf(ctx); err != nil {
		t.Fatal(err)
	}
	arch := log.last().ArchiveID
	missing := sha256.Sum256([]byte("never sent"))
	badKey := env(t, 1, "ex/bad key", `{}`)
	noMask := mustJSON(t, map[string]any{"v": 1, "kind": "usage.attempt", "outbox_id": outbox, "seq": 2, "key": "usage/u2", "canon_version": wire.CanonV1, "body": map[string]any{}})
	oldCanon := mustJSON(t, map[string]any{"v": 1, "kind": "usage.attempt", "outbox_id": outbox, "seq": 3, "key": "usage/u3", "mask_version": wire.MaskV1, "canon_version": "canon/v0", "body": map[string]any{}})
	refMissing := env(t, 4, "ex/req-4/completion", `{}`, missing)
	masked := env(t, 5, "ex/req-5/completion", `{}`)
	busy := env(t, 6, "ex/req-6/completion", `{}`)
	unknownKind := []byte(`{"v":1,"seq":7,"outbox_id":"` + outbox + `","kind":"exchange.unknown","key":"ex/x","body":{}}`)
	notJSON := []byte(`{"seq":8,"outbox_id":"` + outbox + `",`) // no readable seq: answered as seq 0
	srv.RejectKey("ex/req-5/completion", wire.ReasonMaskViolation)
	srv.RejectKey("ex/req-6/completion", wire.ReasonBusy)

	resp, err := c.Commit(ctx, 1, arch, nil, [][]byte{notJSON, unknownKind, busy, masked, refMissing, oldCanon, noMask, badKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.CommitResult{
		{Seq: 0, Status: wire.StatusRejected, Reason: wire.ReasonSchema},
		{Seq: 1, Status: wire.StatusRejected, Reason: wire.ReasonSchema},
		{Seq: 2, Status: wire.StatusRejected, Reason: wire.ReasonMaskVersionMissing},
		{Seq: 3, Status: wire.StatusRejected, Reason: wire.ReasonCanonVersion},
		{Seq: 4, Status: wire.StatusRetry, Reason: wire.ReasonMissingRef, Missing: []wire.Hash{missing}},
		{Seq: 5, Status: wire.StatusRejected, Reason: wire.ReasonMaskViolation},
		{Seq: 6, Status: wire.StatusRetry, Reason: wire.ReasonBusy},
		{Seq: 7, Status: wire.StatusRejected, Reason: wire.ReasonSchema},
	}
	if len(resp.Results) != len(want) {
		t.Fatalf("results %+v", resp.Results)
	}
	for i := range want {
		g, w := resp.Results[i], want[i]
		if g.Seq != w.Seq || g.Status != w.Status || g.Reason != w.Reason || len(g.Missing) != len(w.Missing) || (len(w.Missing) == 1 && g.Missing[0] != w.Missing[0]) {
			t.Errorf("result %d = %+v, want %+v", i, g, w)
		}
	}
	// Receipts for 1-3 and 5, 7; seq 4 and 6 are retry (no receipt), so hw stops at 3.
	if resp.HW[outbox] != 3 || srv.HW(id, outbox) != 3 {
		t.Fatalf("hw %v", resp.HW)
	}
	// The producer re-runs need, sends the piece, and retries; the busy key is cleared.
	srv.AcceptKey("ex/req-6/completion")
	srv.AcceptKey("ex/req-5/completion")
	mp := wire.BatchPiece{Hash: missing, Kind: wire.PieceText, Raw: []byte("never sent")}
	resp, err = c.Commit(ctx, 1, arch, []wire.BatchPiece{mp}, [][]byte{refMissing, busy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results[0].Status != wire.StatusOK || resp.Results[1].Status != wire.StatusOK || resp.HW[outbox] != 7 {
		t.Fatalf("retry results %+v hw %v", resp.Results, resp.HW)
	}
	// A requeued mask_violation envelope under a new seq is accepted once the key is.
	requeued := env(t, 8, "ex/req-5/completion", `{"remasked":true}`)
	resp, err = c.Commit(ctx, 1, arch, nil, [][]byte{requeued}, nil)
	if err != nil || resp.Results[0].Status != wire.StatusOK || resp.HW[outbox] != 8 {
		t.Fatalf("requeue %+v %v", resp, err)
	}
	// RejectKey with missing_ref answers retry with the envelope's refs in missing.
	ref := piece(wire.PieceText, "", "synthetic staged-then-vanished")
	srv.RejectKey("ex/req-9/completion", wire.ReasonMissingRef)
	resp, err = c.Commit(ctx, 1, arch, []wire.BatchPiece{ref}, [][]byte{env(t, 9, "ex/req-9/completion", `{}`, ref.Hash)}, nil)
	if err != nil || resp.Results[0].Status != wire.StatusRetry || len(resp.Results[0].Missing) != 1 || resp.Results[0].Missing[0] != ref.Hash {
		t.Fatalf("injected missing_ref %+v %v", resp, err)
	}
}

func TestCommitStaged(t *testing.T) {
	srv, _, c, log := newPair(t, clienttest.Options{})
	h := piece(wire.PieceText, "", "synthetic staged message").Hash
	other := piece(wire.PieceText, "", "synthetic other")
	srv.MarkStaged(h)
	need, staged, _, err := c.Need(ctx, []wire.Hash{h, other.Hash})
	if err != nil || len(need) != 1 || need[0] != other.Hash || len(staged) != 1 || staged[0] != h {
		t.Fatalf("need %v staged %v err %v", need, staged, err)
	}
	resp, err := c.Commit(ctx, 1, log.last().ArchiveID, []wire.BatchPiece{other}, [][]byte{env(t, 1, "ex/req-s/completion", `{}`, h, other.Hash)}, nil)
	if err != nil || resp.Results[0].Status != wire.StatusOK || len(resp.Staged) != 1 || resp.Staged[0] != h {
		t.Fatalf("commit %+v %v", resp, err)
	}
}

func TestCommitTombstones(t *testing.T) {
	// Commit encodes tombstones with canon.Marshal.
	srv, id, c, log := newPair(t, clienttest.Options{})
	if _, err := c.SourcesSelf(ctx); err != nil {
		t.Fatal(err)
	}
	arch := log.last().ArchiveID
	resp, err := c.Commit(ctx, 1, arch, nil, [][]byte{env(t, 1, "ex/a/completion", `{}`), env(t, 3, "ex/c/completion", `{}`)}, nil)
	if err != nil || resp.HW[outbox] != 1 {
		t.Fatalf("commit %+v %v", resp, err)
	}
	tomb := wire.Tombstone{V: 1, OutboxID: outbox, Seq: 2, Key: "ex/b/completion", Reason: wire.TombRejectedLocal}
	resp, err = c.Commit(ctx, 1, arch, nil, nil, []wire.Tombstone{tomb})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || !reflect.DeepEqual(resp.Results[0], wire.CommitResult{Seq: 2, Status: wire.StatusOK}) || resp.HW[outbox] != 3 {
		t.Fatalf("tombstone commit %+v", resp)
	}
	if got := srv.Tombstones(id); len(got) != 1 || got[0] != tomb {
		t.Errorf("tombstones %+v", got)
	}
	// A tombstone and envelopes in one batch, results in seq order.
	tomb5 := wire.Tombstone{V: 1, OutboxID: outbox, Seq: 5, Key: "ex/e/completion", Reason: wire.TombMissingRef}
	resp, err = c.Commit(ctx, 1, arch, nil, [][]byte{env(t, 6, "ex/f/completion", `{}`), env(t, 4, "ex/d/completion", `{}`)}, []wire.Tombstone{tomb5})
	if err != nil || len(resp.Results) != 3 || resp.Results[0].Seq != 4 || resp.Results[1].Seq != 5 || resp.Results[2].Seq != 6 || resp.HW[outbox] != 6 {
		t.Fatalf("mixed %+v %v", resp, err)
	}
}

func TestEpochChanged(t *testing.T) {
	srv, _, c, log := newPair(t, clienttest.Options{ArchiveID: "00000000-0000-4000-8000-0000000000b1"})
	e := env(t, 1, "ex/r/completion", `{}`)
	if _, err := c.Commit(ctx, 1, "00000000-0000-4000-8000-0000000000b1", nil, [][]byte{e}, nil); err != nil {
		t.Fatal(err)
	}
	srv.SetEpoch(2)
	_, err := c.Commit(ctx, 1, "00000000-0000-4000-8000-0000000000b1", nil, [][]byte{e}, nil)
	var ec *client.EpochChangedError
	if !errors.As(err, &ec) || ec.Epoch != 2 || ec.ArchiveID != "00000000-0000-4000-8000-0000000000b1" {
		t.Fatalf("err = %v", err)
	}
	if log.last().Epoch != 2 {
		t.Errorf("OnServerState did not see epoch 2: %+v", log.last())
	}
	// Replay after the restore: the stored receipt makes it duplicate.
	resp, err := c.Commit(ctx, 2, "00000000-0000-4000-8000-0000000000b1", nil, [][]byte{e}, nil)
	if err != nil || resp.Results[0].Status != wire.StatusDuplicate {
		t.Fatalf("replay %+v %v", resp, err)
	}
	srv.SetArchiveID("00000000-0000-4000-8000-0000000000b2")
	if _, err := c.Commit(ctx, 2, "00000000-0000-4000-8000-0000000000b1", nil, [][]byte{e}, nil); !errors.As(err, &ec) || ec.ArchiveID != "00000000-0000-4000-8000-0000000000b2" {
		t.Fatalf("archive id change: %v", err)
	}
	if log.last().ArchiveID != "00000000-0000-4000-8000-0000000000b2" {
		t.Errorf("OnServerState archive id %+v", log.last())
	}
}

// A 401 is returned once as ErrUnauthorized: one request per call, never a retry loop.
func TestUnauthorizedNeverRetried(t *testing.T) {
	srv := clienttest.NewServer(clienttest.Options{})
	defer srv.Close()
	ct := &countingTransport{}
	var states atomic.Int64
	c := client.New(srv.URL, "cha_src_not-a-real-token", client.Options{HTTPClient: &http.Client{Transport: ct},
		OnServerState: func(wire.ServerState) { states.Add(1) }})
	calls := map[string]func() error{
		"SourcesSelf": func() error { _, err := c.SourcesSelf(ctx); return err },
		"Need":        func() error { _, _, _, err := c.Need(ctx, nil); return err },
		"PutPiece": func() error {
			p := piece(wire.PieceText, "", "x")
			return c.PutPiece(ctx, p.Hash, p.Kind, "", p.Raw)
		},
		"Commit":          func() error { _, err := c.Commit(ctx, 1, "x", nil, nil, nil); return err },
		"Heartbeat":       func() error { _, err := c.Heartbeat(ctx, wire.Heartbeat{V: 1}); return err },
		"MediaFetch":      func() error { return c.MediaFetch(ctx, wire.MediaFetch{RequestID: "r"}) },
		"SessionCoverage": func() error { _, err := c.SessionCoverage(ctx, []string{"codex:x"}); return err },
		"PutCaller":       func() error { return c.PutCaller(ctx, strings.Repeat("a", 64), wire.CallerRequest{Machine: "m"}) },
	}
	for name, call := range calls {
		before := ct.n.Load()
		err := call()
		if !errors.Is(err, client.ErrUnauthorized) {
			t.Errorf("%s: err = %v", name, err)
		}
		if strings.Contains(err.Error(), "not-a-real-token") {
			t.Errorf("%s: error carries the token", name)
		}
		if n := ct.n.Load() - before; n != 1 {
			t.Errorf("%s: %d requests for one call", name, n)
		}
	}
	if states.Load() != 0 {
		t.Errorf("a 401 carried server state")
	}
}

func TestFailNextMapping(t *testing.T) {
	srv, _, c, _ := newPair(t, clienttest.Options{})
	commit := func() error { _, err := c.Commit(ctx, 1, "x", nil, nil, nil); return err }
	route := "POST " + wire.PathCommit

	srv.FailNext(route, http.StatusServiceUnavailable, 7*time.Second)
	var ra *client.RetryAfterError
	if err := commit(); !errors.As(err, &ra) || ra.After != 7*time.Second {
		t.Errorf("503: %v", err)
	}
	srv.FailNext(route, http.StatusTooManyRequests, 0)
	if err := commit(); !errors.As(err, &ra) || ra.After != 0 {
		t.Errorf("429: %v", err)
	}
	srv.FailNext(route, http.StatusRequestEntityTooLarge, 0)
	srv.FailNext(route, http.StatusBadRequest, 0)
	if err := commit(); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("413: %v", err)
	}
	if err := commit(); !errors.Is(err, client.ErrMalformed) {
		t.Errorf("400: %v", err)
	}
	srv.FailNext(route, http.StatusInternalServerError, 0)
	var se *client.StatusError
	if err := commit(); !errors.As(err, &se) || se.StatusCode != 500 || se.Code != "injected" {
		t.Errorf("500: %v", err)
	}
	srv.FailNext("PUT "+wire.PathPiecePrefix, http.StatusUnauthorized, 0)
	p := piece(wire.PieceText, "", "x")
	if err := c.PutPiece(ctx, p.Hash, p.Kind, "", p.Raw); !errors.Is(err, client.ErrUnauthorized) {
		t.Errorf("piece 401: %v", err)
	}
	if err := c.PutPiece(ctx, p.Hash, p.Kind, "", p.Raw); err != nil {
		t.Errorf("FailNext must fail only the next request: %v", err)
	}
}

func TestPieceErrors(t *testing.T) {
	_, _, c, _ := newPair(t, clienttest.Options{Limits: func() wire.Limits { l := wire.DefaultLimits; l.TextPiece = 16; return l }()})
	p := piece(wire.PieceText, "", "synthetic")
	if err := c.PutPiece(ctx, sha256.Sum256([]byte("other")), p.Kind, "", p.Raw); !errors.Is(err, client.ErrMalformed) || !strings.Contains(err.Error(), wire.ErrCodePieceHashMismatch) {
		t.Errorf("hash mismatch: %v", err)
	}
	big := piece(wire.PieceText, "", strings.Repeat("x", 17))
	if err := c.PutPiece(ctx, big.Hash, big.Kind, "", big.Raw); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
}

func TestCommitLimits(t *testing.T) {
	limits := wire.DefaultLimits
	limits.PieceInline, limits.CommitEnvelopes, limits.CommitRaw = 8, 2, 4096
	_, _, c, log := newPair(t, clienttest.Options{Limits: limits})
	if _, err := c.SourcesSelf(ctx); err != nil {
		t.Fatal(err)
	}
	arch := log.last().ArchiveID
	if _, err := c.Commit(ctx, 1, arch, []wire.BatchPiece{piece(wire.PieceText, "", "123456789")}, nil, nil); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("inline piece over limit: %v", err)
	}
	three := [][]byte{env(t, 1, "ex/1/completion", `{}`), env(t, 2, "ex/2/completion", `{}`), env(t, 3, "ex/3/completion", `{}`)}
	if _, err := c.Commit(ctx, 1, arch, nil, three, nil); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("envelope count over limit: %v", err)
	}
	if _, err := c.Commit(ctx, 1, arch, nil, [][]byte{env(t, 1, "ex/1/completion", `{"pad":"`+strings.Repeat("p", 5000)+`"}`)}, nil); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("commit_raw over limit: %v", err)
	}
	v2 := mustJSON(t, map[string]any{"v": 2, "kind": "exchange.v2"})
	if _, err := c.Commit(ctx, 1, arch, nil, [][]byte{v2}, nil); !errors.Is(err, client.ErrUnsupportedProtocol) {
		t.Errorf("v2 envelope: %v", err)
	}
	big := wire.DefaultLimits
	big.EnvelopeJSON = 64
	_, _, c2, log2 := newPair(t, clienttest.Options{Limits: big})
	_, _ = c2.SourcesSelf(ctx)
	resp, err := c2.Commit(ctx, 1, log2.last().ArchiveID, nil, [][]byte{env(t, 1, "ex/1/completion", `{"pad":"`+strings.Repeat("p", 100)+`"}`)}, nil)
	if err != nil || resp.Results[0].Status != wire.StatusRejected || resp.Results[0].Reason != wire.ReasonTooLarge {
		t.Errorf("envelope_json over limit: %+v %v", resp, err)
	}
}

func TestHeartbeatMediaCoverageCallers(t *testing.T) {
	srv, id, c, _ := newPair(t, clienttest.Options{Epoch: 4})
	srv.SetExact(9, nil, nil)
	hb := wire.Heartbeat{V: 1, OutboxID: outbox, BootID: "00112233445566778899aabbccddeeff", N: 1, Component: "cpa-plugin", Version: "0.0.0-test",
		State: "running", MaskVersion: wire.MaskV1, Calls: map[string]uint64{"before_auth": 3}}
	hr, err := c.Heartbeat(ctx, hb)
	if err != nil || hr.Epoch != 4 || hr.ExactRev != 9 || hr.ArchiveID == "" || hr.ServerTime.IsZero() {
		t.Fatalf("heartbeat %+v %v", hr, err)
	}
	if got := srv.Heartbeats(id); len(got) != 1 || got[0].Calls["before_auth"] != 3 {
		t.Errorf("heartbeats %+v", got)
	}

	mf := wire.MediaFetch{RequestID: "req-m", Ordinal: 1, URL: "https://media.example.test/v/1?sig=synthetic", MaskedURL: "https://media.example.test/v/1"}
	for range 2 {
		if err := c.MediaFetch(ctx, mf); err != nil {
			t.Fatal(err)
		}
	}
	mf2 := mf
	mf2.Ordinal = 2
	if err := c.MediaFetch(ctx, mf2); err != nil {
		t.Fatal(err)
	}
	if got := srv.MediaFetches(id); len(got) != 2 || got[0] != mf || got[1] != mf2 {
		t.Errorf("media fetches %+v", got)
	}

	first := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	srv.SetCoverage("codex:t1", wire.Coverage{CPASeen: true, FirstCapturedAt: &first, Exchanges: 3, Responses: []string{"resp_1"}})
	cov, err := c.SessionCoverage(ctx, []string{"codex:t1", "codex:t2"})
	if err != nil {
		t.Fatal(err)
	}
	if c1 := cov["codex:t1"]; !c1.CPASeen || c1.Exchanges != 3 || len(c1.Responses) != 1 || !c1.FirstCapturedAt.Equal(first) {
		t.Errorf("t1 %+v", c1)
	}
	if c2, ok := cov["codex:t2"]; !ok || c2.CPASeen || c2.FirstCapturedAt != nil {
		t.Errorf("t2 %+v %v", c2, ok)
	}
	tooMany := make([]string, wire.MaxCoverageIDs+1)
	if _, err := c.SessionCoverage(ctx, tooMany); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("too many ids: %v", err)
	}

	fp := wire.Hash(sha256.Sum256([]byte("caller")))
	if err := c.PutCaller(ctx, fp.String(), wire.CallerRequest{Machine: "laptop-test", App: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutCaller(ctx, fp.String(), wire.CallerRequest{Machine: "laptop-test"}); err != nil {
		t.Errorf("same name again: %v", err)
	}
	var se *client.StatusError
	if err := c.PutCaller(ctx, fp.String(), wire.CallerRequest{Machine: "other-test"}); !errors.As(err, &se) || se.StatusCode != 409 {
		t.Errorf("renaming: %v", err)
	}
	if err := c.PutCaller(ctx, fp.String(), wire.CallerRequest{Machine: "bad name"}); !errors.Is(err, client.ErrMalformed) {
		t.Errorf("bad machine name: %v", err)
	}
	if got := srv.Callers(); len(got) != 1 || got[fp.String()].Machine != "laptop-test" {
		t.Errorf("callers %+v", got)
	}
}

// The commit and piece requests carry the protocol's headers, a zstd stream whose window the server
// options accept, and CAB1 with pieces first.
func TestWireFormatOfRequests(t *testing.T) {
	type captured struct {
		method, path string
		header       http.Header
		body         []byte
	}
	var mu sync.Mutex
	var got []captured
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{r.Method, r.URL.Path, r.Header.Clone(), b})
		mu.Unlock()
		if r.URL.Path == wire.PathCommit {
			_, _ = w.Write([]byte(`{"epoch":1,"archive_id":"a","hw":{},"staged":[],"results":[]}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer hs.Close()
	c := client.New(hs.URL, "cha_src_synthetic", client.Options{UserAgent: "chat-archive-test/0"})

	// A batch above the 8 MiB window, so the encoder must emit a multi-block frame with an 8 MiB window.
	raw := make([]byte, 12<<20)
	for i := range raw {
		raw[i] = byte(i * 7 % 251)
	}
	big := wire.BatchPiece{Hash: sha256.Sum256(raw), Kind: wire.PieceAttachment, ContentType: "application/octet-stream", Raw: raw}
	e := env(t, 1, "ex/r/completion", `{}`, big.Hash)
	if _, err := c.Commit(ctx, 5, "archive-test", []wire.BatchPiece{big}, [][]byte{e}, nil); err != nil {
		t.Fatal(err)
	}
	text := piece(wire.PieceText, "", "synthetic piece")
	if err := c.PutPiece(ctx, text.Hash, text.Kind, "ignored/for-text", text.Raw); err != nil {
		t.Fatal(err)
	}
	att := piece(wire.PieceAttachment, "image/png", "synthetic png")
	if err := c.PutPiece(ctx, att.Hash, att.Kind, att.ContentType, att.Raw); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("%d requests", len(got))
	}
	cm := got[0]
	for k, v := range map[string]string{
		"Content-Type": wire.ContentTypeBatchV1, "Content-Encoding": "zstd", wire.HeaderEpochSeen: "5",
		wire.HeaderArchiveID: "archive-test", "Authorization": "Bearer cha_src_synthetic", "User-Agent": "chat-archive-test/0",
	} {
		if cm.header.Get(k) != v {
			t.Errorf("commit header %s = %q", k, cm.header.Get(k))
		}
	}
	dec, err := zstd.NewReader(bytes.NewReader(cm.body), zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecoderMaxMemory(256<<20), zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	batch, err := wire.ReadCAB1(dec, wire.DefaultLimits.CommitRaw)
	if err != nil {
		t.Fatalf("server-side decode: %v", err)
	}
	if len(batch.Pieces) != 1 || !bytes.Equal(batch.Pieces[0].Raw, raw) || len(batch.Envelopes) != 1 || !bytes.Equal(batch.Envelopes[0], e) || len(batch.Tombstones) != 0 {
		t.Fatalf("batch mismatch")
	}
	var fh zstd.Header
	if err := fh.Decode(cm.body); err != nil || fh.WindowSize > 8<<20 {
		t.Errorf("frame window %d (err %v)", fh.WindowSize, err)
	}

	tp, ap := got[1], got[2]
	if tp.method != http.MethodPut || tp.path != wire.PathPiecePrefix+text.Hash.String() || tp.header.Get(wire.HeaderPieceKind) != "text" ||
		tp.header.Get("Content-Encoding") != "zstd" || tp.header.Get("Content-Type") != "" {
		t.Errorf("text piece request %s %s %v", tp.method, tp.path, tp.header)
	}
	if ap.header.Get(wire.HeaderPieceKind) != "attachment" || ap.header.Get("Content-Type") != "image/png" {
		t.Errorf("attachment piece headers %v", ap.header)
	}
	if plain, err := dec.DecodeAll(tp.body, nil); err != nil || !bytes.Equal(plain, text.Raw) {
		t.Errorf("piece body is not zstd of the raw bytes: %v", err)
	}
}

func TestErrorBodies(t *testing.T) {
	type answer struct {
		status int
		header map[string]string
		body   string
	}
	var next answer
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range next.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(next.status)
		_, _ = w.Write([]byte(next.body))
	}))
	defer hs.Close()
	c := client.New(hs.URL, "cha_src_synthetic", client.Options{})
	call := func(a answer) error {
		next = a
		_, err := c.SourcesSelf(ctx)
		return err
	}

	if err := call(answer{400, nil, `{"error":"unsupported_protocol"}`}); !errors.Is(err, client.ErrUnsupportedProtocol) || errors.Is(err, client.ErrMalformed) {
		t.Errorf("unsupported_protocol: %v", err)
	}
	if err := call(answer{400, nil, `not json`}); !errors.Is(err, client.ErrMalformed) {
		t.Errorf("plain 400: %v", err)
	}
	var se *client.StatusError
	if err := call(answer{422, nil, `{"reason":"mask_violation","rules":["ca-archive-token","jwt"]}`}); !errors.As(err, &se) ||
		se.StatusCode != 422 || se.Code != "mask_violation" || len(se.Rules) != 2 {
		t.Errorf("422: %v", err)
	}
	if err := call(answer{409, nil, `{"error":"caller_named"}`}); !errors.As(err, &se) || se.StatusCode != 409 {
		t.Errorf("409 other: %v", err)
	}
	var ec *client.EpochChangedError
	if err := call(answer{409, nil, `{"error":"epoch_changed","epoch":9,"archive_id":"arch-9"}`}); !errors.As(err, &ec) || ec.Epoch != 9 || ec.ArchiveID != "arch-9" {
		t.Errorf("409 epoch: %v", err)
	}
	var ra *client.RetryAfterError
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if err := call(answer{503, map[string]string{"Retry-After": date}, ``}); !errors.As(err, &ra) || ra.After < 80*time.Second || ra.After > 91*time.Second {
		t.Errorf("503 date: %v", err)
	}
	if err := call(answer{503, map[string]string{"Retry-After": "soon"}, ``}); !errors.As(err, &ra) || ra.After != 0 {
		t.Errorf("503 junk: %v", err)
	}
	if err := call(answer{502, nil, `<html>bad gateway</html>`}); !errors.As(err, &se) || se.StatusCode != 502 || se.Code != "" {
		t.Errorf("502: %v", err)
	}
	// A 403 is an auth-class failure (the archive never sends one): ErrUnauthorized, never retried.
	if err := call(answer{403, nil, ``}); !errors.Is(err, client.ErrUnauthorized) || errors.As(err, &se) || !strings.Contains(err.Error(), "403") {
		t.Errorf("403: %v", err)
	}
	if err := call(answer{200, nil, `{"source_id":`}); err == nil || errors.As(err, &se) {
		t.Errorf("malformed 200: %v", err)
	}
	// Network error: the server is gone.
	hs.Close()
	if err := call(answer{}); err == nil {
		t.Errorf("closed server: nil error")
	}
	for _, e := range []error{&client.EpochChangedError{Epoch: 1, ArchiveID: "a"}, &client.RetryAfterError{After: time.Second}, &client.StatusError{StatusCode: 500}} {
		if e.Error() == "" {
			t.Errorf("%T has an empty message", e)
		}
	}
}

// TestFakeRejectsBadBody: the fake answers rejected/schema, as the real server must, for an
// envelope whose body does not decode as its kind.
func TestFakeRejectsBadBody(t *testing.T) {
	const arch = "00000000-0000-4000-8000-0000000000d1"
	srv, id, c, _ := newPair(t, clienttest.Options{ArchiveID: arch})
	begin := wire.Envelope{
		V: wire.ProtocolV1, Kind: wire.KindExchangeBegin, OutboxID: outbox, Seq: 1, EpochSeen: 1, Key: "ex/req-bad/begin/0",
		MaskVersion: wire.MaskV1, CanonVersion: wire.CanonV1, CreatedAt: time.Date(2026, 11, 8, 5, 0, 0, 0, time.UTC),
		Body: json.RawMessage(`{"stream":"yes","body":{"n":-1,"items":"x"}}`),
	}
	resp, err := c.Commit(ctx, 1, arch, nil, [][]byte{mustJSON(t, begin), env(t, 2, "ex/req-ok/completion", `{"request_id":"req-ok"}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].Status != wire.StatusRejected || resp.Results[0].Reason != wire.ReasonSchema || resp.Results[1].Status != wire.StatusOK {
		t.Errorf("results %+v", resp.Results)
	}
	if srv.HW(id, outbox) != 2 {
		t.Errorf("hw %d: a schema rejection is a receipt", srv.HW(id, outbox))
	}
}

// TestFakeHold: a held route stalls until released, the client gives up, or the server closes.
func TestFakeHold(t *testing.T) {
	srv, _, c, _ := newPair(t, clienttest.Options{})
	release := srv.Hold("POST " + wire.PathHeartbeat)
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := c.Heartbeat(short, wire.Heartbeat{V: 1, OutboxID: outbox, BootID: outbox}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("held heartbeat: %v", err)
	}
	if _, err := c.SourcesSelf(ctx); err != nil {
		t.Errorf("other routes are not held: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Heartbeat(ctx, wire.Heartbeat{V: 1, OutboxID: outbox, BootID: outbox})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	release()
	release() // idempotent
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("released heartbeat: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("release did not unblock the request")
	}
	// Close releases a hold, so a test's cleanup never hangs.
	srv2, _, c2, _ := newPair(t, clienttest.Options{})
	srv2.Hold("GET " + wire.PathSourcesSelf)
	go func() { _, _ = c2.SourcesSelf(ctx) }()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() { srv2.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a held request")
	}
}

// TestPutPieceZstd: an already zstd-encoded piece goes out byte for byte as given, on PutPiece's
// route with PutPiece's headers, the fake archive decodes and stores it, and errors map as for
// PutPiece. The body is encoded at a level other than the client's, so a re-encode would change it.
func TestPutPieceZstd(t *testing.T) {
	encode := func(raw []byte) []byte {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20), zstd.WithEncoderCRC(false))
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		return enc.EncodeAll(raw, nil)
	}

	type captured struct {
		method, path string
		header       http.Header
		body         []byte
	}
	var mu sync.Mutex
	var got []captured
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{r.Method, r.URL.Path, r.Header.Clone(), b})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer hs.Close()
	c := client.New(hs.URL, "cha_src_synthetic", client.Options{UserAgent: "chat-archive-test/0"})
	att := piece(wire.PieceAttachment, "image/png", strings.Repeat("synthetic png ", 200))
	text := piece(wire.PieceText, "", "synthetic text piece")
	attZ := encode(att.Raw)
	for _, call := range []func() error{
		func() error { return c.PutPiece(ctx, att.Hash, att.Kind, att.ContentType, att.Raw) },
		func() error { return c.PutPieceZstd(ctx, att.Hash, att.Kind, att.ContentType, attZ) },
		func() error { return c.PutPieceZstd(ctx, text.Hash, text.Kind, "ignored/for-text", encode(text.Raw)) },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("%d requests", len(got))
	}
	plain, zs, tz := got[0], got[1], got[2]
	if !bytes.Equal(zs.body, attZ) {
		t.Error("PutPieceZstd did not send the body as given")
	}
	if bytes.Equal(plain.body, attZ) {
		t.Error("the test body must differ from the client's own encoding")
	}
	for _, h := range []http.Header{plain.header, zs.header} {
		h.Del("Content-Length")
	}
	if zs.method != plain.method || zs.path != plain.path || !reflect.DeepEqual(zs.header, plain.header) {
		t.Errorf("PutPieceZstd request %s %s %v differs from PutPiece's %s %s %v", zs.method, zs.path, zs.header, plain.method, plain.path, plain.header)
	}
	if tz.header.Get(wire.HeaderPieceKind) != "text" || tz.header.Get("Content-Type") != "" || tz.header.Get("Content-Encoding") != "zstd" {
		t.Errorf("text piece headers %v", tz.header)
	}

	// Against the fake archive: stored decoded, and PutPiece's error mapping.
	srv, _, fc, _ := newPair(t, clienttest.Options{Limits: func() wire.Limits { l := wire.DefaultLimits; l.TextPiece = 64; return l }()})
	if err := fc.PutPieceZstd(ctx, att.Hash, att.Kind, att.ContentType, attZ); err != nil {
		t.Fatal(err)
	}
	if kind, b, ok := srv.Piece(att.Hash); !ok || kind != wire.PieceAttachment || !bytes.Equal(b, att.Raw) {
		t.Errorf("fake archive: stored %v %d bytes", ok, len(b))
	}
	if err := fc.PutPieceZstd(ctx, sha256.Sum256([]byte("other")), text.Kind, "", encode(text.Raw)); !errors.Is(err, client.ErrMalformed) || !strings.Contains(err.Error(), wire.ErrCodePieceHashMismatch) {
		t.Errorf("hash mismatch: %v", err)
	}
	big := piece(wire.PieceText, "", strings.Repeat("x", 65))
	if err := fc.PutPieceZstd(ctx, big.Hash, big.Kind, "", encode(big.Raw)); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	route := "PUT " + wire.PathPiecePrefix
	srv.FailNext(route, http.StatusUnauthorized, 0)
	if err := fc.PutPieceZstd(ctx, text.Hash, text.Kind, "", encode(text.Raw)); !errors.Is(err, client.ErrUnauthorized) {
		t.Errorf("401: %v", err)
	}
	srv.FailNext(route, http.StatusServiceUnavailable, 3*time.Second)
	var ra *client.RetryAfterError
	if err := fc.PutPieceZstd(ctx, text.Hash, text.Kind, "", encode(text.Raw)); !errors.As(err, &ra) || ra.After != 3*time.Second {
		t.Errorf("503: %v", err)
	}
}
