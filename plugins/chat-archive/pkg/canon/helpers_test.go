package canon

// Test helpers. All test data is synthetic and invented. Secret canaries are generated per run
// (newCanary) and never committed; test inputs reference them as {{canary}}.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

const canaryPlaceholder = "[masked:test-canary]"

// testMasker masks exact registered strings only, so goldens are deterministic and independent of
// pkg/mask's rule set. It is idempotent like mask/v1.
type testMasker struct{ secrets []string }

func (t testMasker) String(s string) (string, mask.Hits) {
	n := 0
	for _, sec := range t.secrets {
		if c := strings.Count(s, sec); c > 0 {
			s = strings.ReplaceAll(s, sec, canaryPlaceholder)
			n += c
		}
	}
	if n == 0 {
		return s, nil
	}
	return s, mask.Hits{"test-canary": uint32(n)}
}

// fakeStripURL mirrors headers.StripURL without masking: scheme://host/path.
func fakeStripURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String() + u.EscapedPath() // as headers.StripURL, unmasked
}

// newCanary returns a fresh secret-shaped value that mask/v1's ca-archive-token rule masks. It is
// random per run and never a real token.
func newCanary(t testing.TB) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	sb.WriteString("cha_src_")
	for range 43 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteByte(alphabet[n.Int64()])
	}
	return sb.String()
}

// env is one test run's synthetic environment.
type env struct {
	t      testing.TB
	canary string
	m      stringMasker
}

func newEnv(t testing.TB) *env {
	c := newCanary(t)
	return &env{t: t, canary: c, m: testMasker{secrets: []string{c}}}
}

func (e *env) splitter(opts Options) *splitter {
	return newSplitter(context.Background(), e.m, fakeStripURL, opts)
}

func (e *env) request(in Input, opts Options) *Split {
	e.t.Helper()
	sp, err := e.splitter(opts).split(in, false)
	if err != nil {
		e.t.Fatalf("split request: %v", err)
	}
	return sp
}

func (e *env) response(in Input, opts Options) *Split {
	e.t.Helper()
	sp, err := e.splitter(opts).split(in, true)
	if err != nil {
		e.t.Fatalf("split response: %v", err)
	}
	return sp
}

func (e *env) messages(format string, msgs []byte, opts Options) *Split {
	e.t.Helper()
	sp, err := e.splitter(opts).messages(format, msgs)
	if err != nil {
		e.t.Fatalf("split messages: %v", err)
	}
	return sp
}

// synth returns n deterministic synthetic bytes of a kind: png, pdf, audio and bin are binary (not
// valid UTF-8); text is plain ASCII lines; secrettext is text with the run's canary in the middle.
func (e *env) synth(kind string, n int) []byte {
	var b []byte
	switch kind {
	case "png":
		b = []byte("\x89PNG\r\n\x1a\n")
	case "pdf":
		b = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	case "audio":
		b = []byte("ID3\x04\x00\x00")
	case "bin":
		b = []byte{0xff, 0xfe, 0x00}
	case "text", "secrettext":
		var sb strings.Builder
		for i := 0; sb.Len() < n; i++ {
			fmt.Fprintf(&sb, "synthetic line %04d: amber kites drift over quiet hills.\n", i)
			if kind == "secrettext" && i == 3 {
				sb.WriteString("deploy credential " + e.canary + " end\n")
			}
		}
		s := sb.String()
		if kind == "text" {
			return []byte(s[:n])
		}
		return []byte(s) // keep the canary whole
	default:
		e.t.Fatalf("unknown synth kind %q", kind)
	}
	for i := 0; len(b) < n; i++ {
		b = append(b, byte(i*37+11))
	}
	return b[:n]
}

var placeholderRE = regexp.MustCompile(`\{\{([a-z0-9:/.+_-]+)\}\}`)

// expand replaces {{canary}}, {{b64:KIND:N}}, {{dataurl:MIME:KIND:N}} and {{text:KIND:N}} (the raw
// synthetic text, for JSON-string embedding of plain text) in a test input.
func (e *env) expand(in []byte) []byte {
	return placeholderRE.ReplaceAllFunc(in, func(m []byte) []byte {
		parts := strings.Split(string(m[2:len(m)-2]), ":")
		switch {
		case parts[0] == "canary":
			return []byte(e.canary)
		case parts[0] == "b64" && len(parts) == 3:
			return []byte(base64.StdEncoding.EncodeToString(e.synth(parts[1], atoi(e.t, parts[2]))))
		case parts[0] == "dataurl" && len(parts) == 4:
			return []byte("data:" + parts[1] + ";base64," +
				base64.StdEncoding.EncodeToString(e.synth(parts[2], atoi(e.t, parts[3]))))
		}
		e.t.Fatalf("bad placeholder %s", m)
		return nil
	})
}

func atoi(t testing.TB, s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) readInput(name string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "split", name))
	if err != nil {
		e.t.Fatal(err)
	}
	return e.expand(b)
}

// ---- golden dumps ----

type goldenItem struct {
	ID string          `json:"id"`
	M  json.RawMessage `json:"m,omitempty"`
	F  json.RawMessage `json:"f,omitempty"`
	P  string          `json:"p,omitempty"`
	V  json.RawMessage `json:"v,omitempty"`
}

type goldenPiece struct {
	Hash        string          `json:"hash"`
	Kind        string          `json:"kind"`
	ContentType string          `json:"content_type,omitempty"`
	Len         int             `json:"len"`
	JSON        json.RawMessage `json:"json,omitempty"`
	Text        *string         `json:"text,omitempty"`
}

type goldenSplit struct {
	Format    string            `json:"format"`
	RawLen    int64             `json:"raw_len"`
	Skeleton  string            `json:"skeleton,omitempty"`
	Tools     string            `json:"tools,omitempty"`
	ToolsV    json.RawMessage   `json:"tools_v,omitempty"`
	System    []goldenItem      `json:"system,omitempty"`
	Items     []goldenItem      `json:"items,omitempty"`
	N         int               `json:"n"`
	ListSHA   string            `json:"list_sha"`
	Refs      []string          `json:"refs,omitempty"`
	Pieces    []goldenPiece     `json:"pieces,omitempty"`
	Flags     []string          `json:"flags,omitempty"`
	Hits      map[string]uint32 `json:"hits,omitempty"`
	Oversize  int               `json:"oversize,omitempty"`
	MediaURLs []string          `json:"media_urls_masked,omitempty"`
}

func hx(h wire.Hash) string { return hex.EncodeToString(h[:]) }

func hp(h *wire.Hash) string {
	if h == nil {
		return ""
	}
	return hx(*h)
}

func dumpItems(items []wire.Item) []goldenItem {
	var out []goldenItem
	for _, it := range items {
		out = append(out, goldenItem{ID: hx(ItemID(it)), M: it.M, F: it.F, P: hp(it.P), V: it.V})
	}
	return out
}

func kindName(k wire.PieceKind) string {
	switch k {
	case wire.PieceJSON:
		return "json"
	case wire.PieceText:
		return "text"
	case wire.PieceAttachment:
		return "attachment"
	}
	return "?"
}

func dumpSplit(sp *Split) []byte {
	g := goldenSplit{Format: sp.Format, RawLen: sp.RawLen, Skeleton: hp(sp.Skeleton), Tools: hp(sp.Tools),
		ToolsV: sp.ToolsV, System: dumpItems(sp.System), Items: dumpItems(sp.Items), N: len(sp.Items),
		ListSHA: hx(ListSHA(ItemIDs(sp.Items))), Flags: sp.Flags, Hits: sp.Hits, Oversize: sp.Oversize}
	for _, h := range sp.Refs(0) {
		g.Refs = append(g.Refs, hx(h))
	}
	for _, p := range sp.Pieces {
		gp := goldenPiece{Hash: hx(p.Hash), Kind: kindName(p.Kind), ContentType: p.ContentType, Len: len(p.Bytes)}
		switch {
		case p.Kind == wire.PieceJSON:
			gp.JSON = p.Bytes
		case utf8.Valid(p.Bytes):
			s := string(p.Bytes)
			gp.Text = &s
		}
		g.Pieces = append(g.Pieces, gp)
	}
	for _, u := range sp.MediaURLs {
		g.MediaURLs = append(g.MediaURLs, u.MaskedURL)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func readTestdata(path string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", path)) }

// checkGolden compares got with testdata/<path>, or rewrites it with -update.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	full := filepath.Join("testdata", path)
	if *update {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden; got:\n%s", path, got)
	}
}

// checkInvariants verifies what every Split must satisfy: piece hashes, refs equal pieces, every $att
// and $opaque stub well formed, and every stub's attachment present unless dropped.
func checkInvariants(t testing.TB, sp *Split) {
	t.Helper()
	have := make(map[wire.Hash]Piece)
	for _, p := range sp.Pieces {
		if sha256.Sum256(p.Bytes) != p.Hash {
			t.Errorf("piece %s: hash does not match bytes", hx(p.Hash))
		}
		if _, dup := have[p.Hash]; dup {
			t.Errorf("piece %s listed twice", hx(p.Hash))
		}
		have[p.Hash] = p
		if p.Kind == wire.PieceJSON {
			if out, _, err := Canonicalize(p.Bytes); err != nil || !bytes.Equal(out, p.Bytes) {
				t.Errorf("json piece %s is not canon/v1", hx(p.Hash))
			}
		}
	}
	refs := sp.Refs(0)
	if len(refs) != len(sp.Pieces) {
		t.Errorf("Refs(0) has %d hashes, Pieces %d", len(refs), len(sp.Pieces))
	}
	for i, h := range refs {
		if i < len(sp.Pieces) && sp.Pieces[i].Hash != h {
			t.Errorf("Pieces[%d] is not Refs(0)[%d]", i, i)
		}
	}
	checkRef := func(h *wire.Hash, what string) {
		if h != nil {
			if _, ok := have[*h]; !ok {
				t.Errorf("%s %s has no piece", what, hx(*h))
			}
		}
	}
	checkRef(sp.Skeleton, "skeleton")
	checkRef(sp.Tools, "tools")
	for _, it := range append(append([]wire.Item{}, sp.System...), sp.Items...) {
		checkRef(it.P, "item payload")
		for _, raw := range [][]byte{it.M, it.F, it.V} {
			if len(raw) > 0 {
				if out, _, err := Canonicalize(raw); err != nil || !bytes.Equal(out, raw) {
					t.Errorf("frame is not canon/v1: %s", raw)
				}
				checkStubs(t, raw, have)
			}
		}
		if it.P != nil {
			if p := have[*it.P]; p.Kind == wire.PieceJSON {
				checkStubs(t, p.Bytes, have)
			}
		}
	}
	if sp.Skeleton != nil {
		checkStubs(t, have[*sp.Skeleton].Bytes, have)
	}
}

func checkStubs(t testing.TB, raw []byte, have map[wire.Hash]Piece) {
	t.Helper()
	// UseNumber: canon/v1 keeps number literals verbatim, so a valid literal such as 1e400 must not
	// fail a float64 decode here.
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Errorf("unmarshal: %v", err)
		return
	}
	var visit func(any)
	visit = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			if a, ok := x[AttKey].(map[string]any); ok {
				h, err := wire.ParseHash(a["sha256"].(string))
				if err != nil {
					t.Errorf("$att sha256: %v", err)
					return
				}
				p, present := have[h]
				if a["dropped"] == nil {
					if !present || p.Kind != wire.PieceAttachment {
						t.Errorf("$att %s has no attachment piece", hx(h))
					} else if n, err := a["len"].(json.Number).Int64(); err != nil || n != int64(len(p.Bytes)) || a["mime"] != p.ContentType {
						t.Errorf("$att %s len/mime differ from the piece", hx(h))
					}
				} else if present {
					t.Errorf("dropped $att %s has a piece", hx(h))
				}
			}
			if o, ok := x[OpaqueKey].(map[string]any); ok {
				if _, err := wire.ParseHash(o["sha256"].(string)); err != nil {
					t.Errorf("$opaque sha256: %v", err)
				}
			}
			for _, v := range x {
				visit(v)
			}
		case []any:
			for _, v := range x {
				visit(v)
			}
		}
	}
	visit(v)
}

// allOutput returns every byte a Split would send: frames, V, ToolsV and all pieces.
func allOutput(sp *Split) []byte {
	var b bytes.Buffer
	b.Write(sp.ToolsV)
	for _, it := range append(append([]wire.Item{}, sp.System...), sp.Items...) {
		b.Write(it.M)
		b.Write(it.F)
		b.Write(it.V)
	}
	for _, p := range sp.Pieces {
		b.Write(p.Bytes)
	}
	for _, u := range sp.MediaURLs {
		b.WriteString(u.MaskedURL)
	}
	return b.Bytes()
}

// realMasker returns a pkg/mask Masker with an empty exact set.
func realMasker(t testing.TB) *mask.Masker {
	t.Helper()
	m, err := mask.New(mask.Config{})
	if err != nil {
		t.Fatalf("mask.New: %v", err)
	}
	return m
}
