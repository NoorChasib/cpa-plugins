package fingerprint_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
)

// Vectors were computed by running CPA v8.0.60's own CallerScope, copied verbatim with go/ast from
// /tmp/cpa-src-v8.0.60/sdk/cliproxy/session/identity.go into a throwaway program. Keys are synthetic.
func TestCallerScopeVectors(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"sk-cpa-synthetic-0001", "766a645e7e60750b6d4ec76f6eecc2d6a09305bcfa36996425051d29bda542a5"},
		{"  sk-cpa-synthetic-0001\t\n", "766a645e7e60750b6d4ec76f6eecc2d6a09305bcfa36996425051d29bda542a5"},
		{"x", "ecbc95151ba2708d993cbbe2b164e530963289a57548d2af1bf140b62052c74e"},
		{"", ""},
		{"   ", ""},
	} {
		if got := fingerprint.CallerScope(tc.key); got != tc.want {
			t.Errorf("CallerScope(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// cpaCallerScope is CPA v8.0.60's CallerScope as go/printer prints it. The test below fails if the
// pinned source changes shape, so the vectors above stay tied to CPA's real formula.
const cpaCallerScope = `func CallerScope(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + value))
	return hex.EncodeToString(sum[:])
}`

func TestCallerScopeMatchesCPASource(t *testing.T) {
	const path = "/tmp/cpa-src-v8.0.60/sdk/cliproxy/session/identity.go"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("CPA v8.0.60 source not present at %s", path)
	}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "CallerScope" || fd.Recv != nil {
			continue
		}
		found = true
		var b bytes.Buffer
		if err := printer.Fprint(&b, fs, fd); err != nil {
			t.Fatal(err)
		}
		if b.String() != cpaCallerScope {
			t.Fatalf("CPA CallerScope changed:\n%s", b.String())
		}
	}
	if !found {
		t.Fatal("CallerScope not found in CPA source")
	}
}

// HMAC, TokenHash and ExactFP vectors were computed independently with Python's hmac and hashlib.
func TestHMAC(t *testing.T) {
	key := []byte("synthetic-hmac-key-not-a-secret")
	for _, tc := range []struct {
		parts []string
		want  string
	}{
		{[]string{"claude", "user@example.invalid"}, "k1:e25b0164e407381072c5952b504f69e9ec7bd5c150f8fe1c4ca8c05a0d24aef2"},
		{[]string{"only"}, "k1:65e2c0562d46d21237bfa4e793a7d7c6e73a3e672b331a93574fd52c684dc6ed"},
		{nil, "k1:1a786d065d555c55536ec52eafd10dc7142c1a8925571f6bf074819fdd3f6d6a"},
	} {
		if got := fingerprint.HMAC("k1", key, tc.parts...); got != tc.want {
			t.Errorf("HMAC(k1, %q) = %q, want %q", tc.parts, got, tc.want)
		}
	}
	if a, b := fingerprint.HMAC("k1", key, "a|b"), fingerprint.HMAC("k1", key, "a", "b"); a != b {
		t.Errorf("parts are joined with |: %s != %s", a, b)
	}
	if a, b := fingerprint.HMAC("k1", key, "v"), fingerprint.HMAC("k2", key, "v"); a[3:] != b[3:] || a[:3] != "k1:" || b[:3] != "k2:" {
		t.Errorf("key id is a prefix only: %s, %s", a, b)
	}
	if a, b := fingerprint.HMAC("k1", key, "v"), fingerprint.HMAC("k1", []byte("other"), "v"); a == b {
		t.Error("different keys gave the same HMAC")
	}
}

func TestTokenHashAndExactFP(t *testing.T) {
	if got := hexOf(fingerprint.TokenHash("cha_src_synthetic")); got != "4ddb07a332867f84b6ba3ee894bf415931cc63417075e52335d68d4793b8b502" {
		t.Errorf("TokenHash = %s", got)
	}
	if got := hexOf(fingerprint.ExactFP("synthetic-value")); got != "d898e5f94041344c77a832b135ceb91df198c817fec777aba95fde1c32ab6f54" {
		t.Errorf("ExactFP = %s", got)
	}
	if fingerprint.TokenHash("v") == fingerprint.ExactFP("v") {
		t.Error("token hash and exact fingerprint share a domain")
	}
}

func hexOf(b [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, c := range b {
		out[2*i], out[2*i+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}
