package mask_test

// Canaries for escapes and percent-encoding around secrets, secrets longer than a scan window, the gitleaks global allowlist
// precedence fix, exact-value contexts and overlaps, back-to-back tokens, placeholder lookalikes,
// linear time on long lines, and StringContext. Every value is synthetic and random; failures name
// the case, never the value.

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
)

func newMasker(t testing.TB, cfg mask.Config) *mask.Masker {
	t.Helper()
	m, err := mask.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// checkMasked fails when any of secrets survives String, Detect of the output is not clean or the
// output is not a fixed point.
func checkMasked(t *testing.T, m *mask.Masker, id, text string, secrets ...string) string {
	t.Helper()
	out, hits := m.String(text)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%s: secret survives masking (hits %v)", id, hits)
		}
	}
	if f := m.Detect(text); len(f) == 0 {
		t.Errorf("%s: Detect found nothing", id)
	}
	if f := m.Detect(out); len(f) != 0 {
		t.Errorf("%s: Detect(masked) = %v", id, f)
	}
	if again, _ := m.String(out); again != out {
		t.Errorf("%s: Mask(Mask(x)) != Mask(x)", id)
	}
	return out
}

// TestEscapedContexts: a JSON escape or %XX right before a token (no \b), "\/" inside a secret, and
// "&" in place of "&".
func TestEscapedContexts(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	tokens := map[string]string{
		"anthropic": "sk-ant-api03-" + g.s(alnumDash, 93) + "AA",
		"proj":      "sk-proj-" + g.s(alnumDash, 60),
		"openrtr":   "sk-or-v1-" + g.s(lowerHex, 64),
		"google":    "AIza" + g.s(alnumDash, 35),
		"xai":       "xai-" + g.s(alnum, 60),
		"hf":        "hf_" + g.s(alnum, 34),
		"npm":       "npm_" + g.s(alnum, 36),
		"stripe":    "sk_live_" + g.s(alnum, 24),
		"archive":   "cha_src_" + g.s(alnum, 43),
		"github":    "ghp_" + g.s(alnum, 36),
	}
	for _, pre := range []string{`\n`, `\t`, `\r`, `%20`, `key%3D`, `%0A`, ` `, `\"`} {
		for name, tok := range tokens {
			checkMasked(t, m, "prefix "+pre+" "+name, "x"+pre+tok+" y", tok)
			// A JSON-in-string tool output (Codex function_call_output style).
			checkMasked(t, m, "json-in-string "+pre+" "+name, `{"output":"run\n`+tok+`\n","metadata":{"exit_code":0}}`, tok)
		}
	}

	// "\/" inside a base64 secret: the decoded secret and the escaped form are both gone.
	secret := g.s(alnum, 12) + "/" + g.s(alnum, 13) + "/" + g.s(alnum, 13)
	escaped := strings.ReplaceAll(secret, "/", `\/`)
	out := checkMasked(t, m, "aws escaped slash", `{"aws_secret_access_key":"`+escaped+`","region":"x"}`, escaped, secret[:12], secret[len(secret)-13:])
	if !strings.Contains(out, "[masked:ca-aws-secret:") {
		t.Errorf("aws escaped slash: no ca-aws-secret placeholder")
	}

	// & in place of & before URL credential parameters.
	for _, c := range []struct{ name, param, val string }{
		{"access_token", "access_token", g.s(alnum, 32)},
		{"amz signature", "X-Amz-Signature", g.s(lowerHex, 64)},
		{"sig", "sig", g.s(alnum, 20)},
	} {
		checkMasked(t, m, "u0026 "+c.name, `https://h.example.invalid/p?page=2&`+c.param+"="+c.val+`&y=1`, c.val)
	}

	// A text attachment holding a JSON document is a decoded string with escapes inside.
	tok := tokens["anthropic"]
	checkMasked(t, m, "json document", `{"config":{"env":"ANTHROPIC_API_KEY=`+tok+`\nOTHER=1"}}`, tok)
}

// TestEscapedExactValues: registered values after an escape or a %XX triplet.
func TestEscapedExactValues(t *testing.T) {
	g := newGen(t)
	v := g.s(alnum, 40)
	ex := &exactSet{byValue: map[string]string{}}
	ex.addExact(v)
	m := newMasker(t, ex.cfg)
	for _, pre := range []string{`\n`, `\t`, `%2F`, `%3D`, `%20`, `name%3D`, `\/`} {
		out := checkMasked(t, m, "exact after "+pre, "line"+pre+v+`\nnext`, v)
		if !strings.Contains(out, ex.byValue[v]) {
			t.Errorf("exact after %s: placeholder missing", pre)
		}
	}
}

// TestLongSecrets: secrets longer than a rule's scan window are masked whole.
func TestLongSecrets(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	chunks := func(s string) []string { // every 40-byte chunk must be gone
		var out []string
		for i := 0; i+40 <= len(s); i += 40 {
			out = append(out, s[i:i+40])
		}
		return append(out, s[len(s)-40:])
	}
	bearer := g.s(alnum+"._-", 1600)
	checkMasked(t, m, "long bearer", "curl -H 'Authorization: Bearer "+bearer+"' https://h.example.invalid", chunks(bearer)...)

	// A JWT whose payload has no interior "ey" keyword hit within 2 KiB.
	noEY := strings.NewReplacer("e", "f", "E", "F", "y", "z", "Y", "Z").Replace(alnum) + "_-"
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJ" + g.s(noEY, 2600) + "." + g.s(noEY, 43)
	for _, ctx := range []string{"token %s end", "id_token=%s&state=1", "Authorization: Bearer %s"} {
		checkMasked(t, m, "long jwt "+ctx, strings.Replace(ctx, "%s", jwt, 1), chunks(jwt[21:])...)
	}

	// A long URL-encoded X-Amz-Security-Token.
	var sb strings.Builder
	for sb.Len() < 3000 {
		sb.WriteString(g.s(alnum, 30))
		sb.WriteString([]string{"%2F", "%2B", "%3D"}[g.r.IntN(3)])
	}
	tokv := sb.String()
	checkMasked(t, m, "long amz token", "GET https://b.s3.example.invalid/k?X-Amz-Security-Token="+tokv+"&X-Amz-Date=20260101T000000Z", chunks(tokv)...)
}

// TestGlobalAllowlistPrecedence: secrets starting with "true", containing "false" or ending with
// "null" are masked (gitleaks' `(?i)^true|false|null$` is applied as `(?i)^(?:true|false|null)$`).
func TestGlobalAllowlistPrecedence(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	nullSecret := "Zq9" + g.generic(12) + "null"
	for mask.GenericStopword(nullSecret) {
		nullSecret = "Zq9" + g.generic(12) + "null"
	}
	for _, c := range []struct{ id, text, secret string }{
		{"userinfo true", "https://deploy:%s@db.internal:5432/x", "trueBlue" + g.s(digits, 4) + "Qx"},
		{"url password false", "https://h.example.invalid/p?password=%s", "myfalse" + g.s(alnum, 10)},
		{"generic null", `db_password = "%s"`, nullSecret},
		{"sk False", "key %s ok", "sk-proj-" + g.s(alnum, 20) + "False" + g.s(alnum, 20)},
		{"bearer true", "Authorization: Bearer %s", "true" + g.s(alnum, 30)},
	} {
		checkMasked(t, m, c.id, strings.Replace(c.text, "%s", c.secret, 1), c.secret)
	}
	// The intended entries are still allowlisted.
	for _, v := range []string{"true", "FALSE", "null"} {
		if out, _ := m.String(`password = "` + v + `"`); out != `password = "`+v+`"` {
			t.Errorf("literal %s was masked", v)
		}
	}
}

// TestExactOverlapWins: a registered value that a rule matches in part is masked whole.
func TestExactOverlapWins(t *testing.T) {
	g := newGen(t)
	v := "ghp_" + g.s(alnum, 36) + "~" + g.s(alnum, 24)
	ex := &exactSet{byValue: map[string]string{}}
	ex.addExact(v)
	m := newMasker(t, ex.cfg)
	if out, hits := m.String("value " + v + " end"); out != "value "+ex.byValue[v]+" end" || hits["exact"] != 1 {
		t.Errorf("exact overlap: got %d bytes, hits %v", len(out), hits)
	}
}

// TestBackToBackTokens: fixed-length tokens with no separator need one pass each; up to maxPasses
// they are all masked, beyond it the string becomes [masked:overflow].
func TestBackToBackTokens(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	var toks []string
	for i := 0; i < 10; i++ {
		toks = append(toks, "npm_"+g.s(alnum, 36))
	}
	checkMasked(t, m, "10 npm tokens", "tokens "+strings.Join(toks, "")+" end", toks...)

	for len(toks) < 40 {
		toks = append(toks, "npm_"+g.s(alnum, 36))
	}
	out, hits := m.String("tokens " + strings.Join(toks, "") + " end")
	if out != "[masked:overflow]" || hits["overflow"] != 1 {
		t.Errorf("40 npm tokens: want [masked:overflow], got %d bytes, hits %v", len(out), hits)
	}
	if again, h := m.String(out); again != out || h != nil {
		t.Error("overflow placeholder is not a fixed point")
	}
	if f := m.Detect("tokens " + strings.Join(toks, "") + " end"); len(f) == 0 || f[len(f)-1].RuleID != "overflow" {
		t.Errorf("Detect of 40 tokens: last finding %v", f)
	}

	// Concatenated GitHub tokens: the greedy ca-github match swallows the next token's prefix; the
	// overlapping candidate is merged, so no token body stays visible.
	var gh []string
	var bodies []string
	for i := 0; i < 12; i++ {
		b := g.s(alnum, 36)
		bodies = append(bodies, b)
		gh = append(gh, "ghp_"+b)
	}
	checkMasked(t, m, "2 github tokens", strings.Join(gh[:2], ""), bodies[:2]...)
	checkMasked(t, m, "12 github tokens", strings.Join(gh, ""), bodies...)
}

// TestPlaceholderLookalike: "[masked:<secret>]" is not a placeholder unless <secret> is a rule id.
func TestPlaceholderLookalike(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	tok := "sk-or-v1-" + g.s(lowerHex, 64)
	checkMasked(t, m, "lookalike", "[masked:"+tok+"]", tok)
	for _, p := range []string{"[masked:ca-github:3f9a1c2b7e4d]", "[masked:exact:0123456789ab]", "[masked:overflow]", "[masked:generic-api-key]"} {
		if out, h := m.String(p); out != p || h != nil {
			t.Errorf("real placeholder %s changed", p)
		}
	}
}

// TestLinearOnLongLines: masking time grows about linearly with the length of a single-line string
// full of generic-api-key candidates (the line-target allowlists used to rescan the whole line per
// candidate).
func TestLinearOnLongLines(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("timing test (meaningless under -short or -race)")
	}
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	build := func(size int) string {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(`x_key = "` + g.s(alnum, 24) + `", `)
		}
		return b.String()
	}
	best := func(s string) time.Duration {
		d := time.Duration(1 << 62)
		for i := 0; i < 2; i++ {
			start := time.Now()
			m.String(s)
			d = min(d, time.Since(start))
		}
		return d
	}
	small, large := build(64<<10), build(256<<10)
	ds, dl := best(small), best(large)
	ratio := float64(dl) / float64(ds)
	t.Logf("64 KiB %v, 256 KiB %v, ratio %.1f", ds, dl, ratio)
	if ratio > 8 { // linear is 4; the quadratic engine measured 12-16
		t.Errorf("4x the input took %.1fx the time", ratio)
	}
}

// TestStringContext: a cancelled context stops masking and returns nothing.
func TestStringContext(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	tok := "ghp_" + g.s(alnum, 36)
	out, hits, err := m.StringContext(context.Background(), "token "+tok)
	if err != nil || strings.Contains(out, tok) || hits["ca-github"] != 1 {
		t.Fatalf("StringContext: err %v hits %v", err, hits)
	}
	if want, _ := m.String("token " + tok); out != want {
		t.Error("StringContext differs from String")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, hits, err := m.StringContext(ctx, "token "+tok); !errors.Is(err, context.Canceled) || out != "" || hits != nil {
		t.Errorf("cancelled: out %d bytes, hits %v, err %v", len(out), hits, err)
	}
	// A deadline bounds one large string.
	var b strings.Builder
	for b.Len() < 4<<20 {
		b.WriteString(`api_key = "` + g.s(alnum, 24) + `"` + "\n")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = m.StringContext(ctx, b.String())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: err %v", err)
	}
	d := time.Since(start)
	t.Logf("deadline of 20ms: returned after %v", d)
	limit := 2 * time.Second
	if raceEnabled {
		limit = 10 * time.Second
	}
	if d > limit {
		t.Errorf("deadline: returned after %v", d)
	}
}

// TestWholeMatchRulesLeaveNoFragment: jwt-base64 and microsoft-teams-webhook mask their whole
// match, so no part of the encoded JWT or of the webhook URL's GUIDs and secret survives (gitleaks'
// first capture group would mask only a few characters of each).
func TestWholeMatchRulesLeaveNoFragment(t *testing.T) {
	g := newGen(t)
	m := newMasker(t, mask.Config{})
	b64url := alnum + "_-"

	// A JWT, base64-encoded once more (std alphabet, padded). Only its header is fixed.
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + g.s(b64url, 60) + "." + g.s(b64url, 43)
	enc := base64.StdEncoding.EncodeToString([]byte(jwt))
	out := checkMasked(t, m, "jwt-base64", "config value "+enc+" end", enc)
	if !regexp.MustCompile(`^config value \[masked:jwt-base64:[0-9a-f]{12}\] end$`).MatchString(out) {
		t.Errorf("jwt-base64: output is not one whole-token placeholder (%d bytes)", len(out))
	}
	for i := 0; i+8 <= len(enc); i += 8 {
		if strings.Contains(out, enc[i:i+8]) {
			t.Errorf("jwt-base64: a fragment at offset %d survives", i)
			break
		}
	}
	if f := m.Detect("config value " + enc + " end"); len(f) != 1 || f[0].RuleID != "jwt-base64" || f[0].Span != len(enc) {
		t.Errorf("jwt-base64: Detect = %v, want one finding of %d bytes", f, len(enc))
	}

	// A Teams incoming-webhook URL: two GUIDs, a 32-character secret and a third GUID.
	secret := g.s(lowerHex, 32)
	guids := []string{g.uuid(), g.uuid(), g.uuid()}
	url := "https://contoso.webhook.office.com/webhookb2/" + guids[0] + "@" + guids[1] + "/IncomingWebhook/" + secret + "/" + guids[2]
	out = checkMasked(t, m, "microsoft-teams-webhook", "post to "+url+" now", append([]string{secret}, guids...)...)
	if out != "post to [masked:microsoft-teams-webhook] now" {
		t.Errorf("microsoft-teams-webhook: output is not one whole-URL placeholder (%d bytes)", len(out))
	}
	if f := m.Detect("post to " + url + " now"); len(f) != 1 || f[0].Span != len(url) {
		t.Errorf("microsoft-teams-webhook: Detect = %v, want one finding of %d bytes", f, len(url))
	}
}
