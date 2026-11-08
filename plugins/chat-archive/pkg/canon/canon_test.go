package canon

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestCanonGolden runs the canon/v1 definition goldens in testdata/canon: NAME.in canonicalizes to
// exactly NAME.out with the flags listed in NAME.flags (none when absent), and NAME.out is a fixed point.
func TestCanonGolden(t *testing.T) {
	ins, err := filepath.Glob(filepath.Join("testdata", "canon", "*.in"))
	if err != nil || len(ins) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, in := range ins {
		name := strings.TrimSuffix(in, ".in")
		t.Run(filepath.Base(name), func(t *testing.T) {
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(name + ".out")
			if err != nil {
				t.Fatal(err)
			}
			var wantFlags []string
			if fb, err := os.ReadFile(name + ".flags"); err == nil {
				wantFlags = strings.Fields(string(fb))
			}
			got, flags, err := Canonicalize(raw)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("got  %q\nwant %q", got, want)
			}
			if !slices.Equal(flags, wantFlags) {
				t.Errorf("flags %v, want %v", flags, wantFlags)
			}
			again, flags2, err := Canonicalize(got)
			if err != nil || !bytes.Equal(again, got) || len(flags2) != 0 {
				t.Errorf("output is not a fixed point: %q %v %v", again, flags2, err)
			}
		})
	}
}

func TestCanonicalizeRejects(t *testing.T) {
	for _, in := range []string{``, ` `, `{`, `{"a":}`, `{"a":1}{"b":2}`, `[1,]`, `01`, `"\x"`, "\"a\x01b\"",
		`{"a" 1}`, `[1] x`, `nul`, `"\ud800`} {
		if out, _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("Canonicalize(%q) = %q, want an error", in, out)
		}
	}
}

func TestMarshal(t *testing.T) {
	h := wire.Hash{1, 2, 3}
	tomb := wire.Tombstone{V: 1, OutboxID: strings.Repeat("a", 32), Seq: 7, Key: "ex/r1/begin/0", Reason: wire.TombMissingRef}
	got, err := Marshal(tomb)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"ex/r1/begin/0","outbox_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"missing_ref","seq":7,"v":1}`
	if string(got) != want {
		t.Errorf("Marshal tombstone\n got %s\nwant %s", got, want)
	}
	// encoding/json escapes <, > and & and U+2028; canon/v1 does not.
	got, err = Marshal(map[string]any{"z": "<a>& ", "a": []any{1.5, h}})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"a":[1.5,"0102030000000000000000000000000000000000000000000000000000000000"],"z":"<a>&` + " " + `"}`
	if string(got) != want {
		t.Errorf("Marshal map\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalizeMasked(t *testing.T) {
	e := newEnv(t)
	in := []byte(`{"note":"key ` + e.canary + ` here","` + e.canary + `":1,"n":[1,{"x":"` + e.canary + `"}]}`)
	got, hits, flags, err := canonicalizeMasked(in, e.m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"[masked:test-canary]":1,"n":[1,{"x":"[masked:test-canary]"}],"note":"key [masked:test-canary] here"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if hits["test-canary"] != 3 || len(flags) != 0 {
		t.Errorf("hits %v flags %v", hits, flags)
	}
	// Two keys that mask to the same string collide: the later one wins and dup_keys is set.
	in = []byte(`{"a` + e.canary + `":1,"a` + e.canary + `x":2,"a[masked:test-canary]":3}`)
	got, _, flags, err = canonicalizeMasked(in, e.m)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a[masked:test-canary]":3,"a[masked:test-canary]x":2}` || !slices.Equal(flags, []string{wire.FlagDupKeys}) {
		t.Errorf("collision: %s %v", got, flags)
	}
	if _, _, _, err := CanonicalizeMasked(in, nil); err == nil {
		t.Error("CanonicalizeMasked with a nil Masker must fail")
	}
}

// TestCanonicalizeMaskedReal checks the exported entry point with pkg/mask.
func TestCanonicalizeMaskedReal(t *testing.T) {
	m := realMasker(t)
	canary := newCanary(t)
	got, hits, _, err := CanonicalizeMasked([]byte(`{"t":"use `+canary+` now"}`), m)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(canary)) || len(hits) == 0 {
		t.Errorf("canary not masked: %s %v", got, hits)
	}
	if !bytes.Contains(got, []byte("[masked:ca-archive-token")) {
		t.Errorf("unexpected placeholder: %s", got)
	}
}

func TestNewPiece(t *testing.T) {
	p := NewPiece(wire.PieceText, "", []byte("hello"))
	if hx(p.Hash) != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || p.Kind != wire.PieceText {
		t.Errorf("NewPiece: %+v", p)
	}
}
