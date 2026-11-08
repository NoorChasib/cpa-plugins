package importer

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const (
	alnum     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alnumDash = alnum + "_-"
)

type canaryGen struct{ r *rand.Rand }

// s returns n random characters of alpha with at least one digit (several rules gate on one).
func (g canaryGen) s(alpha string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[g.r.IntN(len(alpha))]
	}
	b[g.r.IntN(n)] = "0123456789"[g.r.IntN(10)]
	return string(b)
}

// TestSecretCanaries: secrets in a transcript, a saved tool output, a workflow script, a Codex line, a
// base64 text document, a nested tool input, an escaped string, and an ops secret registered through
// the exact set in an env-dump line are masked in everything the fake archive received, raw,
// JSON-escaped, base64, percent-encoded and hex, while the ordinary markers beside them arrive. Values
// are generated at test time; failures name canary ids only.
func TestSecretCanaries(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	g := canaryGen{rand.New(rand.NewPCG(seed, 0x6361))}
	defer func() {
		if t.Failed() {
			t.Logf("canary seed %d", seed)
		}
	}()
	x := newH(t)
	secrets := map[string]string{}
	markers := map[string]string{}
	mark := func(id string) string {
		m := "cha-marker-" + g.s("0123456789abcdef", 10)
		markers[id] = m
		return m
	}

	// ops-exact: an ops secret registered by fingerprint (ARCHIVE_EXTRA_EXACT_FPS), in an env-dump line.
	ops := g.s(alnum, 32)
	secrets["ops-exact"] = ops
	x.srv.SetExact(3, []wire.Hash{fingerprint.ExactFP(ops)}, nil)
	x.enroll()

	c := x.claude(gSession)
	p1 := "sk-ant-api03-" + g.s(alnumDash, 93) + "AA"
	secrets["transcript"] = p1
	bearer := g.s(alnum, 40)
	secrets["nested-tool-input"] = bearer
	docTok := "ghp_" + g.s(alnum, 36)
	secrets["base64-doc"] = docTok
	doc := "# deploy notes\nset the token\nGITHUB_TOKEN=" + docTok + "\nend of document " + mark("base64-doc") + "\n"
	docB64 := base64.StdEncoding.EncodeToString([]byte(doc))
	oat := "sk-ant-oat01-" + g.s(alnum, 60)
	secrets["escaped"] = oat
	escaped := strings.ReplaceAll(oat, "-", `-`)

	userDoc := `{"type":"user","uuid":"` + x.g.UUID() + `","sessionId":"` + gSession + `","timestamp":"2026-09-01T10:00:00.000Z","message":{"role":"user","content":[` +
		`{"type":"text","text":"see the notes"},{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"` + docB64 + `"}}]}}`
	userEsc := `{"type":"user","uuid":"` + x.g.UUID() + `","sessionId":"` + gSession + `","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":"key ` + escaped + ` ` + mark("escaped") + `"}}`
	curl := `curl -sS -H "Authorization: Bearer ` + bearer + `" https://api.example.invalid/v1 ` + mark("nested-tool-input")
	x.write(x.mainFile(gProject, gSession),
		c.UserText("my key is "+p1+" "+mark("transcript")),
		userDoc,
		userEsc,
		c.ToolUse("msg_c", "toolu_c", "Bash", M{"command": curl, "nested": M{"deeper": []any{curl}}}, M{"command": curl, "nested": M{"deeper": []any{curl}}}),
		c.ToolResult("toolu_c", "$ env\nOPS_VALUE="+ops+"\n"+mark("ops-exact"), M{"stdout": "OPS_VALUE=" + ops, "interrupted": false}),
	)

	// saved-output: a saved tool output.
	k1, k2 := "sk-proj-"+g.s(alnumDash, 60), "xai-"+g.s(alnum, 60)
	secrets["saved-output-openai"], secrets["saved-output-xai"] = k1, k2
	sdir := filepath.Join(x.projects(), gProject, gSession)
	x.writeRaw(filepath.Join(sdir, "tool-results", "envdump.txt"),
		[]byte("$ env | sort\nHOME=/home/user\nOPENAI_API_KEY="+k1+"\nXAI_API_KEY="+k2+"\n"+mark("saved-output")+"\n"))
	// workflow-script: a workflow script no run embeds.
	k3 := "ghs_" + g.s(alnum, 36)
	secrets["workflow-script"] = k3
	x.writeRaw(filepath.Join(sdir, "workflows", "scripts", "deploy.js"),
		[]byte("steps:\n  - run: |\n      export GH_TOKEN="+k3+"\n      gh api /repos/example/example\n// "+mark("workflow-script")+"\n"))
	// codex-line: a Codex session line.
	k4 := "hf_" + g.s(alnum, 34)
	secrets["codex-line"] = k4
	cx := &synth.Codex{G: x.g, ThreadID: gThread, Session: gThread}
	x.write(filepath.Join(x.codex(), "2026", "10", "01", "rollout-c-"+gThread+".jsonl"),
		cx.SessionMeta("openai", "", 0),
		cx.CustomToolCall("call_c", "printenv HF_TOKEN"),
		cx.CustomToolCallOutput("call_c", "HF_TOKEN="+k4+"\n"+mark("codex-line")),
		cx.CommandExecution("HF_TOKEN="+k4+"\n"+synth.Text(1200, 9), true),
	)

	x.mustRun()
	got := x.envelopes()
	if len(got) < 10 {
		t.Fatalf("%d envelopes", len(got))
	}
	var all []byte
	for _, b := range x.srv.RequestBodies() {
		all = append(all, b...)
		all = append(all, '\n')
	}
	for _, r := range got { // envelopes and the pieces they reference
		for _, h := range r.Envelope.Refs {
			if _, raw, ok := x.srv.Piece(h); ok {
				all = append(all, raw...)
			}
		}
	}
	for id, s := range secrets {
		js, _ := json.Marshal(s)
		forms := map[string]string{
			"raw": s, "json": string(js[1 : len(js)-1]), "b64": base64.StdEncoding.EncodeToString([]byte(s)),
			"b64url": base64.RawURLEncoding.EncodeToString([]byte(s)), "pct": url.QueryEscape(s), "hex": hex.EncodeToString([]byte(s)),
		}
		for form, v := range forms {
			if bytes.Contains(all, []byte(v)) {
				t.Errorf("canary %s present (%s) in what the archive received", id, form)
			}
		}
	}
	if bytes.Contains(all, []byte(docB64)) {
		t.Error("canary base64-doc: the base64 document reached the archive undecoded")
	}
	for id, m := range markers {
		if !bytes.Contains(all, []byte(m)) {
			t.Errorf("marker %s missing from what the archive received", id)
		}
	}
	for _, ph := range []string{"[masked:ca-anthropic", "[masked:ca-github", "[masked:exact:", "[masked:ca-auth-header", "[masked:ca-hf", "[masked:ca-xai"} {
		if !bytes.Contains(all, []byte(ph)) {
			t.Errorf("placeholder %s missing", ph)
		}
	}
	// The importer's own state holds positions only.
	p := pathsFor(x.home)
	var st []byte
	for _, f := range []string{p.StateDB, p.StateDB + "-wal"} {
		b, _ := os.ReadFile(f)
		st = append(st, b...)
	}
	for id, s := range secrets {
		if bytes.Contains(st, []byte(s)) {
			t.Errorf("canary %s in the state file", id)
		}
	}
	for id, m := range markers {
		if bytes.Contains(st, []byte(m)) || strings.Contains(x.out.String(), m) || strings.Contains(x.errb.String(), m) {
			t.Errorf("content marker %s in state or output", id)
		}
	}
}
