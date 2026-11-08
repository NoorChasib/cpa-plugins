package importer

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestExactFPPiped: piped stdin works and stdout is only the 64-hex fingerprint.
func TestExactFPPiped(t *testing.T) {
	x := newH(t)
	for _, in := range []string{"s3cr3t-value-for-test\n", "s3cr3t-value-for-test\r\n", "s3cr3t-value-for-test"} {
		x.out.Reset()
		x.errb.Reset()
		x.env.Stdin = strings.NewReader(in)
		if rc := Main([]string{"exact-fp"}, x.env); rc != 0 {
			t.Fatalf("rc %d: %s", rc, x.errb.String())
		}
		fp := fingerprint.ExactFP("s3cr3t-value-for-test")
		if x.out.String() != hex.EncodeToString(fp[:])+"\n" {
			t.Fatalf("stdout %q", x.out.String())
		}
		if x.errb.String() != "" {
			t.Fatalf("stderr %q on piped stdin", x.errb.String())
		}
	}
	x.env.Stdin = strings.NewReader("")
	if rc := Main([]string{"exact-fp"}, x.env); rc == 0 {
		t.Error("empty stdin accepted")
	}
}

// TestExactFPRefusesUnmatchable: a value exact-value masking can never find (under 20 or over 512 bytes,
// or with a character outside the run alphabet) prints no fingerprint, and stderr names only the reason.
func TestExactFPRefusesUnmatchable(t *testing.T) {
	x := newH(t)
	for in, reason := range map[string]string{
		"short-pass-19-bytes":                    "shorter than 20 bytes",
		strings.Repeat("a", 513):                 "longer than 512 bytes",
		"correct horse battery staple value":     "outside [A-Za-z0-9_-+/=.~]",
		"Pa$$w0rd!with-symbols-long-enough-here": "outside [A-Za-z0-9_-+/=.~]",
		"user:password-long-enough-for-the-test": "outside [A-Za-z0-9_-+/=.~]",
	} {
		x.out.Reset()
		x.errb.Reset()
		x.env.Stdin = strings.NewReader(in + "\n")
		if rc := Main([]string{"exact-fp"}, x.env); rc != 1 {
			t.Errorf("rc %d for a value with reason %q", rc, reason)
		}
		if x.out.String() != "" {
			t.Errorf("stdout %q for an unmatchable value", x.out.String())
		}
		if !strings.Contains(x.errb.String(), reason) || strings.Contains(x.errb.String(), in) {
			t.Errorf("stderr %q, want the reason %q and never the value", x.errb.String(), reason)
		}
	}
	// The run alphabet itself, at both length bounds, is accepted.
	for _, in := range []string{strings.Repeat("Az09_-+/=.~", 2)[:20], strings.Repeat("x", 512)} {
		x.out.Reset()
		x.env.Stdin = strings.NewReader(in)
		if rc := Main([]string{"exact-fp"}, x.env); rc != 0 || len(strings.TrimSpace(x.out.String())) != 64 {
			t.Errorf("rc %d stdout %q for a matchable value", rc, x.out.String())
		}
	}
}

// TestMaskReviewRefuses: mask-review runs only with a terminal on stdout and CHAT_ARCHIVE_MASK_REVIEW_OPERATOR=1.
func TestMaskReviewRefuses(t *testing.T) {
	x := newH(t)
	x.writeCorpus()
	for _, env := range []string{"", "1"} {
		x.out.Reset()
		x.errb.Reset()
		x.env.Getenv = func(k string) string {
			if k == "CHAT_ARCHIVE_MASK_REVIEW_OPERATOR" {
				return env
			}
			return ""
		}
		if rc := Main([]string{"mask-review", "--rule", "generic-api-key", "--sample", "5"}, x.env); rc == 0 {
			t.Fatalf("mask-review ran with env %q and no terminal", env)
		}
		if x.out.String() != "" || !strings.Contains(x.errb.String(), "the operator's own terminal") {
			t.Fatalf("stdout %q stderr %q", x.out.String(), x.errb.String())
		}
	}
	// A terminal on stdout without the flag also refuses (a real tty when the test has one).
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		defer tty.Close()
		x.env.Stdout = tty
		x.env.Getenv = func(string) string { return "" }
		if rc := Main([]string{"mask-review", "--rule", "generic-api-key", "--sample", "5"}, x.env); rc == 0 {
			t.Fatal("mask-review ran without CHAT_ARCHIVE_MASK_REVIEW_OPERATOR")
		}
	}
}

func TestReviewSpansAlign(t *testing.T) {
	orig := "a token = abcdefghij1234567890 here and api_key=zyxwvut9876543210abc end"
	out := "a token = [masked:generic-api-key] here and api_key=[masked:generic-api-key] end"
	sp := reviewSpans(orig, out, "generic-api-key")
	if len(sp) != 2 || !strings.Contains(sp[0], `"abcdefghij1234567890"`) || !strings.Contains(sp[1], `"zyxwvut9876543210abc"`) {
		t.Fatalf("spans %q", sp)
	}
}

func TestInstallTimer(t *testing.T) {
	x := newH(t)
	x.env.Executable = "/opt/bin/chat-archive-import"
	x.env.GOOS = "linux"
	if rc := Main([]string{"install-timer"}, x.env); rc != 0 {
		t.Fatalf("rc %d: %s", rc, x.errb.String())
	}
	svc, _ := os.ReadFile(filepath.Join(x.home, ".config", "systemd", "user", "chat-archive-import.service"))
	tmr, _ := os.ReadFile(filepath.Join(x.home, ".config", "systemd", "user", "chat-archive-import.timer"))
	if !strings.Contains(string(svc), "ExecStart=/opt/bin/chat-archive-import run") || !strings.Contains(string(svc), "Nice=10") ||
		!strings.Contains(string(svc), "MemoryHigh=1536M") || !strings.Contains(string(svc), "MemoryMax=2G") ||
		!strings.Contains(string(svc), "Environment=GOMEMLIMIT=1GiB") || !strings.Contains(string(tmr), "OnUnitActiveSec=5min") {
		t.Fatalf("systemd units:\n%s\n%s", svc, tmr)
	}
	x.env.GOOS = "darwin"
	if rc := Main([]string{"install-timer"}, x.env); rc != 0 {
		t.Fatalf("rc %d: %s", rc, x.errb.String())
	}
	pl, _ := os.ReadFile(filepath.Join(x.home, "Library", "LaunchAgents", "com.chat-archive.import.plist"))
	if !strings.Contains(string(pl), "<integer>300</integer>") || !strings.Contains(string(pl), "<string>run</string>") || !strings.Contains(string(pl), "LowPriorityIO") {
		t.Fatalf("plist:\n%s", pl)
	}
}

// TestStatusAndDeadListCountsOnly: status and dead list print counts, ids, rule ids, shapes and times,
// never content or real file names.
func TestStatusAndDeadListCountsOnly(t *testing.T) {
	x := newH(t)
	x.enroll()
	x.writeCorpus()
	path := x.mainFile(gProject, gSession)
	x.srv.RejectKey(keyOfLine(t, x, path, 1), wire.ReasonMaskViolation)
	x.mustRun()
	x.out.Reset()
	if rc := Main([]string{"status"}, x.env); rc != 0 {
		t.Fatal(x.errb.String())
	}
	status := x.out.String()
	for _, want := range []string{"last run:", "cursors:", "dead letters (dead): mask_violation=1", "faults: none"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	x.out.Reset()
	if rc := Main([]string{"dead", "list"}, x.env); rc != 0 {
		t.Fatal(x.errb.String())
	}
	dl := x.out.String()
	if !strings.Contains(dl, "reason=mask_violation") || !strings.Contains(dl, "claude-projects/<project>/<session>.jsonl") {
		t.Errorf("dead list:\n%s", dl)
	}
	for _, out := range []string{status, dl} {
		if strings.Contains(out, gProject) || strings.Contains(out, "please") {
			t.Errorf("content or a real path in output:\n%s", out)
		}
	}
}

// TestMaskDryRunSynthetic: mask-dry-run prints rule counts, timings and the gate on synthetic history,
// and nothing of the content.
func TestMaskDryRunSynthetic(t *testing.T) {
	x := newH(t)
	x.writeCorpus()
	secret := "ghp_" + strings.Repeat("a1B2c3D4e5", 4)[:36]
	x.writeRaw(filepath.Join(x.projects(), gProject, gSession, "tool-results", "s.txt"), []byte("token "+secret+"\n"))
	if rc := Main([]string{"mask-dry-run", "--synth-mb", "1", "--sample-bytes", "100000000"}, x.env); rc != 0 {
		t.Fatalf("rc %d: %s", rc, x.errb.String())
	}
	out := x.out.String()
	t.Log(out)
	for _, want := range []string{"mask-dry-run:", "matches per rule", "ca-github", "MB/s per rule over", "cold 8.5 MB class", "cold 50 MB class", "largest inputs", "gate (class inputs):"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) || strings.Contains(out, gProject) || regexp.MustCompile(`please|subagent task`).MatchString(out) {
		t.Errorf("content in output:\n%s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	x := newH(t)
	for _, args := range [][]string{{}, {"nope"}, {"enroll"}, {"run", "--max-bytes", "-1"}, {"requeue"}, {"dead"}, {"mask-review", "--rule", "x"}} {
		if rc := Main(args, x.env); rc != 2 {
			t.Errorf("%v: rc %d, want 2", args, rc)
		}
	}
	if rc := Main([]string{"run"}, x.env); rc != 1 || !strings.Contains(x.errb.String(), "not enrolled") {
		t.Errorf("run before enroll: rc %d %q", rc, x.errb.String())
	}
}

func TestEnrollKeepsMachineID(t *testing.T) {
	x := newH(t)
	x.enroll()
	p := pathsFor(x.home)
	if fi, err := os.Stat(p.TokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(p.ConfigDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(p.StateDB); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v %v", fi.Mode(), err)
	}
	m1 := x.meta()
	tok, _ := os.ReadFile(p.TokenFile)
	if strings.Contains(x.out.String(), strings.TrimSpace(string(tok))) {
		t.Fatal("enroll printed the token")
	}
	x.enroll() // re-enroll: same machine (pinned by the test), new source, new outbox
	m2 := x.meta()
	if m1.OutboxID == m2.OutboxID || m2.NextSeq != 1 {
		t.Fatalf("re-enroll kept outbox %s", m2.OutboxID)
	}
	cfg, _ := loadConfig(p)
	if cfg.MachineID != testMachineID {
		t.Fatalf("machine id %s", cfg.MachineID)
	}
}

func TestRegisterCaller(t *testing.T) {
	x := newH(t)
	x.enroll()
	kf := filepath.Join(x.home, "key")
	os.WriteFile(kf, []byte("sk-cpa-synthetic-test-key\n"), 0o600)
	if rc := Main([]string{"register-caller", "--key-file", kf, "--name", "laptop"}, x.env); rc != 0 {
		t.Fatalf("rc %d: %s", rc, x.errb.String())
	}
	fp := fingerprint.CallerScope("sk-cpa-synthetic-test-key")
	if got := x.srv.Callers()[fp]; got.Machine != "laptop" {
		t.Fatalf("callers %+v", x.srv.Callers())
	}
	if strings.Contains(x.out.String(), "sk-cpa-synthetic") {
		t.Fatal("register-caller printed the key")
	}
}

func TestGateInputsPreferReal(t *testing.T) {
	syn := &dryInput{synth: "x", size: 50_000_000}
	real := &dryInput{size: 52_000_000}
	if _, l := gateInputs([]*dryInput{syn, real}); l != real {
		t.Fatal("the synthetic input beat a real 50 MB-class input")
	}
	if _, l := gateInputs([]*dryInput{syn}); l != syn {
		t.Fatal("no stand-in without a real input")
	}
}
