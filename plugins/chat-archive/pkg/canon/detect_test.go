package canon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestSplitOutputPassesDetect runs the exported split entry points with the real pkg/mask and
// pkg/headers, then applies the server's detect-only re-check to
// everything a producer would send: text pieces, every string and key of json pieces, masked text
// attachments, and every string and key of the wire Body (frames, V, ToolsV, hashes), all through the
// exported DetectPiece. Nothing may be found, or the server would reject the producer's own output as
// mask_violation.
func TestSplitOutputPassesDetect(t *testing.T) {
	m := realMasker(t)
	type tc struct {
		name     string
		in       func(e *env) (Input, []string) // the input and any secrets beyond e.canary
		opts     Options
		response bool
	}
	var cases []tc
	for _, c := range splitCases() {
		cases = append(cases, tc{name: c.name, opts: c.opts, response: c.response,
			in: func(e *env) (Input, []string) { return c.input(e), nil }})
	}
	// Secrets of several rules (github-pat, aws-access-token, ca-archive-token) in keys, nested
	// tool input, JSON-in-string arguments, the system prompt and volatile fields.
	ghp := func() string { return "ghp_" + randString(t, alnum62, 36) }
	akia := func() string { return "AKIA" + randString(t, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16) }
	cases = append(cases,
		tc{name: "claude-rule-canaries", in: func(e *env) (Input, []string) {
			g, a := ghp(), akia()
			body := `{"model":"claude-test","system":"deploy with ` + a + `",` +
				`"tools":[{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","note":"` + e.canary + `"}}],` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"token ` + g + ` then ` + e.canary + `"}]},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"t","input":{"` + g + `":{"nested":"` + a + `"}},` +
				`"cache_control":{"type":"ephemeral","k":"` + e.canary + `"}}]}]}`
			return Input{Format: FormatClaude, Body: []byte(body)}, []string{g, a}
		}},
		tc{name: "chat-rule-canaries", in: func(e *env) (Input, []string) {
			g, a := ghp(), akia()
			args, _ := json.Marshal(`{"key":"` + g + `","list":["` + a + `","` + e.canary + `"]}`)
			body := `{"model":"gpt-test","messages":[{"role":"system","content":"use ` + e.canary + `"},` +
				`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":` + string(args) + `}}]}]}`
			return Input{Format: FormatOpenAIChat, Body: []byte(body)}, []string{g, a}
		}},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			in, extra := c.in(e)
			opts := c.opts
			opts.Masker = m
			split := SplitRequest
			if c.response {
				split = SplitResponse
			}
			sp, err := split(context.Background(), in, opts)
			if err != nil {
				t.Fatal(err)
			}
			out := allOutput(sp)
			for _, s := range append([]string{e.canary}, extra...) {
				if bytes.Contains(out, []byte(s)) {
					t.Fatal("a secret reached the output")
				}
			}
			if len(extra) > 0 && len(sp.Hits) < 2 {
				t.Errorf("expected hits of several rules, got %v", sp.Hits)
			}
			for _, p := range sp.Pieces {
				f, err := DetectPiece(p.Kind, p.ContentType, p.Bytes, m)
				if err != nil || len(f) != 0 {
					t.Errorf("%s piece: DetectPiece found %v (err %v)", p.Kind, f, err)
				}
				// DetectPiece of a json piece is the per-string walk.
				if p.Kind == wire.PieceJSON {
					eachJSONString(t, p.Bytes, func(s string) {
						if f := m.Detect(s); len(f) != 0 {
							t.Errorf("json piece string: Detect found %v", f)
						}
					})
				}
			}
			body, err := json.Marshal(sp.Body())
			if err != nil {
				t.Fatal(err)
			}
			if f, err := DetectPiece(wire.PieceJSON, "", body, m); err != nil || len(f) != 0 {
				t.Errorf("body: DetectPiece found %v (err %v)", f, err)
			}
		})
	}
}

// eachJSONString calls f for every string token (object keys included) in a JSON document.
func eachJSONString(t *testing.T, b []byte, f func(string)) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("JSON does not decode: %v", err)
		}
		if s, ok := tok.(string); ok {
			f(s)
		}
	}
}

const alnum62 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func randString(t *testing.T, alphabet string, n int) string {
	t.Helper()
	var sb strings.Builder
	for range n {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteByte(alphabet[i.Int64()])
	}
	return sb.String()
}
