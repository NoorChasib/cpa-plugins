// Package clienttest is an in-memory fake archive server implementing the source-authenticated
// routes producers call, for the plugin's and the importer's tests. It listens on 127.0.0.1 through net/http/httptest.
//
// Behaviour it models: bearer-token auth (401 on a bad or missing token, never 403); X-Archive-Epoch,
// X-Archive-Archive-Id and X-Archive-Server-Time on every source-authenticated response; zstd and CAB1
// decoding (framing errors 400); 409 epoch_changed when X-Archive-Epoch-Seen or X-Archive-Archive-Id
// differ from the current state; receipts by (key_ns, key) with body_sha256 = sha256(Envelope.Body)
// (ok, duplicate, conflict); retry/missing_ref for refs neither in the batch nor stored; tombstones as
// receipts; hw per (source, outbox) over contiguous receipts; enrollment codes; media-fetch idempotent
// by (request_id, ordinal); staged lists for hashes marked with MarkStaged. Not modelled: staging
// ages (the 3-day rule) and promotion, token rotation and revocation, enrollment-code expiry, mask
// detection (use RejectKey), admin and read routes.
//
// In more detail, a commit answers per seq, in seq order: rejected/schema for an envelope or tombstone
// that fails the v1 decoder (wire.DecodeEnvelope, wire.DecodeTombstone); rejected/too_large for an
// envelope over limits.envelope_json; rejected/mask_version_missing unless mask_version is mask/v1;
// rejected/canon_version unless canon_version is canon/v1; rejected/schema for a body that does not
// decode as its kind (wire.DecodeBody); retry/missing_ref (with missing) for refs
// not in the batch, not stored and not marked staged; then a RejectKey reason (retry for missing_ref,
// busy and store_unavailable, rejected otherwise); then ok, duplicate or conflict by key. Every status
// except retry is a receipt for its (source, outbox, seq). A whole request answers 400 for bad zstd,
// CAB1 framing or an envelope or tombstone with v above 1 (unsupported_protocol), and 413 for a stream
// over limits.commit_raw, more than limits.commit_envelopes envelopes or an inline piece over
// limits.piece_inline. MarkStaged declares hashes present only in the staging tier:
// need lists them in staged, they satisfy refs, and a commit lists every acked ref among them in
// staged. Hold stalls a route until released, for shutdown and deadline tests.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package clienttest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Options configure a fake archive.
type Options struct {
	ArchiveID string      // "" means a fixed test UUID
	Epoch     uint64      // 0 means 1
	Limits    wire.Limits // zero value means wire.DefaultLimits
}

// defaultArchiveID is the archive id of a Server started with Options.ArchiveID empty.
const defaultArchiveID = "00000000-0000-4000-8000-00000000a001"

// Server is a running fake archive. All methods are safe for concurrent use.
type Server struct {
	// URL is the base URL (http://127.0.0.1:<port>) to pass to client.New and client.Enroll.
	URL string

	hs *httptest.Server
	mu sync.Mutex

	archiveID string
	epoch     uint64
	limits    wire.Limits
	exact     wire.ExactSet

	sources  map[string]*source // by source id
	byToken  map[string]*source
	codes    map[string]bool // unused enrollment codes
	coverage map[string]wire.Coverage
	rejects  map[string]wire.Reason
	fails    map[string][]failure     // by route, FIFO
	holds    map[string]chan struct{} // by route: closed on release
	pieces   map[wire.Hash]piece
	staged   map[wire.Hash]bool
	receipts map[nsKey]receipt
	seqs     map[outboxKey]map[uint64]bool
	hw       map[outboxKey]uint64
	callers  map[string]wire.CallerRequest
	bodies   [][]byte
}

type source struct {
	id, kind, label string
	envelopes       []Received
	tombstones      []wire.Tombstone
	heartbeats      []wire.Heartbeat
	media           []wire.MediaFetch
	mediaSeen       map[string]bool
}

type failure struct {
	status     int
	retryAfter time.Duration
}

type piece struct {
	kind        wire.PieceKind
	contentType string
	raw         []byte
}

type nsKey struct{ ns, key string }

type receipt struct {
	bodySHA wire.Hash
}

type outboxKey struct{ source, outbox string }

// NewServer starts a fake archive.
func NewServer(opts Options) *Server {
	s := &Server{
		archiveID: opts.ArchiveID,
		epoch:     opts.Epoch,
		limits:    opts.Limits,
		exact:     wire.ExactSet{FPs: []wire.Hash{}, CallerFPs: []wire.Hash{}},
		sources:   map[string]*source{},
		byToken:   map[string]*source{},
		codes:     map[string]bool{},
		coverage:  map[string]wire.Coverage{},
		rejects:   map[string]wire.Reason{},
		fails:     map[string][]failure{},
		holds:     map[string]chan struct{}{},
		pieces:    map[wire.Hash]piece{},
		staged:    map[wire.Hash]bool{},
		receipts:  map[nsKey]receipt{},
		seqs:      map[outboxKey]map[uint64]bool{},
		hw:        map[outboxKey]uint64{},
		callers:   map[string]wire.CallerRequest{},
	}
	if s.archiveID == "" {
		s.archiveID = defaultArchiveID
	}
	if s.epoch == 0 {
		s.epoch = 1
	}
	if s.limits == (wire.Limits{}) {
		s.limits = wire.DefaultLimits
	}
	s.hs = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.hs.URL
	return s
}

// Close releases every Hold and stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	for route, ch := range s.holds {
		close(ch)
		delete(s.holds, route)
	}
	s.mu.Unlock()
	s.hs.Close()
}

// AddSource creates a source of kind "cpa-plugin" or "importer" and returns its id and a cha_src_ token.
func (s *Server) AddSource(kind, label string) (sourceID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addSourceLocked(kind, label)
}

func (s *Server) addSourceLocked(kind, label string) (string, string) {
	src := &source{id: "src_" + randomString(crockford, 26), kind: kind, label: label, mediaSeen: map[string]bool{}}
	token := "cha_src_" + randomString(base62, 43)
	s.sources[src.id] = src
	s.byToken[token] = src
	return src.id, token
}

// NewEnrollCode mints a single-use cha_enr_ code for POST /v1/enroll (kind importer).
func (s *Server) NewEnrollCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := "cha_enr_" + randomString(base62, 32)
	s.codes[code] = true
	return code
}

// SetEpoch sets the archive epoch, as a restore would (stored receipts are kept, so replays come back
// duplicate).
func (s *Server) SetEpoch(epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch = epoch
}

// SetArchiveID sets the archive id.
func (s *Server) SetArchiveID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archiveID = id
}

// SetExact sets the exact set returned by GET /v1/sources/self and the exact_rev of heartbeat answers.
func (s *Server) SetExact(rev uint32, fps, callerFPs []wire.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exact = wire.ExactSet{Rev: rev, FPs: append([]wire.Hash{}, fps...), CallerFPs: append([]wire.Hash{}, callerFPs...)}
}

// SetCoverage sets the answer of POST /v1/ingest/session-coverage for one session id; ids never set
// answer cpa_seen false.
func (s *Server) SetCoverage(sessionID string, c wire.Coverage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coverage[sessionID] = c
}

// RejectKey makes every later commit of key answer rejected with reason until AcceptKey(key). A retry
// reason (missing_ref, busy, store_unavailable) answers retry instead, without a receipt; missing_ref
// lists the envelope's refs in missing.
func (s *Server) RejectKey(key string, reason wire.Reason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejects[key] = reason
}

// AcceptKey undoes RejectKey.
func (s *Server) AcceptKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rejects, key)
}

// MarkStaged declares hashes present only in the staging tier: need answers
// them in staged instead of need, they resolve refs, and a commit lists every acked envelope ref
// among them in staged. Staged takes precedence over a stored copy, as a re-sent staging piece is
// re-staged.
func (s *Server) MarkStaged(hashes ...wire.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hashes {
		s.staged[h] = true
	}
}

// FailNext makes the next request to route answer status (with Retry-After when retryAfter > 0) and
// nothing else. route is a method and a path constant from pkg/wire, for example
// "POST " + wire.PathCommit or "PUT " + wire.PathPiecePrefix. Calls queue: two calls fail the next two
// requests.
func (s *Server) FailNext(route string, status int, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[route] = append(s.fails[route], failure{status: status, retryAfter: retryAfter})
}

// Hold makes every request to route (named as for FailNext) wait, before it is read or answered,
// until release is called, the request's context ends (the client gave up or disconnected) or the
// server is closed; then it is served normally. It models a stalled archive for shutdown and
// in-flight deadline tests. Calling Hold again for a held route returns the same release; release
// may be called more than once.
func (s *Server) Hold(route string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.holds[route]
	if !ok {
		ch = make(chan struct{})
		s.holds[route] = ch
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur, ok := s.holds[route]; ok && cur == ch {
			close(ch)
			delete(s.holds, route)
		}
	}
}

// Received is one envelope the fake answered.
type Received struct {
	SourceID string
	Envelope wire.Envelope
	Raw      []byte // the CAB1 type-1 record bytes as received
	Status   wire.Status
	Reason   wire.Reason
}

// Envelopes returns every envelope sourceID committed, in arrival order, with the status it got.
func (s *Server) Envelopes(sourceID string) []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceID]; src != nil {
		return append([]Received(nil), src.envelopes...)
	}
	return nil
}

// Tombstones returns every tombstone sourceID committed, in arrival order.
func (s *Server) Tombstones(sourceID string) []wire.Tombstone {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceID]; src != nil {
		return append([]wire.Tombstone(nil), src.tombstones...)
	}
	return nil
}

// Piece returns a stored piece's kind and raw bytes.
func (s *Server) Piece(h wire.Hash) (kind wire.PieceKind, raw []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pieces[h]
	return p.kind, p.raw, ok
}

// Heartbeats returns every heartbeat sourceID sent, in arrival order.
func (s *Server) Heartbeats(sourceID string) []wire.Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceID]; src != nil {
		return append([]wire.Heartbeat(nil), src.heartbeats...)
	}
	return nil
}

// MediaFetches returns every distinct media fetch sourceID sent, in arrival order.
func (s *Server) MediaFetches(sourceID string) []wire.MediaFetch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceID]; src != nil {
		return append([]wire.MediaFetch(nil), src.media...)
	}
	return nil
}

// Callers returns the caller names set through PUT /v1/callers, by caller_fp.
func (s *Server) Callers() map[string]wire.CallerRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]wire.CallerRequest, len(s.callers))
	for k, v := range s.callers {
		out[k] = v
	}
	return out
}

// HW returns hw(sourceID, outboxID).
func (s *Server) HW(sourceID, outboxID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hw[outboxKey{sourceID, outboxID}]
}

// RequestBodies returns every request body received on any route, decompressed, in arrival order, for
// canary scans of what the archive received.
func (s *Server) RequestBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.bodies))
	for i, b := range s.bodies {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

// ---- HTTP ----

// routeOf names a request the way FailNext does: method, space, path, with the {sha256} and
// {caller_fp} segments cut off.
func routeOf(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, wire.PathPiecePrefix):
		p = wire.PathPiecePrefix
	case strings.HasPrefix(p, wire.PathCallerPrefix):
		p = wire.PathCallerPrefix
	}
	return r.Method + " " + p
}

// bodyCap is the largest decoded request body each route accepts.
func (s *Server) bodyCap(route string) int64 {
	switch route {
	case "POST " + wire.PathCommit:
		return s.limits.CommitRaw
	case "PUT " + wire.PathPiecePrefix:
		return max(s.limits.TextPiece, s.limits.Attachment)
	case "POST " + wire.PathHeartbeat:
		return s.limits.Heartbeat
	case "POST " + wire.PathMediaFetch:
		return wire.MaxMediaFetchJSON
	}
	return 8 << 20
}

var errTooLarge = errors.New("body too large")

// readBody reads the request body, zstd-decoded when Content-Encoding is zstd (with the server's
// decoder options: 8 MiB window, 256 MiB decoder memory), capped at limit decoded bytes.
func readBody(r *http.Request, limit int64) (raw, decoded []byte, err error) {
	raw, err = io.ReadAll(io.LimitReader(r.Body, 2*limit+1<<20))
	if err != nil {
		return nil, nil, err
	}
	if r.Header.Get("Content-Encoding") != "zstd" {
		if int64(len(raw)) > limit {
			return raw, nil, errTooLarge
		}
		return raw, raw, nil
	}
	dec, err := zstd.NewReader(bytes.NewReader(raw),
		zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecoderMaxMemory(256<<20), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return raw, nil, err
	}
	defer dec.Close()
	decoded, err = io.ReadAll(io.LimitReader(dec, limit+1))
	if err != nil {
		return raw, nil, err
	}
	if int64(len(decoded)) > limit {
		return raw, nil, errTooLarge
	}
	return raw, decoded, nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	route := routeOf(r)
	s.mu.Lock()
	hold := s.holds[route]
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	raw, body, berr := readBody(r, s.bodyCap(route))

	s.mu.Lock()
	defer s.mu.Unlock()
	if body != nil {
		s.bodies = append(s.bodies, body)
	} else if raw != nil {
		s.bodies = append(s.bodies, raw)
	}

	if q := s.fails[route]; len(q) > 0 {
		f := q[0]
		s.fails[route] = q[1:]
		if f.status != http.StatusUnauthorized {
			s.stateHeaders(w)
		}
		if f.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((f.retryAfter+time.Second-1)/time.Second)))
		}
		writeJSON(w, f.status, wire.ErrorBody{Error: "injected"})
		return
	}

	switch route {
	case "GET " + wire.PathHealthz:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	case "POST " + wire.PathEnroll:
		s.enroll(w, body, berr)
		return
	}

	src := s.authenticate(r)
	if src == nil {
		writeJSON(w, http.StatusUnauthorized, wire.ErrorBody{Error: "unauthorized"})
		return
	}
	s.stateHeaders(w)
	if berr != nil {
		if errors.Is(berr, errTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
		} else {
			writeJSON(w, http.StatusBadRequest, wire.ErrorBody{Error: "bad_body"})
		}
		return
	}
	switch route {
	case "GET " + wire.PathSourcesSelf:
		s.sourcesSelf(w, src)
	case "POST " + wire.PathNeed:
		s.need(w, body)
	case "PUT " + wire.PathPiecePrefix:
		s.putPiece(w, r, body)
	case "POST " + wire.PathCommit:
		s.commit(w, r, src, body)
	case "POST " + wire.PathHeartbeat:
		s.heartbeat(w, src, body)
	case "POST " + wire.PathMediaFetch:
		s.mediaFetch(w, src, body)
	case "POST " + wire.PathSessionCoverage:
		s.sessionCoverage(w, body)
	case "PUT " + wire.PathCallerPrefix:
		s.putCaller(w, r, body)
	default:
		writeJSON(w, http.StatusNotFound, wire.ErrorBody{Error: "not_found"})
	}
}

func (s *Server) authenticate(r *http.Request) *source {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil
	}
	return s.byToken[tok]
}

func (s *Server) stateHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set(wire.HeaderEpoch, strconv.FormatUint(s.epoch, 10))
	h.Set(wire.HeaderArchiveID, s.archiveID)
	h.Set(wire.HeaderServerTime, time.Now().UTC().Format(time.RFC3339Nano))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, wire.ErrorBody{Error: code})
}

func (s *Server) enroll(w http.ResponseWriter, body []byte, berr error) {
	if berr != nil {
		badRequest(w, "bad_body")
		return
	}
	var req wire.EnrollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		badRequest(w, "bad_json")
		return
	}
	if !s.codes[req.Code] {
		writeJSON(w, http.StatusUnauthorized, wire.ErrorBody{Error: "unauthorized"})
		return
	}
	if req.Kind != "importer" {
		badRequest(w, "bad_kind")
		return
	}
	delete(s.codes, req.Code)
	id, token := s.addSourceLocked(req.Kind, req.Label)
	writeJSON(w, http.StatusOK, wire.EnrollResponse{SourceID: id, Token: token, ArchiveID: s.archiveID, Epoch: s.epoch})
}

func (s *Server) sourcesSelf(w http.ResponseWriter, src *source) {
	outboxes := map[string]uint64{}
	for k, v := range s.hw {
		if k.source == src.id {
			outboxes[k.outbox] = v
		}
	}
	for k := range s.seqs {
		if _, ok := outboxes[k.outbox]; !ok && k.source == src.id {
			outboxes[k.outbox] = 0
		}
	}
	writeJSON(w, http.StatusOK, wire.SourcesSelf{
		SourceID:  src.id,
		Kind:      src.kind,
		Label:     src.label,
		ArchiveID: s.archiveID,
		Epoch:     s.epoch,
		Outboxes:  outboxes,
		Accepted: wire.Accepted{
			Protocol: []int{wire.ProtocolV1},
			Canon:    []string{wire.CanonV1},
			Mask:     map[string]wire.MaskAcceptance{wire.MaskV1: {CreatedBefore: nil}},
		},
		Exact:      s.exact,
		Limits:     s.limits,
		ServerTime: time.Now().UTC(),
	})
}

func (s *Server) need(w http.ResponseWriter, body []byte) {
	var req wire.NeedRequest
	if err := json.Unmarshal(body, &req); err != nil {
		badRequest(w, "bad_json")
		return
	}
	if len(req.Pieces) > s.limits.NeedHashes {
		writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
		return
	}
	out := wire.NeedResponse{Need: []wire.Hash{}, Staged: []wire.Hash{}}
	seen := map[wire.Hash]bool{}
	for _, h := range req.Pieces {
		if seen[h] {
			continue
		}
		seen[h] = true
		switch _, stored := s.pieces[h]; {
		case s.staged[h]:
			out.Staged = append(out.Staged, h)
		case !stored:
			out.Need = append(out.Need, h)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putPiece(w http.ResponseWriter, r *http.Request, body []byte) {
	h, err := wire.ParseHash(strings.TrimPrefix(r.URL.Path, wire.PathPiecePrefix))
	if err != nil {
		badRequest(w, "bad_hash")
		return
	}
	kind, err := wire.ParsePieceKind(r.Header.Get(wire.HeaderPieceKind))
	if err != nil {
		badRequest(w, "bad_piece_kind")
		return
	}
	limit := s.limits.TextPiece
	if kind == wire.PieceAttachment {
		limit = s.limits.Attachment
	}
	if int64(len(body)) > limit {
		writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
		return
	}
	if sha256.Sum256(body) != h {
		badRequest(w, wire.ErrCodePieceHashMismatch)
		return
	}
	if _, ok := s.pieces[h]; ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	ct := ""
	if kind == wire.PieceAttachment {
		ct = r.Header.Get("Content-Type")
	}
	s.pieces[h] = piece{kind: kind, contentType: ct, raw: append([]byte(nil), body...)}
	w.WriteHeader(http.StatusCreated)
}

// commitItem is one envelope or tombstone of a batch, in seq order.
type commitItem struct {
	raw  []byte
	tomb bool
	env  wire.Envelope
	ts   wire.Tombstone
	err  error  // decode error (ErrSchema)
	seq  uint64 // best-effort seq of an item that failed to decode, else its seq
	obx  string // best-effort outbox_id likewise
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request, src *source, body []byte) {
	seen, _ := strconv.ParseUint(r.Header.Get(wire.HeaderEpochSeen), 10, 64)
	if seen != s.epoch || r.Header.Get(wire.HeaderArchiveID) != s.archiveID {
		writeJSON(w, http.StatusConflict, wire.ErrorBody{Error: wire.ErrCodeEpochChanged, Epoch: s.epoch, ArchiveID: s.archiveID})
		return
	}
	batch, err := wire.ReadCAB1(bytes.NewReader(body), s.limits.CommitRaw)
	switch {
	case errors.Is(err, wire.ErrCAB1TooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
		return
	case err != nil:
		badRequest(w, "bad_cab1")
		return
	}
	if len(batch.Envelopes) > s.limits.CommitEnvelopes {
		writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
		return
	}
	for _, p := range batch.Pieces {
		if int64(len(p.Raw)) > s.limits.PieceInline {
			writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
			return
		}
	}

	items := make([]commitItem, 0, len(batch.Envelopes)+len(batch.Tombstones))
	for _, raw := range batch.Envelopes {
		it := commitItem{raw: raw}
		it.env, it.err = wire.DecodeEnvelope(raw)
		if errors.Is(it.err, wire.ErrUnsupportedProtocol) {
			badRequest(w, wire.ErrCodeUnsupportedProtocol)
			return
		}
		it.seq, it.obx = it.env.Seq, it.env.OutboxID
		if it.err != nil {
			it.seq, it.obx = lenientPosition(raw)
			it.env = wire.Envelope{Seq: it.seq, OutboxID: it.obx}
		}
		items = append(items, it)
	}
	for _, raw := range batch.Tombstones {
		it := commitItem{raw: raw, tomb: true}
		it.ts, it.err = wire.DecodeTombstone(raw)
		if errors.Is(it.err, wire.ErrUnsupportedProtocol) {
			badRequest(w, wire.ErrCodeUnsupportedProtocol)
			return
		}
		it.seq, it.obx = it.ts.Seq, it.ts.OutboxID
		if it.err != nil {
			it.seq, it.obx = lenientPosition(raw)
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].seq < items[j].seq })

	inBatch := make(map[wire.Hash]bool, len(batch.Pieces))
	for _, p := range batch.Pieces {
		inBatch[p.Hash] = true
	}
	resp := wire.CommitResponse{Epoch: s.epoch, ArchiveID: s.archiveID, HW: map[string]uint64{}, Staged: []wire.Hash{}, Results: []wire.CommitResult{}}
	stagedSeen := map[wire.Hash]bool{}
	touched := map[string]bool{}

	for _, it := range items {
		res := wire.CommitResult{Seq: it.seq}
		if validOutbox(it.obx) {
			touched[it.obx] = true
		}
		if it.tomb {
			if it.err != nil {
				res.Status, res.Reason = wire.StatusRejected, wire.ReasonSchema
			} else {
				res.Status = wire.StatusOK
				src.tombstones = append(src.tombstones, it.ts)
			}
			s.addReceipt(src.id, it.obx, it.seq)
			resp.Results = append(resp.Results, res)
			continue
		}
		res.Status, res.Reason, res.Missing = s.judge(src, it, inBatch)
		if res.Status != wire.StatusRetry {
			s.addReceipt(src.id, it.obx, it.seq)
		}
		if acked(res.Status) {
			for _, h := range it.env.Refs {
				if s.staged[h] && !stagedSeen[h] {
					stagedSeen[h] = true
					resp.Staged = append(resp.Staged, h)
				}
			}
		}
		src.envelopes = append(src.envelopes, Received{
			SourceID: src.id, Envelope: it.env, Raw: it.raw, Status: res.Status, Reason: res.Reason,
		})
		resp.Results = append(resp.Results, res)
	}

	// Every valid batch piece is stored, including pieces of duplicate and rejected envelopes
	// (the server stores them too; mask detection is not modelled).
	for _, p := range batch.Pieces {
		if _, ok := s.pieces[p.Hash]; !ok {
			s.pieces[p.Hash] = piece{kind: p.Kind, contentType: p.ContentType, raw: p.Raw}
		}
	}
	for obx := range touched {
		resp.HW[obx] = s.hw[outboxKey{src.id, obx}]
	}
	writeJSON(w, http.StatusOK, resp)
}

// judge decides one decoded-or-not envelope's status (package doc order).
func (s *Server) judge(src *source, it commitItem, inBatch map[wire.Hash]bool) (wire.Status, wire.Reason, []wire.Hash) {
	e := it.env
	switch {
	case it.err != nil:
		return wire.StatusRejected, wire.ReasonSchema, nil
	case int64(len(it.raw)) > s.limits.EnvelopeJSON:
		return wire.StatusRejected, wire.ReasonTooLarge, nil
	case e.MaskVersion != wire.MaskV1:
		return wire.StatusRejected, wire.ReasonMaskVersionMissing, nil
	case e.CanonVersion != wire.CanonV1:
		return wire.StatusRejected, wire.ReasonCanonVersion, nil
	}
	if _, err := wire.DecodeBody(e.Kind, e.Body); err != nil {
		return wire.StatusRejected, wire.ReasonSchema, nil // the body does not decode as its kind
	}
	var missing []wire.Hash
	for _, h := range e.Refs {
		if _, stored := s.pieces[h]; !inBatch[h] && !stored && !s.staged[h] {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return wire.StatusRetry, wire.ReasonMissingRef, missing
	}
	if reason, ok := s.rejects[e.Key]; ok {
		switch reason {
		case wire.ReasonMissingRef:
			return wire.StatusRetry, reason, append([]wire.Hash{}, e.Refs...)
		case wire.ReasonBusy, wire.ReasonStoreUnavailable:
			return wire.StatusRetry, reason, nil
		}
		return wire.StatusRejected, reason, nil
	}
	ns := ""
	if e.Kind.SourceScoped() {
		ns = src.id
	}
	k := nsKey{ns, e.Key}
	sum := e.BodySHA256()
	prev, ok := s.receipts[k]
	switch {
	case !ok:
		s.receipts[k] = receipt{bodySHA: sum}
		return wire.StatusOK, "", nil
	case prev.bodySHA == sum:
		return wire.StatusDuplicate, "", nil
	}
	return wire.StatusConflict, "", nil
}

func acked(st wire.Status) bool {
	return st == wire.StatusOK || st == wire.StatusDuplicate || st == wire.StatusConflict
}

// addReceipt records a receipt for (source, outbox, seq) and advances hw over contiguous receipts.
func (s *Server) addReceipt(sourceID, outbox string, seq uint64) {
	if !validOutbox(outbox) || seq < 1 {
		return
	}
	k := outboxKey{sourceID, outbox}
	if s.seqs[k] == nil {
		s.seqs[k] = map[uint64]bool{}
	}
	s.seqs[k][seq] = true
	for s.seqs[k][s.hw[k]+1] {
		s.hw[k]++
	}
}

// lenientPosition extracts seq and outbox_id from a record that failed the v1 decoder, so its
// rejected/schema result and receipt can name them. Zero values when even that fails.
func lenientPosition(raw []byte) (uint64, string) {
	var p struct {
		Seq      json.Number `json:"seq"`
		OutboxID any         `json:"outbox_id"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return 0, ""
	}
	seq, _ := strconv.ParseUint(p.Seq.String(), 10, 64)
	obx, _ := p.OutboxID.(string)
	return seq, obx
}

func validOutbox(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Server) heartbeat(w http.ResponseWriter, src *source, body []byte) {
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		badRequest(w, "bad_json")
		return
	}
	if head.V > wire.ProtocolV1 {
		badRequest(w, wire.ErrCodeUnsupportedProtocol)
		return
	}
	var hb wire.Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		badRequest(w, "bad_json")
		return
	}
	src.heartbeats = append(src.heartbeats, hb)
	writeJSON(w, http.StatusOK, wire.HeartbeatResponse{Epoch: s.epoch, ArchiveID: s.archiveID, ExactRev: s.exact.Rev, ServerTime: time.Now().UTC()})
}

func (s *Server) mediaFetch(w http.ResponseWriter, src *source, body []byte) {
	var mf wire.MediaFetch
	if err := json.Unmarshal(body, &mf); err != nil {
		badRequest(w, "bad_json")
		return
	}
	k := mf.RequestID + "\x00" + strconv.FormatUint(uint64(mf.Ordinal), 10)
	if !src.mediaSeen[k] {
		src.mediaSeen[k] = true
		src.media = append(src.media, mf)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) sessionCoverage(w http.ResponseWriter, body []byte) {
	var req wire.CoverageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		badRequest(w, "bad_json")
		return
	}
	if len(req.SessionIDs) > wire.MaxCoverageIDs {
		writeJSON(w, http.StatusRequestEntityTooLarge, wire.ErrorBody{Error: "too_large"})
		return
	}
	out := wire.CoverageResponse{Sessions: map[string]wire.Coverage{}}
	for _, id := range req.SessionIDs {
		c, ok := s.coverage[id]
		if !ok {
			c = wire.Coverage{Responses: []string{}}
		}
		out.Sessions[id] = c
	}
	writeJSON(w, http.StatusOK, out)
}

var machineName = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func (s *Server) putCaller(w http.ResponseWriter, r *http.Request, body []byte) {
	fp := strings.TrimPrefix(r.URL.Path, wire.PathCallerPrefix)
	if _, err := wire.ParseHash(fp); err != nil {
		badRequest(w, "bad_caller_fp")
		return
	}
	var req wire.CallerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		badRequest(w, "bad_json")
		return
	}
	if !machineName.MatchString(req.Machine) {
		badRequest(w, "bad_machine")
		return
	}
	if prev, ok := s.callers[fp]; ok && prev.Machine != req.Machine {
		writeJSON(w, http.StatusConflict, wire.ErrorBody{Error: "caller_named"})
		return
	}
	s.callers[fp] = req
	writeJSON(w, http.StatusOK, wire.CallerResponse{Machine: req.Machine})
}

// ---- ids ----

const (
	crockford = "0123456789abcdefghjkmnpqrstvwxyz"
	base62    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
