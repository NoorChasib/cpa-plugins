// Package client is the HTTP client for every archive route producers call: the plugin, the importer
// and the server's tests. It is pure Go; it imports pkg/wire, pkg/canon and
// klauspost/compress/zstd.
//
// The client never retries a request. Every non-2xx answer becomes one typed error (errors.go), and
// the caller owns backoff; in particular a 401 (or a 403) is returned once as
// ErrUnauthorized and must not be retried in a loop.
//
// Every request is bounded. Commit (120 s) and piece PUT (300 s) always carry their own deadline;
// every other route gets a default deadline when the caller's context has none: 60 s for need, 30 s
// for the rest. The default transport also bounds the wait for response headers to 120 s. A caller's
// earlier deadline (for example the plugin's shutdown context) always wins. The default transport
// never uses a proxy: the archive is reached directly over WireGuard or the tailnet, and a proxy from
// the environment (HTTP_PROXY) would see the source token; pass Options.HTTPClient to use one.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Producer timeouts.
const (
	ConnectTimeout  = 5 * time.Second
	CommitTimeout   = 120 * time.Second
	PutPieceTimeout = 300 * time.Second
)

// Default deadlines of the other routes, applied only when the caller's context has none
// (variables so an internal test can shorten them).
var (
	needTimeout  = 60 * time.Second
	routeTimeout = 30 * time.Second
)

// responseHeaderTimeout bounds the default transport's wait for response headers.
const responseHeaderTimeout = CommitTimeout

// maxResponseBytes bounds a response body read. The largest legitimate answer is a need response of
// 32,768 hex hashes (about 2.2 MB).
const maxResponseBytes = 16 << 20

// Client calls the source-authenticated archive routes with one source token. It wraps an
// http.Client with the producer timeouts above and keep-alive, and is safe for concurrent use.
type Client struct {
	base    string
	token   string
	hc      *http.Client
	ua      string
	onState func(wire.ServerState)
}

// Options configure a Client.
type Options struct {
	// HTTPClient, when set, is used as is. Nil means a shared client whose transport has the
	// ConnectTimeout dial timeout, keep-alive, a 120 s response-header wait and no proxy.
	HTTPClient *http.Client
	// UserAgent is sent on every request when non-empty.
	UserAgent string
	// OnServerState, when set, is called with the parsed X-Archive-* headers of every response that
	// carries them. An epoch or archive id other than the one the producer last saw means the archive
	// was restored or replaced, and the producer replays.
	OnServerState func(wire.ServerState)
}

// defaultHTTPClient is shared by every Client built without Options.HTTPClient and by Enroll, so
// keep-alive connections are pooled once per process. It never uses a proxy.
var defaultHTTPClient = sync.OnceValue(func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: t}
})

// zstdEncoder is the producer encoder: level 3, window 8 MiB, concurrency 1. EncodeAll is safe for
// concurrent use; calls share the one encoder.
var zstdEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
		zstd.WithWindowSize(8<<20),
		zstd.WithEncoderConcurrency(1))
})

// New returns a Client for baseURL (scheme://host[:port], no trailing path) authenticating with token
// as a bearer token.
func New(baseURL, token string, opts Options) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = defaultHTTPClient()
	}
	return &Client{
		base:    strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      hc,
		ua:      opts.UserAgent,
		onState: opts.OnServerState,
	}
}

// request is one HTTP call.
type request struct {
	method  string
	path    string
	body    []byte
	header  http.Header
	timeout time.Duration // always applied (commit, piece PUT); 0: see fallback
	// fallback applies when timeout is 0 and the caller's context has no deadline; 0 means routeTimeout.
	fallback time.Duration
}

// response is a completed call with a 2xx status.
type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends req once. A non-2xx answer becomes the typed error of errors.go; transport errors are
// returned as they are. The route, never the token or a body, names the error.
func do(ctx context.Context, hc *http.Client, base, token, ua string, onState func(wire.ServerState), req request) (response, error) {
	d := req.timeout
	if _, ok := ctx.Deadline(); d == 0 && !ok {
		d = req.fallback
		if d == 0 {
			d = routeTimeout
		}
	}
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	var body io.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method, base+req.path, body)
	if err != nil {
		return response{}, fmt.Errorf("client: %s %s: %w", req.method, req.path, err)
	}
	for k, v := range req.header {
		hr.Header[k] = v
	}
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	if ua != "" {
		hr.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return response{}, fmt.Errorf("client: %s %s: %w", req.method, req.path, err)
	}
	defer resp.Body.Close()
	if onState != nil {
		if st, ok := wire.ParseServerState(resp.Header); ok {
			onState(st)
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return response{}, fmt.Errorf("client: %s %s: read response: %w", req.method, req.path, err)
	}
	if len(b) > maxResponseBytes {
		return response{}, fmt.Errorf("client: %s %s: response over %d bytes", req.method, req.path, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return response{}, fmt.Errorf("client: %s %s: %w", req.method, req.path, statusError(resp.StatusCode, resp.Header, b))
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

func (c *Client) do(ctx context.Context, req request) (response, error) {
	return do(ctx, c.hc, c.base, c.token, c.ua, c.onState, req)
}

// jsonRequest encodes v as the body of a JSON request.
func jsonRequest(method, path string, v any) (request, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return request{}, fmt.Errorf("client: %s %s: encode request: %w", method, path, err)
	}
	return request{method: method, path: path, body: b, header: http.Header{"Content-Type": {"application/json"}}}, nil
}

// decode parses a 2xx JSON body into v.
func decode(req request, resp response, v any) error {
	if err := json.Unmarshal(resp.body, v); err != nil {
		return fmt.Errorf("client: %s %s: decode response: %w", req.method, req.path, err)
	}
	return nil
}

// Enroll calls POST /v1/enroll with an enrollment code in the body and no token.
// A bad, expired or used code is ErrUnauthorized.
func Enroll(ctx context.Context, baseURL string, req wire.EnrollRequest) (wire.EnrollResponse, error) {
	r, err := jsonRequest(http.MethodPost, wire.PathEnroll, req)
	if err != nil {
		return wire.EnrollResponse{}, err
	}
	resp, err := do(ctx, defaultHTTPClient(), strings.TrimRight(baseURL, "/"), "", "", nil, r)
	if err != nil {
		return wire.EnrollResponse{}, err
	}
	var out wire.EnrollResponse
	return out, decode(r, resp, &out)
}

// SourcesSelf calls GET /v1/sources/self.
func (c *Client) SourcesSelf(ctx context.Context) (wire.SourcesSelf, error) {
	r := request{method: http.MethodGet, path: wire.PathSourcesSelf}
	resp, err := c.do(ctx, r)
	if err != nil {
		return wire.SourcesSelf{}, err
	}
	var out wire.SourcesSelf
	return out, decode(r, resp, &out)
}

// Need calls POST /v1/ingest/need with at most limits.need_hashes hashes and returns the hashes the
// server needs, those present only in the staging tier, and the response's ServerState.
func (c *Client) Need(ctx context.Context, hashes []wire.Hash) (need, staged []wire.Hash, st wire.ServerState, err error) {
	if hashes == nil {
		hashes = []wire.Hash{}
	}
	r, err := jsonRequest(http.MethodPost, wire.PathNeed, wire.NeedRequest{Pieces: hashes})
	if err != nil {
		return nil, nil, wire.ServerState{}, err
	}
	r.fallback = needTimeout
	resp, err := c.do(ctx, r)
	if err != nil {
		return nil, nil, wire.ServerState{}, err
	}
	st, _ = wire.ParseServerState(resp.header)
	var out wire.NeedResponse
	if err := decode(r, resp, &out); err != nil {
		return nil, nil, st, err
	}
	return out.Need, out.Staged, st, nil
}

// PutPiece calls PUT /v1/pieces/{sha256} with raw zstd-compressed (level 3, window <= 8 MiB, encoder
// concurrency 1), X-Archive-Piece-Kind set from kind, and Content-Type set to contentType for
// attachments. 201 and 200 both return nil.
func (c *Client) PutPiece(ctx context.Context, h wire.Hash, kind wire.PieceKind, contentType string, raw []byte) error {
	enc, err := zstdEncoder()
	if err != nil {
		return fmt.Errorf("client: zstd encoder: %w", err)
	}
	return c.putPiece(ctx, h, kind, contentType, enc.EncodeAll(raw, make([]byte, 0, len(raw)/2+64)))
}

// PutPieceZstd is PutPiece for a piece the caller already holds zstd-compressed (for example the
// plugin's outbox, which stores pieces in the same format: one zstd frame, level 3, window <= 8 MiB).
// zstdBody is sent as given, never decoded or re-encoded, so uploading a large piece holds no second
// copy of it. The route, headers, timeout and error mapping are PutPiece's. The server decodes the
// body and checks its sha256 against h, so a body that decodes to other bytes is a 400
// piece_hash_mismatch (ErrMalformed).
func (c *Client) PutPieceZstd(ctx context.Context, h wire.Hash, kind wire.PieceKind, contentType string, zstdBody []byte) error {
	return c.putPiece(ctx, h, kind, contentType, zstdBody)
}

// putPiece sends one zstd-encoded piece body to PUT /v1/pieces/{sha256}.
func (c *Client) putPiece(ctx context.Context, h wire.Hash, kind wire.PieceKind, contentType string, body []byte) error {
	r := request{
		method:  http.MethodPut,
		path:    wire.PathPiecePrefix + h.String(),
		body:    body,
		header:  http.Header{"Content-Encoding": {"zstd"}, wire.HeaderPieceKind: {kind.String()}},
		timeout: PutPieceTimeout,
	}
	if kind == wire.PieceAttachment && contentType != "" {
		r.header.Set("Content-Type", contentType)
	}
	_, err := c.do(ctx, r)
	return err
}

// Commit calls POST /v1/ingest/commit with one CAB1 batch inside one zstd stream (level 3, window
// <= 8 MiB, encoder concurrency 1), headers X-Archive-Epoch-Seen and X-Archive-Archive-Id.
// envs are canon/v1 envelope bytes; tombs are encoded with canon.Marshal.
func (c *Client) Commit(ctx context.Context, epochSeen uint64, archiveID string, pieces []wire.BatchPiece, envs [][]byte, tombs []wire.Tombstone) (wire.CommitResponse, error) {
	batch := wire.Batch{Pieces: pieces, Envelopes: envs}
	for _, t := range tombs {
		b, err := canon.Marshal(t)
		if err != nil {
			return wire.CommitResponse{}, fmt.Errorf("client: encode tombstone seq %d: %w", t.Seq, err)
		}
		batch.Tombstones = append(batch.Tombstones, b)
	}
	var raw bytes.Buffer
	raw.Grow(wire.CAB1Size(batch))
	if err := wire.WriteCAB1(&raw, batch); err != nil {
		return wire.CommitResponse{}, fmt.Errorf("client: encode batch: %w", err)
	}
	enc, err := zstdEncoder()
	if err != nil {
		return wire.CommitResponse{}, fmt.Errorf("client: zstd encoder: %w", err)
	}
	r := request{
		method: http.MethodPost,
		path:   wire.PathCommit,
		body:   enc.EncodeAll(raw.Bytes(), make([]byte, 0, raw.Len()/2+64)),
		header: http.Header{
			"Content-Type":       {wire.ContentTypeBatchV1},
			"Content-Encoding":   {"zstd"},
			wire.HeaderEpochSeen: {strconv.FormatUint(epochSeen, 10)},
			wire.HeaderArchiveID: {archiveID},
		},
		timeout: CommitTimeout,
	}
	resp, err := c.do(ctx, r)
	if err != nil {
		return wire.CommitResponse{}, err
	}
	var out wire.CommitResponse
	return out, decode(r, resp, &out)
}

// Heartbeat calls POST /v1/heartbeat.
func (c *Client) Heartbeat(ctx context.Context, hb wire.Heartbeat) (wire.HeartbeatResponse, error) {
	r, err := jsonRequest(http.MethodPost, wire.PathHeartbeat, hb)
	if err != nil {
		return wire.HeartbeatResponse{}, err
	}
	resp, err := c.do(ctx, r)
	if err != nil {
		return wire.HeartbeatResponse{}, err
	}
	var out wire.HeartbeatResponse
	return out, decode(r, resp, &out)
}

// MediaFetch calls POST /v1/media-fetch; 202 returns nil.
func (c *Client) MediaFetch(ctx context.Context, mf wire.MediaFetch) error {
	r, err := jsonRequest(http.MethodPost, wire.PathMediaFetch, mf)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, r)
	return err
}

// SessionCoverage calls POST /v1/ingest/session-coverage with at most wire.MaxCoverageIDs ids and
// returns the sessions map.
func (c *Client) SessionCoverage(ctx context.Context, ids []string) (map[string]wire.Coverage, error) {
	if ids == nil {
		ids = []string{}
	}
	r, err := jsonRequest(http.MethodPost, wire.PathSessionCoverage, wire.CoverageRequest{SessionIDs: ids})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	var out wire.CoverageResponse
	return out.Sessions, decode(r, resp, &out)
}

// PutCaller calls PUT /v1/callers/{caller_fp} once (the importer's register-caller). A 409 (already
// named differently) is a *StatusError with StatusCode 409.
func (c *Client) PutCaller(ctx context.Context, fp string, req wire.CallerRequest) error {
	r, err := jsonRequest(http.MethodPut, wire.PathCallerPrefix+url.PathEscape(fp), req)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, r)
	return err
}
