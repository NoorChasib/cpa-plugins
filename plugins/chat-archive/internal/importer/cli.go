// Package importer is chat-archive-import, the pure-Go history importer: it reads Claude Code
// transcripts, workflow files, saved tool outputs, skills and Codex rollouts, masks and splits them
// with pkg/mask and pkg/canon, and uploads import.record and import.file envelopes with pkg/client.
// Cursors, dead letters and tombstones live in a modernc.org/sqlite file; one run at a time holds an
// exclusive flock. It never prints, logs or stores content: output is counts, ids, rule ids and times.
package importer

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"
)

// Version is the importer's component version (heartbeats, enrollment, User-Agent).
var Version = "0.1.0"

// Env is the process environment of one command; tests replace its parts.
type Env struct {
	Home       string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Getenv     func(string) string
	Now        func() time.Time
	HTTPClient *http.Client                                     // nil: pkg/client's default transport
	Sleep      func(ctx context.Context, d time.Duration) error // nil: real sleep
	GOOS       string                                           // "" means runtime.GOOS
	Executable string                                           // "" means os.Executable()

	hooks *testHooks
}

// testHooks are fault-injection points for tests.
type testHooks struct {
	afterCommit func() error // called after a batch's commits, before the cursor transaction
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) getenv(k string) string {
	if e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

func (e *Env) goos() string {
	if e.GOOS != "" {
		return e.GOOS
	}
	return runtime.GOOS
}

// DefaultEnv is the real process environment.
func DefaultEnv() *Env {
	home, _ := os.UserHomeDir()
	return &Env{Home: home, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
}

const usage = `usage: chat-archive-import <command> [flags]

commands:
  enroll --archive-url U --code cha_enr_... [--label L] [--intermittent]
  register-caller --key-file F --name N
  run [--session ID] [--max-bytes N] [--dry-run]
  backfill
  status
  dead list [--reason R]
  requeue --reason R [--since T] [--remask] [--split] [--dry-run]
  exact-fp
  mask-dry-run [--root R] [--sample-bytes N] [--synth-mb N] [--seed S]
  mask-review --rule R --sample N   (operator only, in their own terminal)
  install-timer
  version
`

// Main runs one command and returns the process exit code: 0 success (and a run that found the lock
// busy), 1 failure, 2 usage error.
func Main(args []string, e *Env) int {
	if len(args) == 0 {
		fmt.Fprint(e.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	var err error
	switch cmd {
	case "enroll":
		url := fs.String("archive-url", "", "archive base URL")
		code := fs.String("code", "", "enrollment code (cha_enr_...)")
		label := fs.String("label", "", "source label (default: hostname)")
		inter := fs.Bool("intermittent", false, "a machine that sleeps (24 h dead-man threshold)")
		if fs.Parse(rest) != nil || *url == "" || *code == "" || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.enroll(*url, *code, *label, *inter)
	case "register-caller":
		keyFile := fs.String("key-file", "", "file holding the CPA client key")
		name := fs.String("name", "", "machine name")
		if fs.Parse(rest) != nil || *keyFile == "" || *name == "" || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.registerCaller(*keyFile, *name)
	case "run":
		session := fs.String("session", "", "only this Claude Code session (SessionEnd hook)")
		maxBytes := fs.String("max-bytes", strconv.Itoa(defaultMaxBytes), "new bytes to read this run (0 = no limit)")
		dry := fs.Bool("dry-run", false, "build envelopes without uploading or moving cursors")
		if fs.Parse(rest) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		n, perr := strconv.ParseInt(*maxBytes, 10, 64)
		if perr != nil || n < 0 {
			return usageErr(e, fs)
		}
		return e.runCmd(runOpts{session: *session, maxBytes: n, dryRun: *dry}, false)
	case "backfill":
		if fs.Parse(rest) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		return e.runCmd(runOpts{backfill: true}, true)
	case "status":
		if fs.Parse(rest) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.status()
	case "dead":
		if len(rest) == 0 || rest[0] != "list" {
			return usageErr(e, fs)
		}
		reason := fs.String("reason", "", "only this reason")
		if fs.Parse(rest[1:]) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.deadList(*reason)
	case "requeue":
		reason := fs.String("reason", "", "dead letters with this reason")
		since := fs.String("since", "", "only dead letters since T (RFC 3339 or a duration such as 24h)")
		remask := fs.Bool("remask", false, "rebuild with the current masker")
		split := fs.Bool("split", false, "rebuild with large frame strings as pieces, against current limits")
		dry := fs.Bool("dry-run", false, "count only")
		if fs.Parse(rest) != nil || *reason == "" || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		var t int64
		if *since != "" {
			ts, perr := parseSince(*since, e.now())
			if perr != nil {
				return usageErr(e, fs)
			}
			t = ts.Unix()
		}
		return e.requeueCmd(requeueOpts{reason: *reason, since: t, remask: *remask, split: *split, dryRun: *dry})
	case "exact-fp":
		if fs.Parse(rest) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.exactFP()
	case "mask-dry-run":
		root := fs.String("root", "", "only this root (claude-projects, codex-sessions, claude-skills) or directory")
		sample := fs.Int64("sample-bytes", 0, "stratified sample of at most N bytes (0 = everything)")
		synth := fs.Int("synth-mb", 0, "also time a synthetic input of N MB of invented text")
		seed := fs.Uint64("seed", 1, "sampling seed")
		if fs.Parse(rest) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.maskDryRun(dryRunOpts{root: *root, sampleBytes: *sample, synthMB: *synth, seed: *seed})
	case "mask-review":
		rule := fs.String("rule", "", "rule id")
		n := fs.Int("sample", 0, "number of spans")
		root := fs.String("root", "", "only this root or directory")
		if fs.Parse(rest) != nil || *rule == "" || *n <= 0 || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.maskReview(*rule, *n, *root)
	case "install-timer":
		if fs.Parse(rest) != nil || fs.NArg() > 0 {
			return usageErr(e, fs)
		}
		err = e.installTimer()
	case "version":
		fmt.Fprintln(e.Stdout, "chat-archive-import", Version)
	case "help", "-h", "--help":
		fmt.Fprint(e.Stdout, usage)
	default:
		fmt.Fprint(e.Stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(e.Stderr, "chat-archive-import %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func usageErr(e *Env, fs *flag.FlagSet) int {
	fmt.Fprint(e.Stderr, usage)
	return 2
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-d), nil
}

// runCmd is `run` and `backfill`: take the lock (run exits 0 at once when it is busy, backfill waits),
// sweep, record the run and send the heartbeat.
func (e *Env) runCmd(o runOpts, wait bool) int {
	p := pathsFor(e.Home)
	release, busy, err := acquireLock(p.LockFile, wait)
	if err != nil {
		fmt.Fprintf(e.Stderr, "chat-archive-import: lock: %v\n", err)
		return 1
	}
	if busy {
		fmt.Fprintln(e.Stdout, "another importer run holds the lock; nothing to do")
		return 0
	}
	defer release()
	r, err := e.newRunner(o)
	if err != nil {
		fmt.Fprintf(e.Stderr, "chat-archive-import: %v\n", err)
		return 1
	}
	defer r.close()
	runErr := r.run()
	r.finish(runErr)
	r.summary(e.Stdout, runErr)
	if runErr != nil {
		return 1
	}
	return 0
}
