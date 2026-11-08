package uploader

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
	"github.com/klauspost/compress/zstd"
)

type fixture struct {
	srv    *clienttest.Server
	src    string
	ob     *outbox.Outbox
	up     *Uploader
	stats  *hooks.Stats
	exacts atomic.Int32
	deads  []string
}

func setup(t *testing.T, limits wire.Limits) *fixture {
	t.Helper()
	IdleWait = 20 * time.Millisecond
	f := &fixture{srv: clienttest.NewServer(clienttest.Options{Limits: limits}), stats: hooks.NewStats()}
	t.Cleanup(f.srv.Close)
	var token string
	f.src, token = f.srv.AddSource("cpa-plugin", "test")
	ob, err := outbox.Open(outbox.Options{Dir: filepath.Join(t.TempDir(), "d"), CapBytes: 1 << 40, Retention: 168 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	f.ob = ob
	f.up = New(Options{BaseURL: f.srv.URL, Token: token, Outbox: ob, Stats: f.stats,
		OnExact: func(wire.ExactSet) { f.exacts.Add(1) }, OnDead: func(k string) { f.deads = append(f.deads, k) }})
	return f
}

func (f *fixture) start(t *testing.T) {
	f.up.Start()
	t.Cleanup(f.up.Stop)
}

func beginEnv(t *testing.T, rid string, pieces ...canon.Piece) outbox.NewEnvelope {
	t.Helper()
	var items []wire.Item
	var refs []wire.Hash
	for _, p := range pieces {
		h := p.Hash
		items = append(items, wire.Item{F: []byte(`{"type":"text"}`), P: &h})
		refs = append(refs, p.Hash)
	}
	b := wire.ExchangeBegin{RequestID: rid, ObservedAt: time.Unix(5, 0).UTC(), SourceFormat: "claude", Kind: "chat", Transport: "http",
		Headers: []wire.Header{}, Body: wire.Body{Format: canon.FormatClaude, Items: items, N: uint32(len(items)), ListSHA: canon.ListSHA(canon.ItemIDs(items))}}
	body, err := canon.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return outbox.NewEnvelope{Kind: wire.KindExchangeBegin, Key: wire.ExchangeBeginKey(rid, 0), CreatedAt: time.Unix(5, 0), Body: body, Refs: refs, Pieces: pieces}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pending(t *testing.T, ob *outbox.Outbox) int {
	items, err := ob.Pending(context.Background(), time.Now().Add(time.Hour), 1000, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	return len(items)
}

func TestHaveNeedCommitAndPut(t *testing.T) {
	lim := wire.DefaultLimits
	lim.PieceInline = 64 // force PUT for the larger piece
	f := setup(t, lim)
	small := canon.NewPiece(wire.PieceText, "", []byte("tiny"))
	large := canon.NewPiece(wire.PieceText, "", []byte(strings.Repeat("synthetic large piece ", 20)))
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r1", small, large)})
	f.start(t)
	waitFor(t, "ack", func() bool { return pending(t, f.ob) == 0 && len(f.srv.Envelopes(f.src)) == 1 })
	if _, raw, ok := f.srv.Piece(large.Hash); !ok || string(raw) != string(large.Bytes) {
		t.Fatal("large piece not PUT")
	}
	if _, _, ok := f.srv.Piece(small.Hash); !ok {
		t.Fatal("small piece not inline")
	}
	if f.exacts.Load() == 0 {
		t.Fatal("exact set never delivered")
	}
	// A second envelope with the same pieces: have-cache hits, nothing re-sent.
	before := len(f.srv.RequestBodies())
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r2", small, large)})
	f.up.Notify()
	waitFor(t, "second ack", func() bool { return len(f.srv.Envelopes(f.src)) == 2 && pending(t, f.ob) == 0 })
	if f.up.have.size() < 2 {
		t.Fatal("have-cache empty")
	}
	if got := len(f.srv.RequestBodies()) - before; got > 2 {
		t.Fatalf("second upload made %d requests (need skipped expected)", got)
	}
	if hw := f.srv.HW(f.src, f.ob.Meta().OutboxID); hw != 2 {
		t.Fatalf("hw = %d", hw)
	}
}

// putRecorder keeps the wire bytes of every PUT /v1/pieces request, before the archive decodes them.
type putRecorder struct {
	mu   sync.Mutex
	puts map[string][]byte // piece hash (hex) -> request body as sent
	next http.RoundTripper
}

func (r *putRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && strings.HasPrefix(req.URL.Path, wire.PathPiecePrefix) && req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.puts[strings.TrimPrefix(req.URL.Path, wire.PathPiecePrefix)] = b
		r.mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	return r.next.RoundTrip(req)
}

// TestPutSendsStoredBytes: a piece the outbox stores compressed is PUT as its stored zstd bytes,
// byte for byte, never decoded and re-encoded. The stored frame is replaced with another valid
// encoding of the same piece (a faster level, no checksum) that a re-encode could not reproduce.
// A piece stored raw, because compression did not make it smaller, is PUT compressed by the
// client. The archive gets both pieces exactly.
func TestPutSendsStoredBytes(t *testing.T) {
	lim := wire.DefaultLimits
	lim.PieceInline = 64 // force PUT for both pieces
	f := setup(t, lim)
	rec := &putRecorder{puts: map[string][]byte{}, next: http.DefaultTransport}
	f.up.cl = client.New(f.srv.URL, f.up.opts.Token, client.Options{HTTPClient: &http.Client{Transport: rec}, OnServerState: f.up.onState})
	text := canon.NewPiece(wire.PieceText, "", []byte(strings.Repeat("synthetic compressible piece ", 400)))
	noise := make([]byte, 4096)
	rand.NewChaCha8([32]byte{1}).Read(noise)
	att := canon.NewPiece(wire.PieceAttachment, "image/png", noise)
	ctx := context.Background()
	f.ob.Write(ctx, []outbox.NewEnvelope{beginEnv(t, "r1", text, att)})

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	other := enc.EncodeAll(text.Bytes, nil)
	db, err := sql.Open("sqlite3", "file:"+f.ob.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE pieces SET inline=?, stored_len=? WHERE hash=? AND codec=1", other, len(other), text.Hash[:]); err != nil {
		t.Fatal(err)
	}
	db.Close()
	stored, err := f.ob.StoredPiece(ctx, text.Hash)
	if err != nil || !stored.Zstd || !bytes.Equal(stored.Data, other) {
		t.Fatalf("text piece not stored as the replacement frame: zstd=%v err=%v", stored.Zstd, err)
	}
	if raw, err := f.ob.StoredPiece(ctx, att.Hash); err != nil || raw.Zstd {
		t.Fatalf("incompressible piece stored compressed: %v", err)
	}

	f.start(t)
	waitFor(t, "ack", func() bool { return pending(t, f.ob) == 0 && len(f.srv.Envelopes(f.src)) == 1 })
	rec.mu.Lock()
	sent, sentAtt := rec.puts[text.Hash.String()], rec.puts[att.Hash.String()]
	rec.mu.Unlock()
	if !bytes.Equal(sent, other) {
		t.Fatalf("PUT body of the compressed piece (%d bytes) is not its stored frame (%d bytes)", len(sent), len(other))
	}
	if sentAtt == nil || bytes.Equal(sentAtt, noise) {
		t.Fatal("raw-stored piece not PUT zstd-encoded")
	}
	for _, p := range []canon.Piece{text, att} {
		if _, raw, ok := f.srv.Piece(p.Hash); !ok || !bytes.Equal(raw, p.Bytes) {
			t.Fatalf("archive piece %s missing or different", p.Kind)
		}
	}
}

// TestRejectedPieceDeadLettersItsEnvelopes: a 422 (mask_violation) on PUT /v1/pieces makes every
// envelope referencing that piece a requeue-eligible dead letter; the rest of the batch commits
// and the uploader does not stall on the same PUT.
func TestRejectedPieceDeadLettersItsEnvelopes(t *testing.T) {
	lim := wire.DefaultLimits
	lim.PieceInline = 64 // force PUT for the larger piece
	f := setup(t, lim)
	large := canon.NewPiece(wire.PieceText, "", []byte(strings.Repeat("synthetic rejected piece ", 20)))
	other := canon.NewPiece(wire.PieceText, "", []byte("unrelated"))
	ctx := context.Background()
	f.ob.Write(ctx, []outbox.NewEnvelope{beginEnv(t, "r1", large), beginEnv(t, "r2", other)})
	f.srv.FailNext("PUT "+wire.PathPiecePrefix, http.StatusUnprocessableEntity, 0)
	f.start(t)
	waitFor(t, "r2 acked, r1 dead", func() bool {
		d, _ := f.ob.DeadLetters(ctx, string(wire.ReasonMaskViolation), 0, 10)
		return pending(t, f.ob) == 0 && len(d) == 1 && len(f.srv.Envelopes(f.src)) == 1
	})
	if got := f.srv.Envelopes(f.src)[0].Envelope.Key; got != wire.ExchangeBeginKey("r2", 0) {
		t.Fatalf("committed %s", got)
	}
	f.up.Stop()
	if len(f.deads) != 1 || f.deads[0] != wire.ExchangeBeginKey("r1", 0) {
		t.Fatalf("OnDead = %v", f.deads)
	}
	if s := f.up.Stats(); s.ConsecutiveFailures != 0 {
		t.Fatalf("uploader stats = %+v", s)
	}
}

// TestInlinePiecesStayNearCommitTarget: inline pieces fill a commit only up to the 8 MiB producer
// target even when commit_raw allows 32 MiB; the rest go by PUT.
func TestInlinePiecesStayNearCommitTarget(t *testing.T) {
	f := setup(t, wire.DefaultLimits)
	var pieces []canon.Piece
	for i := 0; i < 4; i++ {
		b := make([]byte, 3<<20)
		for j := range b {
			b[j] = byte('a' + (i+j)%26)
		}
		pieces = append(pieces, canon.NewPiece(wire.PieceText, "", b))
	}
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r1", pieces...)})
	f.start(t)
	waitFor(t, "ack", func() bool { return pending(t, f.ob) == 0 })
	for _, p := range pieces {
		if _, _, ok := f.srv.Piece(p.Hash); !ok {
			t.Fatal("piece missing at the archive")
		}
	}
	for _, b := range f.srv.RequestBodies() {
		if len(b) > wire.TargetCommitBytes+(64<<10) && len(b) != 3<<20 {
			t.Fatalf("request body of %d bytes (commit above the 8 MiB target)", len(b))
		}
	}
}

func TestLocalDeadLetterTombstoneReachesArchive(t *testing.T) {
	f := setup(t, wire.Limits{})
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r1")})
	f.srv.FailNext("POST "+wire.PathCommit, http.StatusBadRequest, 0)
	f.start(t)
	waitFor(t, "tombstone", func() bool { return len(f.srv.Tombstones(f.src)) == 1 })
	tb := f.srv.Tombstones(f.src)[0]
	if tb.Seq != 1 || tb.Reason != wire.TombRejectedLocal {
		t.Fatalf("tombstone = %+v", tb)
	}
	waitFor(t, "hw past tombstone", func() bool { return f.srv.HW(f.src, f.ob.Meta().OutboxID) == 1 })
	if len(f.deads) != 1 || f.deads[0] != wire.ExchangeBeginKey("r1", 0) {
		t.Fatalf("OnDead = %v", f.deads)
	}
}

func TestMissingRefThreeTimesBecomesTombstone(t *testing.T) {
	MaxBackoff = 50 * time.Millisecond
	f := setup(t, wire.Limits{})
	e := beginEnv(t, "r1")
	f.srv.RejectKey(e.Key, wire.ReasonMissingRef)
	f.ob.Write(context.Background(), []outbox.NewEnvelope{e})
	f.start(t)
	waitFor(t, "missing_ref tombstone", func() bool { return len(f.srv.Tombstones(f.src)) == 1 })
	if r := f.srv.Tombstones(f.src)[0].Reason; r != wire.TombMissingRef {
		t.Fatalf("reason = %s", r)
	}
}

func TestEpochChangeReplaysWindow(t *testing.T) {
	f := setup(t, wire.Limits{})
	p := canon.NewPiece(wire.PieceText, "", []byte("synthetic"))
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r1", p), beginEnv(t, "r2")})
	f.start(t)
	waitFor(t, "first acks", func() bool { return len(f.srv.Envelopes(f.src)) == 2 && pending(t, f.ob) == 0 })
	f.srv.SetEpoch(2)
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r3")})
	f.up.Notify()
	waitFor(t, "replay", func() bool { return len(f.srv.Envelopes(f.src)) >= 5 && pending(t, f.ob) == 0 })
	got := f.srv.Envelopes(f.src)
	dups := 0
	for _, r := range got[2:] {
		if r.Status == wire.StatusDuplicate {
			dups++
		}
	}
	if dups != 2 {
		t.Fatalf("replayed duplicates = %d of %d", dups, len(got))
	}
	if m := f.ob.Meta(); m.EpochSeen != 2 {
		t.Fatalf("epoch_seen = %d", m.EpochSeen)
	}
	if s := f.up.Stats(); s.EpochSeen != 2 || s.ReplayGapFrom != 0 {
		t.Fatalf("uploader stats = %+v", s)
	}
}

func TestUnauthorizedNeverRetriedInALoop(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f := setup(t, wire.Limits{})
		f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r1")})
		f.srv.FailNext("POST "+wire.PathCommit, status, 0)
		f.start(t)
		waitFor(t, "auth fault", func() bool { return f.stats.HasFault("auth_failed") })
		n := len(f.srv.RequestBodies())
		time.Sleep(500 * time.Millisecond)
		if got := len(f.srv.RequestBodies()); got != n {
			t.Fatalf("status %d: %d more requests after an auth failure", status, got-n)
		}
		if s := f.up.Stats(); s.LastErrorClass != "401" || pending(t, f.ob) != 1 {
			t.Fatalf("status %d: stats %+v pending %d", status, s, pending(t, f.ob))
		}
		f.up.Stop()
	}
}

func TestStagedHashesNeverHaveCached(t *testing.T) {
	f := setup(t, wire.Limits{})
	p := canon.NewPiece(wire.PieceText, "", []byte("staged synthetic"))
	f.srv.MarkStaged(p.Hash)
	f.ob.Write(context.Background(), []outbox.NewEnvelope{beginEnv(t, "r1", p)})
	f.start(t)
	waitFor(t, "ack", func() bool { return pending(t, f.ob) == 0 })
	if f.up.have.has(p.Hash) {
		t.Fatal("staged hash entered the have-cache")
	}
}

func TestMediaFetchSentAndDroppedOnStop(t *testing.T) {
	f := setup(t, wire.Limits{})
	f.start(t)
	exp := time.Now().Add(time.Hour)
	if !f.up.Media().Enqueue(wire.MediaFetch{RequestID: "r1", Ordinal: 0, URL: "https://media.invalid/v.mp4?sig=x", MaskedURL: "https://media.invalid/v.mp4", ExpiresAt: &exp}) {
		t.Fatal("enqueue refused")
	}
	waitFor(t, "media fetch", func() bool { return len(f.srv.MediaFetches(f.src)) == 1 })
	f.srv.Hold("POST " + wire.PathMediaFetch)
	f.up.Media().Enqueue(wire.MediaFetch{RequestID: "r2", URL: "https://media.invalid/b", MaskedURL: "https://media.invalid/b", ExpiresAt: &exp})
	start := time.Now()
	f.up.Stop()
	if d := time.Since(start); d > StopDeadline+FinalHeartbeat+2*time.Second {
		t.Fatalf("stop took %v", d)
	}
}
