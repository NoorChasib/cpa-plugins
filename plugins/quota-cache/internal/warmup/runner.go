package warmup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Identity struct {
	AuthIndex, Email string
	Disabled         bool
}
type Runner interface {
	Warm(context.Context, Options, Identity) error
}
type RunnerFunc func(context.Context, Options, Identity) error

func (f RunnerFunc) Warm(ctx context.Context, o Options, id Identity) error { return f(ctx, o, id) }

// CLI runs from an empty disposable directory, without user customizations,
// tools, MCP, inherited API keys or provider overrides. Authentication stays
// inside the CLI's separately signed-in profile. Output is bounded and is
// neither logged nor saved; callers receive only static error codes.
type CLI struct{}

type limitedOutput struct{ bytes.Buffer }

func (w *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := (64 << 10) - w.Len(); left > 0 {
		_, _ = w.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}

func runnerEnv(profile string) []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "PATH" || key == "HOME" || key == "USER" || key == "LOGNAME" || key == "TMPDIR" || key == "LANG" || strings.HasPrefix(key, "LC_") || key == "SSL_CERT_FILE" || key == "SSL_CERT_DIR" || key == "NODE_EXTRA_CA_CERTS" || key == "HTTPS_PROXY" || key == "HTTP_PROXY" || key == "NO_PROXY" || key == "https_proxy" || key == "http_proxy" || key == "no_proxy" {
			env = append(env, item)
		}
	}
	return append(env, "CLAUDE_CONFIG_DIR="+profile, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_REFUSAL_FALLBACK=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
}

func (CLI) Warm(ctx context.Context, o Options, id Identity) error {
	if !authIndex.MatchString(id.AuthIndex) || !strings.Contains(id.Email, "@") {
		return errors.New("identity_unknown")
	}
	profile := filepath.Join(o.ProfileRoot, id.AuthIndex)
	if info, err := os.Lstat(profile); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("profile_missing")
	}
	binary, err := exec.LookPath(o.CLIPath)
	if err != nil {
		return errors.New("cli_unavailable")
	}
	directory, err := os.MkdirTemp("", "quota-cache-warmup-")
	if err != nil {
		return errors.New("runner_unavailable")
	}
	defer os.RemoveAll(directory)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != syscall.ESRCH {
				return err
			}
			return os.ErrProcessDone
		}
		cmd.Dir, cmd.Env, cmd.Stdin = directory, runnerEnv(profile), nil
		cmd.WaitDelay = time.Second
		var output limitedOutput
		cmd.Stdout, cmd.Stderr = &output, io.Discard
		err := cmd.Run()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return output.Bytes(), err
	}
	status, err := run("--safe-mode", "--setting-sources", "", "auth", "status", "--json")
	var auth struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
		Email      string `json:"email"`
		Provider   string `json:"apiProvider"`
		Plan       string `json:"subscriptionType"`
		Directory  string `json:"configDirectory"`
	}
	if err != nil || json.Unmarshal(status, &auth) != nil || !auth.LoggedIn || auth.AuthMethod != "claude.ai" || auth.Provider != "firstParty" {
		return errors.New("subscription_login_required")
	}
	switch auth.Plan {
	case "pro", "max", "team", "enterprise":
	default:
		return errors.New("subscription_login_required")
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Email), strings.TrimSpace(id.Email)) || filepath.Clean(auth.Directory) != profile {
		return errors.New("profile_identity_mismatch")
	}
	answer, err := run("--safe-mode", "--setting-sources", "", "--print", "Reply only OK.", "--model", o.Model, "--effort", o.Effort, "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands", "--max-turns", "1", "--no-session-persistence", "--permission-mode", "dontAsk", "--permission-prompts", "none", "--prompt-suggestions", "false", "--output-format", "json")
	if ctx.Err() != nil {
		return errors.New("outcome_unknown")
	}
	var result struct {
		IsError bool   `json:"is_error"`
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err != nil || json.Unmarshal(answer, &result) != nil || result.IsError || result.Type != "result" || result.Subtype != "success" {
		return errors.New("request_failed")
	}
	return nil
}
