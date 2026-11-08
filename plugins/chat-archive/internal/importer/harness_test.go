package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestMain doubles as the importer binary for subprocess tests: with CA_IMPORT_HELPER=1 it runs Main
// with the arguments after "--" and exits.
func TestMain(m *testing.M) {
	if os.Getenv("CA_IMPORT_HELPER") == "1" {
		args := os.Args
		for i, a := range args {
			if a == "--" {
				args = args[i+1:]
				break
			}
		}
		e := DefaultEnv()
		e.Home = os.Getenv("CA_IMPORT_HOME")
		os.Exit(Main(args, e))
	}
	os.Exit(m.Run())
}

const testMachineID = "00000000-0000-4000-8000-0000000000aa"

// h is one test home with a fake archive.
type h struct {
	t        *testing.T
	home     string
	srv      *clienttest.Server
	out      *syncBuf
	errb     *syncBuf
	env      *Env
	sourceID string
	g        *synth.Gen
	now      time.Time
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func newH(t *testing.T) *h {
	t.Helper()
	x := &h{t: t, home: t.TempDir(), srv: clienttest.NewServer(clienttest.Options{}), out: &syncBuf{}, errb: &syncBuf{}, g: synth.New(1),
		now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(x.srv.Close)
	x.env = &Env{
		Home: x.home, Stdin: strings.NewReader(""), Stdout: x.out, Stderr: x.errb,
		Now:   func() time.Time { return x.now },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return x
}

// enroll enrolls the importer against the fake archive and pins the machine id for goldens.
func (x *h) enroll() {
	x.t.Helper()
	code := x.srv.NewEnrollCode()
	if rc := Main([]string{"enroll", "--archive-url", x.srv.URL, "--code", code, "--label", "test"}, x.env); rc != 0 {
		x.t.Fatalf("enroll rc=%d: %s", rc, x.errb.String())
	}
	p := pathsFor(x.home)
	cfg, err := loadConfig(p)
	if err != nil {
		x.t.Fatal(err)
	}
	cfg.MachineID = testMachineID
	if err := saveConfig(p, cfg); err != nil {
		x.t.Fatal(err)
	}
	x.sourceID = cfg.SourceID
}

func (x *h) run(args ...string) int {
	x.t.Helper()
	return Main(append([]string{"run"}, args...), x.env)
}

func (x *h) mustRun(args ...string) {
	x.t.Helper()
	if rc := x.run(args...); rc != 0 {
		x.t.Fatalf("run rc=%d\nstdout: %s\nstderr: %s", rc, x.out.String(), x.errb.String())
	}
}

func (x *h) projects() string { return filepath.Join(x.home, ".claude", "projects") }
func (x *h) codex() string    { return filepath.Join(x.home, ".codex", "sessions") }
func (x *h) skills() string   { return filepath.Join(x.home, ".claude", "skills") }

// write writes lines to a file and pins its mtime, so locators and created_at are deterministic.
func (x *h) write(path string, lines ...string) {
	x.t.Helper()
	if err := synth.Write(path, lines...); err != nil {
		x.t.Fatal(err)
	}
	x.pin(path)
}

func (x *h) writeRaw(path string, data []byte) {
	x.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		x.t.Fatal(err)
	}
	x.pin(path)
}

var pinTime = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func (x *h) pin(path string) {
	if err := os.Chtimes(path, pinTime, pinTime); err != nil {
		x.t.Fatal(err)
	}
}

// envelopes returns what the fake archive received from this source.
func (x *h) envelopes() []clienttest.Received { return x.srv.Envelopes(x.sourceID) }

func (x *h) state() *State {
	x.t.Helper()
	st, err := openState(pathsFor(x.home).StateDB)
	if err != nil {
		x.t.Fatal(err)
	}
	x.t.Cleanup(func() { st.Close() })
	return st
}

func (x *h) meta() Meta {
	x.t.Helper()
	st := x.state()
	m, err := st.meta()
	if err != nil {
		x.t.Fatal(err)
	}
	return m
}

func decodeRecord(t *testing.T, e wire.Envelope) wire.ImportRecord {
	t.Helper()
	var r wire.ImportRecord
	if err := json.Unmarshal(e.Body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func statuses(rs []clienttest.Received) map[wire.Status]int {
	out := map[wire.Status]int{}
	for _, r := range rs {
		out[r.Status]++
	}
	return out
}

// mainFile returns the path of a main session file.
func (x *h) mainFile(project, session string) string {
	return filepath.Join(x.projects(), project, session+".jsonl")
}

// testBuilder is a builder with the real masker and default limits, for computing keys in tests.
func testBuilder(t *testing.T) *builder {
	t.Helper()
	m, err := mask.New(mask.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &builder{ctx: context.Background(), m: m, memo: newMemo(), limits: wire.DefaultLimits, stats: newRunStats()}
}
