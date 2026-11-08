package mask_test

// The mask/v1 canary corpus. Every value is
// synthetic, generated at test time with a random suffix that satisfies its rule. Failures name the
// canary id only, never the value.

import (
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
)

const (
	alnum     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alnumDash = alnum + "_-"
	lowerHex  = "0123456789abcdef"
	base32    = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	b64       = alnum + "+/"
	digits    = "0123456789"
)

type gen struct{ r *rand.Rand }

func newGen(t testing.TB) *gen {
	seed := uint64(time.Now().UnixNano())
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("canary seed %d", seed)
		}
	})
	return &gen{rand.New(rand.NewPCG(seed, 0x6361))}
}

// s returns n random characters of alpha, with at least one of alpha's digits when it has any
// (several rules gate on one).
func (g *gen) s(alpha string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[g.r.IntN(len(alpha))]
	}
	var ds []byte
	for i := 0; i < len(alpha); i++ {
		if '0' <= alpha[i] && alpha[i] <= '9' {
			ds = append(ds, alpha[i])
		}
	}
	if len(ds) > 0 && n > 2 {
		b[g.r.IntN(n)] = ds[g.r.IntN(len(ds))]
	}
	return string(b)
}

// generic returns n random alphanumerics that hold none of generic-api-key's stopwords (gitleaks
// allowlists those, about 1 in 50 random 24-character values).
func (g *gen) generic(n int) string {
	for {
		if v := g.s(alnum, n); !mask.GenericStopword(v) {
			return v
		}
	}
}

func (g *gen) uuid() string {
	h := g.s(lowerHex, 32)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
}

// canary is one corpus entry: Text is a decoded string value as canon hands it to String.
type canary struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Secrets []string `json:"secrets,omitempty"` // must not appear in the masked output
	Keep    []string `json:"keep,omitempty"`    // must appear in the masked output
	Rule    string   `json:"rule,omitempty"`    // a placeholder of this rule must appear
	Exact   string   `json:"exact,omitempty"`   // the exact placeholder that must appear
}

// exactSet is the registered exact values of the corpus.
type exactSet struct {
	cfg     mask.Config
	byValue map[string]string // value -> expected placeholder
}

func (e *exactSet) addExact(v string) {
	fp := fingerprint.ExactFP(v)
	e.cfg.ExactFPs = append(e.cfg.ExactFPs, fp)
	e.byValue[v] = "[masked:exact:" + hex.EncodeToString(fp[:6]) + "]"
}

func (e *exactSet) addCaller(key string) {
	fp, err := hex.DecodeString(fingerprint.CallerScope(key))
	if err != nil || len(fp) != 32 {
		panic("bad CallerScope")
	}
	e.cfg.CallerFPs = append(e.cfg.CallerFPs, [32]byte(fp))
	e.byValue[key] = "[masked:exact:" + hex.EncodeToString(fp[:6]) + "]"
}

func pem(g *gen, kind string) (string, []string) {
	var body []string
	for i := 0; i < 4; i++ {
		body = append(body, g.s(b64, 64))
	}
	return "-----BEGIN " + kind + " PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n-----END " + kind + " PRIVATE KEY-----", body
}

// corpus builds P1-P26, E1-E5 and N1-N6 with fresh random values.
func corpus(t testing.TB) ([]canary, *exactSet) {
	g := newGen(t)
	ex := &exactSet{byValue: map[string]string{}, cfg: mask.Config{ExactRev: 7}}
	var c []canary
	add := func(id, rule, prefix, secret, suffix string) {
		c = append(c, canary{ID: id, Rule: rule, Text: prefix + secret + suffix, Secrets: []string{secret}, Keep: nonEmpty(prefix, suffix)})
	}

	// P1-P22: one per explicit pattern (P22 has three forms).
	add("P1", "ca-anthropic", "here: ", "sk-ant-api03-"+g.s(alnumDash, 93)+"AA", " end")
	add("P2", "ca-anthropic", "oauth ", "sk-ant-oat01-"+g.s(alnumDash, 80), "\n")
	add("P3", "ca-sk", "openrouter ", "sk-or-v1-"+g.s(lowerHex, 64), " ok")
	add("P4", "ca-sk", "project ", "sk-proj-"+g.s(alnumDash, 100), ".")
	add("P5a", "ca-stripe", "stripe ", "sk_live_"+g.s(alnum, 24), " ok")
	add("P5b", "ca-stripe", "stripe ", "rk_live_"+g.s(alnum, 24), " ok")
	add("P6", "ca-xai", "xai ", "xai-"+g.s(alnum, 80), " ok")
	add("P7", "ca-google-api-key", "gemini ", "AIza"+g.s(alnumDash, 35), " ok")
	add("P8a", "aws-access-token", "id ", "AKIA"+g.s(base32, 16), " ok")
	add("P8b", "ca-aws-secret", "aws_secret_access_key=", g.s(b64, 40), "\nnext")
	for i, p := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		add([]string{"P9", "P10", "P11", "P12", "P13"}[i], "ca-github", "gh ", p+g.s(alnum, 36), " ok")
	}
	add("P14", "ca-github", "fine-grained ", "github_pat_"+g.s(alnum, 22)+"_"+g.s(alnum, 59), " ok")
	add("P15", "ca-gitlab", "gitlab ", "glpat-"+g.s(alnumDash, 20), " ok")
	add("P16", "ca-hf", "hf ", "hf_"+g.s(alnum, 34), " ok")
	add("P17", "ca-npm", "npm ", "npm_"+g.s(alnum, 36), " ok")
	add("P18a", "", "slack bot ", "xoxb-"+g.s(digits, 12)+"-"+g.s(digits, 13)+"-"+g.s(alnum, 24), " ok")
	add("P18b", "", "slack user ", "xoxp-"+g.s(digits, 12)+"-"+g.s(digits, 13)+"-"+g.s(digits, 13)+"-"+g.s(lowerHex, 32), " ok")
	add("P18c", "", "slack app ", "xapp-1-"+g.s(alnum, 11)+"-"+g.s(digits, 13)+"-"+g.s(lowerHex, 64), " ok")
	for i, kind := range []string{"RSA", "EC", "OPENSSH"} {
		block, body := pem(g, kind)
		c = append(c, canary{ID: "P19" + string(rune('a'+i)), Rule: "private-key", Text: "key file:\n" + block + "\nafter", Secrets: body, Keep: []string{"key file:", "after"}})
	}
	add("P20", "", "jwt ", "eyJ"+g.s(alnumDash, 30)+".eyJ"+g.s(alnumDash, 60)+"."+g.s(alnumDash, 43), " ok")
	add("P21a", "ca-google-oauth", "google ", "ya29."+g.s(alnumDash, 60), " ok")
	add("P21b", "ca-google-oauth", "refresh ", "1//0"+g.s(alnumDash, 40), " ok")
	add("P22a", "ca-url-cred", "GET https://api.example.invalid/v1/models?key=", g.s(alnum, 32), "&alt=json")
	add("P22b", "ca-url-userinfo", "dsn postgres://appuser:", g.s(alnum, 24), "@db.example.invalid:5432/app")
	add("P22c", "generic-api-key", `password = "`, g.generic(24), `"`)

	// P23: a decoded text/plain document (canon masks it as one string).
	tok := "ghp_" + g.s(alnum, 36)
	c = append(c, canary{ID: "P23", Rule: "ca-github", Text: "# deploy notes\nset the token\nGITHUB_TOKEN=" + tok + "\nend of document\n", Secrets: []string{tok}, Keep: []string{"# deploy notes", "GITHUB_TOKEN=", "end of document"}})
	// P24: a nested tool_use.input string.
	bearer := g.s(alnum, 40)
	c = append(c, canary{ID: "P24", Text: `curl -sS -H "Authorization: Bearer ` + bearer + `" https://api.example.invalid/v1`, Secrets: []string{bearer}, Keep: []string{"curl -sS -H", "https://api.example.invalid/v1"}})
	// P25: a secret written with JSON escapes; canon hands String the decoded value.
	oat := "sk-ant-oat01-" + g.s(alnum, 60)
	var decoded string
	escaped := `"key ` + strings.ReplaceAll(oat, "-", `-`) + `"`
	if err := json.Unmarshal([]byte(escaped), &decoded); err != nil {
		t.Fatal(err)
	}
	c = append(c, canary{ID: "P25", Rule: "ca-anthropic", Text: decoded, Secrets: []string{oat}, Keep: []string{"key "}})
	// P26: saved tool output, workflow script, Codex session line.
	k1, k2 := "sk-proj-"+g.s(alnumDash, 60), "xai-"+g.s(alnum, 60)
	c = append(c, canary{ID: "P26a", Text: "$ env | sort\nHOME=/home/user\nOPENAI_API_KEY=" + k1 + "\nXAI_API_KEY=" + k2 + "\n$ ", Secrets: []string{k1, k2}, Keep: []string{"HOME=/home/user", "OPENAI_API_KEY=", "XAI_API_KEY="}})
	k3 := "ghs_" + g.s(alnum, 36)
	c = append(c, canary{ID: "P26b", Rule: "ca-github", Text: "steps:\n  - run: |\n      export GH_TOKEN=" + k3 + "\n      gh api /repos/example/example\n", Secrets: []string{k3}, Keep: []string{"steps:", "export GH_TOKEN=", "gh api /repos/example/example"}})
	k4 := "hf_" + g.s(alnum, 34)
	var line struct {
		Payload struct {
			Output string `json:"output"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(`{"type":"response_item","payload":{"type":"function_call_output","output":"HF_TOKEN=`+k4+`\nexit 0\n"}}`), &line); err != nil {
		t.Fatal(err)
	}
	c = append(c, canary{ID: "P26c", Rule: "ca-hf", Text: line.Payload.Output, Secrets: []string{k4}, Keep: []string{"HF_TOKEN=", "exit 0"}})

	// E1: archive tokens in content (ca-archive-token: cha_src_ and cha_enr_).
	add("E1a", "ca-archive-token", "token ", "cha_src_"+g.s(alnum, 43), " ok")
	add("E1b", "ca-archive-token", "code ", "cha_enr_"+g.s(alnum, 32), " ok")
	// E2: a Coolify-style 64-character password registered by fingerprint.
	pw := g.s(alnum, 64)
	ex.addExact(pw)
	c = append(c, canary{ID: "E2", Text: "the coolify value " + pw + " was rotated", Secrets: []string{pw}, Exact: ex.byValue[pw], Keep: []string{"the coolify value ", " was rotated"}})
	// E3: a per-machine CPA key through the CallerScope set, plus an sk-cpa- key that ca-sk masks.
	cpa := g.s(alnum, 40)
	ex.addCaller(cpa)
	c = append(c, canary{ID: "E3a", Text: "client key " + cpa + " in use", Secrets: []string{cpa}, Exact: ex.byValue[cpa], Keep: []string{"client key ", " in use"}})
	// sk-cpa- keys also match ca-sk; exact wins on an overlap, so the placeholder
	// is the exact one.
	skcpa := "sk-cpa-" + g.s(alnum, 32)
	ex.addCaller(skcpa)
	c = append(c, canary{ID: "E3b", Text: "client key " + skcpa + " in use", Secrets: []string{skcpa}, Exact: ex.byValue[skcpa], Keep: []string{"client key ", " in use"}})
	// E4: the HMAC key value.
	hk := g.s(lowerHex, 64)
	ex.addExact(hk)
	c = append(c, canary{ID: "E4", Text: "hmac " + hk + " loaded", Secrets: []string{hk}, Exact: ex.byValue[hk], Keep: []string{"hmac ", " loaded"}})
	// E5: registered values inside NAME=, --key=, URL and path runs; name, host and path kept.
	for i, reg := range []func(string){ex.addExact, ex.addCaller} {
		v := g.s(alnum, 32)
		reg(v)
		sfx := []string{"x", "c"}[i]
		ph := ex.byValue[v]
		c = append(c,
			canary{ID: "E5" + sfx + "-name", Text: "OPS_VALUE=" + v, Secrets: []string{v}, Exact: ph, Keep: []string{"OPS_VALUE="}},
			canary{ID: "E5" + sfx + "-flag", Text: "restic --key=" + v + " backup", Secrets: []string{v}, Keep: []string{"restic --key=", " backup"}},
			canary{ID: "E5" + sfx + "-url", Text: "POST https://h/" + v, Secrets: []string{v}, Exact: ph, Keep: []string{"POST https://h/"}},
			canary{ID: "E5" + sfx + "-path", Text: "/path/" + v + "/x", Secrets: []string{v}, Exact: ph, Keep: []string{"/path/", "/x"}},
		)
	}

	// N1-N6: negative controls, unchanged.
	neg := func(id, text string) { c = append(c, canary{ID: id, Text: text, Keep: []string{text}}) }
	neg("N1", "ordinary body marker cha-marker-"+g.s(lowerHex, 8)+" stays visible")
	neg("N2", g.s(lowerHex, 64))
	neg("N2b", "sha256 "+g.s(lowerHex, 64))
	neg("N3", g.uuid())
	neg("N3b", "request "+g.uuid()+" done")
	neg("N4", "max_tokens: 4096")
	neg("N4b", `{"max_tokens": 4096, "stream": true}`)
	neg("N5a", "$API_KEY")
	neg("N5b", "process.env.OPENAI_API_KEY")
	neg("N5c", "export ANTHROPIC_API_KEY=$API_KEY")
	neg("N6a", "[masked:ca-github:3f9a1c2b7e4d]")
	neg("N6b", "token: [masked:ca-github:3f9a1c2b7e4d] and [masked:exact:0123456789ab]")
	return c, ex
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func TestCanaries(t *testing.T) {
	cs, ex := corpus(t)
	m, err := mask.New(ex.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.ExactRev() != 7 {
		t.Errorf("ExactRev = %d", m.ExactRev())
	}
	for _, c := range cs {
		out, hits := m.String(c.Text)
		for _, s := range c.Secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s: secret survives masking (hits %v)", c.ID, hits)
			}
		}
		for _, k := range c.Keep {
			if !strings.Contains(out, k) {
				t.Errorf("%s: kept text %q missing", c.ID, k)
			}
		}
		if c.Rule != "" && (!strings.Contains(out, "[masked:"+c.Rule+"]") && !strings.Contains(out, "[masked:"+c.Rule+":")) {
			t.Errorf("%s: no %s placeholder (hits %v)", c.ID, c.Rule, hits)
		}
		if c.Exact != "" && !strings.Contains(out, c.Exact) {
			t.Errorf("%s: exact placeholder missing (hits %v)", c.ID, hits)
		}
		if len(c.Secrets) > 0 && len(hits) == 0 {
			t.Errorf("%s: no hits", c.ID)
		}
		if len(c.Secrets) == 0 && (out != c.Text || hits != nil) {
			t.Errorf("%s: negative control changed (hits %v)", c.ID, hits)
		}
		// Detect reports exactly what String replaced, with spans only.
		f := m.Detect(c.Text)
		var n uint32
		for _, h := range hits {
			n += h
		}
		if uint32(len(f)) != n {
			t.Errorf("%s: Detect found %d, String replaced %d", c.ID, len(f), n)
		}
		for _, x := range f {
			if x.Span <= 0 || hits[x.RuleID] == 0 {
				t.Errorf("%s: bad finding %v", c.ID, x)
			}
		}
		// The server's re-check of masked content finds nothing; masking is idempotent.
		if f := m.Detect(out); len(f) != 0 {
			t.Errorf("%s: Detect(masked) = %v", c.ID, f)
		}
		if again, h := m.String(out); again != out || h != nil {
			t.Errorf("%s: Mask(Mask(x)) != Mask(x)", c.ID)
		}
	}
}

// TestCanaryCorpusFile writes the generated corpus to testdata/canaries/corpus.jsonl (ignored by git,
// regenerated on every run) so other packages' canary tests can read the same shapes.
func TestCanaryCorpusFile(t *testing.T) {
	cs, _ := corpus(t)
	dir := filepath.Join("testdata", "canaries")
	var b strings.Builder
	for _, c := range cs {
		line, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Logf("corpus not written: %v", err)
	}
}

func TestTagsAreDeterministicAcrossRules(t *testing.T) {
	m, err := mask.New(mask.Config{})
	if err != nil {
		t.Fatal(err)
	}
	g := newGen(t)
	tok := "ghp_" + g.s(alnum, 36)
	a, _ := m.String("first " + tok)
	b, _ := m.String("export GITHUB_TOKEN=" + tok + "\n")
	tagA := a[strings.Index(a, "[masked:"):]
	tagB := b[strings.Index(b, "[masked:"):]
	if !strings.HasPrefix(tagA, "[masked:ca-github:") || tagA[len(tagA)-14:] != tagB[len(tagB)-15:len(tagB)-1] {
		t.Errorf("same secret, different tags: %q %q", tagA, tagB)
	}
	// Untagged rules never carry a tag.
	out, _ := m.String(`password = "` + g.generic(24) + `"`)
	if !strings.Contains(out, "[masked:generic-api-key]") {
		t.Errorf("generic-api-key placeholder must be untagged: %q", strings.TrimPrefix(out, `password = "`))
	}
}

// TestExactSubRuns pins the order of exact-value sub-candidates and their 20 to 512 byte bounds.
func TestExactSubRuns(t *testing.T) {
	g := newGen(t)
	v := g.s(alnum, 24)
	ex := &exactSet{byValue: map[string]string{}}
	ex.addExact(v)
	m, err := mask.New(ex.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ph := ex.byValue[v]
	for _, tc := range []struct{ in, want string }{
		{v, ph},                                 // whole run
		{"A=" + v, "A=" + ph},                   // suffix after the first separator
		{"a/b/c/" + v, "a/b/c/" + ph},           // suffix after the third
		{"a/b/c/d/" + v, "a/b/c/d/" + ph},       // suffix after the fourth
		{v + "/tail", ph + "/tail"},             // first segment
		{"x/" + v + "/y/z", "x/" + ph + "/y/z"}, // middle segment
		{"a/b/c/d/e/" + v, "a/b/c/d/e/" + ph},   // after the last separator (here the fifth)
		{"NAME: " + v, "NAME: " + ph},           // ':' and space split runs
		{"Bearer " + v, "Bearer " + ph},         // whitespace splits runs
		{v + "x", v + "x"},                      // a longer run is a different value
		{strings.Repeat("a", 500) + "/" + v, strings.Repeat("a", 500) + "/" + ph}, // runs over 512 bytes are tested too
		{v + ".", ph + "."},                               // end of a sentence
		{v + "...", ph + "..."},                           // trailing dots
		{"+" + v, "+" + ph},                               // diff line
		{"-" + v + ".", "-" + ph + "."},                   // both trimmed
		{"https://h/" + v + ".", "https://h/" + ph + "."}, // last segment, trailing trimmed
		{"/home/u/.config/archive/token/" + v, "/home/u/.config/archive/token/" + ph}, // deep path
		{"https://discord.com/api/webhooks/123456789/" + v, "https://discord.com/api/webhooks/123456789/" + ph},
		{strings.Repeat("a", 520) + "=" + v, strings.Repeat("a", 520) + "=" + ph},
	} {
		want := tc.want
		if want == "" {
			want = tc.in
		}
		if got, _ := m.String(tc.in); got != want {
			t.Errorf("String(%d-byte case) mismatch", len(tc.in))
		}
	}
	// Values under 20 bytes are never candidates, even if registered.
	short := g.s(alnum, 19)
	ex2 := &exactSet{byValue: map[string]string{}}
	ex2.addExact(short)
	m2, _ := mask.New(ex2.cfg)
	if got, _ := m2.String("x " + short + " y"); got != "x "+short+" y" {
		t.Error("a 19-byte value was masked")
	}
}

// TestExactFormulasMatchFingerprint: a value registered through pkg/fingerprint's ExactFP and
// CallerScope is found, and the placeholder names the first 12 hex of that fingerprint.
func TestExactFormulasMatchFingerprint(t *testing.T) {
	g := newGen(t)
	a, b := g.s(alnum, 30), g.s(alnum, 30)
	ex := &exactSet{byValue: map[string]string{}}
	ex.addExact(a)
	ex.addCaller(b)
	m, _ := mask.New(ex.cfg)
	fa := fingerprint.ExactFP(a)
	out, hits := m.String(a + " " + b)
	want := "[masked:exact:" + hex.EncodeToString(fa[:6]) + "] [masked:exact:" + fingerprint.CallerScope(b)[:12] + "]"
	if out != want || hits["exact"] != 2 {
		t.Errorf("exact placeholders: got %q hits %v", out, hits)
	}
}

func TestGetAndVersion(t *testing.T) {
	if mask.Version != "mask/v1" {
		t.Fatal(mask.Version)
	}
	f, ok := mask.Get("mask/v1")
	if !ok || f == nil {
		t.Fatal("Get(mask/v1) missing")
	}
	if m, err := f(mask.Config{ExactRev: 3}); err != nil || m.ExactRev() != 3 {
		t.Fatalf("New via registry: %v", err)
	}
	if f, ok := mask.Get("mask/v2"); ok || f != nil {
		t.Error("Get(mask/v2) must be absent")
	}
	if s := mask.RulesetSHA256(); len(s) != 64 || strings.Trim(s, lowerHex) != "" {
		t.Errorf("RulesetSHA256 = %q", s)
	}
}

func TestConcurrentUse(t *testing.T) {
	cs, ex := corpus(t)
	m, err := mask.New(ex.cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, len(cs))
	for i, c := range cs {
		want[i], _ = m.String(c.Text)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, c := range cs {
				if got, _ := m.String(c.Text); got != want[i] {
					t.Errorf("%s: concurrent result differs", c.ID)
				}
				m.Detect(c.Text)
			}
		}()
	}
	wg.Wait()
}
