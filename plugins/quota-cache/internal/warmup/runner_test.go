package warmup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cliFixture(t *testing.T, email string, tail string) (Options, Identity, string) {
	t.Helper()
	root := t.TempDir()
	options := Defaults(filepath.Join(root, "snapshot.json"))
	options.ProfileRoot = filepath.Join(root, "profiles")
	options.CLIPath = filepath.Join(root, "fake-claude")
	options, err := options.Validate()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(options.ProfileRoot, accountA)
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ -n "$ANTHROPIC_API_KEY$ANTHROPIC_BASE_URL$CLAUDE_CODE_OAUTH_TOKEN$CLAUDE_CODE_USE_BEDROCK" ]; then exit 19; fi
case " $* " in
  *" auth status "*) printf '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max","email":"` + email + `","configDirectory":"%s"}' "$CLAUDE_CONFIG_DIR" ;;
  *) printf '%s\n' "$@" > "$CLAUDE_CONFIG_DIR/arguments"
     ` + tail + `
     ;;
esac
`
	if err := os.WriteFile(options.CLIPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return options, Identity{AuthIndex: accountA, Email: "one@example.com"}, profile
}

func TestCLIUsesTheSelectedProfileModelAndNoInheritedAPIKey(t *testing.T) {
	o, identity, profile := cliFixture(t, "one@example.com", `printf '{"type":"result","subtype":"success","is_error":false}'`)
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK"} {
		t.Setenv(key, "must-not-reach-runner")
	}
	o.Model, o.Effort = "claude-future-model", "medium"
	if err := (CLI{}).Warm(context.Background(), o, identity); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(profile, "arguments"))
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, required := range []string{"--safe-mode\n", "--setting-sources\n\n", "--model\nclaude-future-model\n", "--effort\nmedium\n", "--tools\n\n", "--no-session-persistence\n", "--max-turns\n1\n", "--strict-mcp-config\n"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing runner control %q", required)
		}
	}
	if strings.Contains(args, "--bare") || strings.Contains(args, "--fallback-model") {
		t.Fatal("runner bypasses subscription auth or adds model fallback")
	}
}

func TestCLIRefusesAProfileForAnotherAccountBeforeInference(t *testing.T) {
	o, identity, profile := cliFixture(t, "other@example.com", "exit 0")
	if err := (CLI{}).Warm(context.Background(), o, identity); err == nil || err.Error() != "profile_identity_mismatch" {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, "arguments")); !os.IsNotExist(err) {
		t.Fatal("inference ran against the wrong profile")
	}
}

func TestCLITimeoutAndProviderOutputAreNotReturned(t *testing.T) {
	o, identity, _ := cliFixture(t, "one@example.com", "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := (CLI{}).Warm(ctx, o, identity); err == nil || err.Error() != "outcome_unknown" {
		t.Fatalf("error = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("runner did not kill the process group")
	}
	o, identity, _ = cliFixture(t, "one@example.com", "printf 'sensitive-provider-body'; printf 'sensitive-token' >&2; exit 17")
	if err := (CLI{}).Warm(context.Background(), o, identity); err == nil || err.Error() != "request_failed" {
		t.Fatalf("error = %v", err)
	}
}
