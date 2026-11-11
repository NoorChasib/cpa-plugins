package clienttest_test

// Raw-HTTP behaviour of the fake archive that pkg/client never produces on its own: missing tokens,
// bad zstd, bad CAB1 framing, unknown routes, header presence. All data is synthetic.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type fake struct {
	t     *testing.T
	srv   *clienttest.Server
	id    string
	token string
}

func newFake(t *testing.T) *fake {
	srv := clienttest.NewServer(clienttest.Options{Epoch: 2, ArchiveID: "00000000-0000-4000-8000-0000000000c1"})
	t.Cleanup(srv.Close)
	id, tok := srv.AddSource("importer", "test-importer")
	return &fake{t: t, srv: srv, id: id, token: tok}
}

func (f *fake) do(method, path, token string, body []byte, header map[string]string) (*http.Response, wire.ErrorBody) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var eb wire.ErrorBody
	_ = json.Unmarshal(b, &eb)
	return resp, eb
}

func zst(t *testing.T, b []byte) []byte {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

func commitHeaders() map[string]string {
	return map[string]string{
		"Content-Type": wire.ContentTypeBatchV1, "Content-Encoding": "zstd",
		wire.HeaderEpochSeen: "2", wire.HeaderArchiveID: "00000000-0000-4000-8000-0000000000c1",
	}
}

func TestAuthAndStateHeaders(t *testing.T) {
	f := newFake(t)
	for _, tok := range []string{"", "cha_src_wrong"} {
		resp, eb := f.do(http.MethodGet, wire.PathSourcesSelf, tok, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized || eb.Error == "" {
			t.Errorf("token %q: %d %+v", tok, resp.StatusCode, eb)
		}
		if resp.Header.Get(wire.HeaderEpoch) != "" {
			t.Errorf("401 carries epoch headers")
		}
	}
	resp, _ := f.do(http.MethodGet, wire.PathSourcesSelf, f.token, nil, nil)
	st, ok := wire.ParseServerState(resp.Header)
	if resp.StatusCode != 200 || !ok || st.Epoch != 2 || st.ArchiveID != "00000000-0000-4000-8000-0000000000c1" || st.ServerTime.IsZero() {
		t.Errorf("self: %d %+v %v", resp.StatusCode, st, ok)
	}
	// Error answers on source routes carry the headers too.
	resp, _ = f.do(http.MethodPost, wire.PathNeed, f.token, []byte(`nope`), nil)
	if resp.StatusCode != 400 || resp.Header.Get(wire.HeaderEpoch) != "2" {
		t.Errorf("bad need: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = f.do(http.MethodGet, wire.PathHealthz, "", nil, nil)
	if resp.StatusCode != 200 {
		t.Errorf("healthz %d", resp.StatusCode)
	}
	resp, _ = f.do(http.MethodGet, "/v1/admin/sources", f.token, nil, nil)
	if resp.StatusCode != 404 {
		t.Errorf("admin route %d", resp.StatusCode)
	}
}

func TestCommitFraming(t *testing.T) {
	f := newFake(t)
	cases := map[string]struct {
		body   []byte
		header map[string]string
		status int
		code   string
	}{
		"bad zstd":         {[]byte("not zstd at all"), commitHeaders(), 400, ""},
		"bad magic":        {zst(t, []byte("CAB2\xff\x00\x00\x00\x00")), commitHeaders(), 400, "bad_cab1"},
		"unknown type":     {zst(t, []byte("CAB1\x04\x00\x00\x00\x00\xff\x00\x00\x00\x00")), commitHeaders(), 400, "bad_cab1"},
		"missing end":      {zst(t, []byte("CAB1")), commitHeaders(), 400, "bad_cab1"},
		"epoch seen stale": {zst(t, []byte("CAB1\xff\x00\x00\x00\x00")), map[string]string{"Content-Encoding": "zstd", wire.HeaderEpochSeen: "1", wire.HeaderArchiveID: "00000000-0000-4000-8000-0000000000c1"}, 409, wire.ErrCodeEpochChanged},
		"no epoch headers": {zst(t, []byte("CAB1\xff\x00\x00\x00\x00")), map[string]string{"Content-Encoding": "zstd"}, 409, wire.ErrCodeEpochChanged},
		"empty batch":      {zst(t, []byte("CAB1\xff\x00\x00\x00\x00")), commitHeaders(), 200, ""},
		"uncompressed":     {[]byte("CAB1\xff\x00\x00\x00\x00"), map[string]string{wire.HeaderEpochSeen: "2", wire.HeaderArchiveID: "00000000-0000-4000-8000-0000000000c1"}, 200, ""},
	}
	for name, c := range cases {
		resp, eb := f.do(http.MethodPost, wire.PathCommit, f.token, c.body, c.header)
		if resp.StatusCode != c.status || (c.code != "" && eb.Error != c.code) {
			t.Errorf("%s: %d %+v", name, resp.StatusCode, eb)
		}
		if c.status == 409 && (eb.Epoch != 2 || eb.ArchiveID != "00000000-0000-4000-8000-0000000000c1") {
			t.Errorf("%s: epoch body %+v", name, eb)
		}
	}
	// Bodies are recorded decompressed (canary scans read them).
	var sawCAB1 bool
	for _, b := range f.srv.RequestBodies() {
		if bytes.HasPrefix(b, []byte("CAB1\xff")) {
			sawCAB1 = true
		}
	}
	if !sawCAB1 {
		t.Errorf("no decompressed CAB1 body recorded")
	}
}

func TestPieceRoute(t *testing.T) {
	f := newFake(t)
	raw := []byte("synthetic piece")
	h := wire.Hash(sha256.Sum256(raw))
	put := func(path string, body []byte, kind string) int {
		resp, _ := f.do(http.MethodPut, path, f.token, zst(t, body), map[string]string{"Content-Encoding": "zstd", wire.HeaderPieceKind: kind})
		return resp.StatusCode
	}
	if s := put(wire.PathPiecePrefix+strings.ToUpper(h.String()), raw, "text"); s != 400 {
		t.Errorf("upper-case hash: %d", s)
	}
	if s := put(wire.PathPiecePrefix+h.String(), raw, "blob"); s != 400 {
		t.Errorf("bad kind: %d", s)
	}
	if s := put(wire.PathPiecePrefix+h.String(), raw, "text"); s != 201 {
		t.Errorf("first put: %d", s)
	}
	if s := put(wire.PathPiecePrefix+h.String(), raw, "text"); s != 200 {
		t.Errorf("second put: %d", s)
	}
	if kind, got, ok := f.srv.Piece(h); !ok || kind != wire.PieceText || !bytes.Equal(got, raw) {
		t.Errorf("stored %v %q %v", kind, got, ok)
	}
}

// TestSetLimits: SetLimits changes, mid-test, the limits sources/self returns and the piece limit PUT
// enforces; the zero value restores wire.DefaultLimits.
func TestSetLimits(t *testing.T) {
	f := newFake(t)
	self := func() wire.Limits {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, f.srv.URL+wire.PathSourcesSelf, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s wire.SourcesSelf
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		return s.Limits
	}
	put := func(n int) int {
		raw := bytes.Repeat([]byte("p"), n)
		h := wire.Hash(sha256.Sum256(raw))
		resp, _ := f.do(http.MethodPut, wire.PathPiecePrefix+h.String(), f.token, raw, map[string]string{wire.HeaderPieceKind: "text"})
		return resp.StatusCode
	}
	if got := self(); got != wire.DefaultLimits {
		t.Fatalf("initial limits %+v", got)
	}
	lim := wire.DefaultLimits
	lim.TextPiece = 8 << 10
	f.srv.SetLimits(lim)
	if got := self(); got != lim {
		t.Fatalf("limits after SetLimits %+v", got)
	}
	if s := put(8 << 10); s != 201 {
		t.Errorf("put at text_piece: %d", s)
	}
	if s := put(8<<10 + 1); s != 413 {
		t.Errorf("put over text_piece: %d", s)
	}
	lim.TextPiece = 9 << 10
	f.srv.SetLimits(lim)
	if s := put(8<<10 + 1); s != 201 {
		t.Errorf("put after the raise: %d", s)
	}
	f.srv.SetLimits(wire.Limits{})
	if got := self(); got != wire.DefaultLimits {
		t.Errorf("limits after SetLimits(zero) %+v", got)
	}
}

func TestEnrollAndHeartbeatEdges(t *testing.T) {
	f := newFake(t)
	code := f.srv.NewEnrollCode()
	body, _ := json.Marshal(wire.EnrollRequest{Code: code, Kind: "cpa-plugin"})
	if resp, _ := f.do(http.MethodPost, wire.PathEnroll, "", body, nil); resp.StatusCode != 400 {
		t.Errorf("enroll kind cpa-plugin: %d", resp.StatusCode)
	}
	if resp, eb := f.do(http.MethodPost, wire.PathHeartbeat, f.token, []byte(`{"v":2}`), nil); resp.StatusCode != 400 || eb.Error != wire.ErrCodeUnsupportedProtocol {
		t.Errorf("heartbeat v2: %d %+v", resp.StatusCode, eb)
	}
	big := []byte(`{"v":1,"faults":["` + strings.Repeat("x", 70<<10) + `"]}`)
	if resp, _ := f.do(http.MethodPost, wire.PathHeartbeat, f.token, big, nil); resp.StatusCode != 413 {
		t.Errorf("heartbeat over 64 KiB: %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodPost, wire.PathHeartbeat, f.token, zst(t, []byte(`{"v":1,"n":5}`)), map[string]string{"Content-Encoding": "zstd"}); resp.StatusCode != 200 {
		t.Errorf("zstd heartbeat: %d", resp.StatusCode)
	}
	if hb := f.srv.Heartbeats(f.id); len(hb) != 1 || hb[0].N != 5 {
		t.Errorf("heartbeats %+v", hb)
	}
	mf := []byte(`{"request_id":"r","url":"` + strings.Repeat("u", 17<<10) + `"}`)
	if resp, _ := f.do(http.MethodPost, wire.PathMediaFetch, f.token, mf, nil); resp.StatusCode != 413 {
		t.Errorf("media fetch over 16 KiB: %d", resp.StatusCode)
	}
}
