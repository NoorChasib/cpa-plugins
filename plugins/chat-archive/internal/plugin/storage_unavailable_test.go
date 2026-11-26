package plugin_test

// dropped.storage_unavailable (plugin 0.1.6): while capture is configured on but no runtime accepts
// hooks, each hook call a running runtime would have admitted is counted, in hook events, on every
// path: a missing secret, no working directory, a failed outbox open (a data-dir that is a file,
// another holder of the outbox lock), a reconfigure's gap with the replacement's open stalled, and
// a reconfigure from capture off whose first open stalls. Capture off and an unconfigured plugin
// count nothing. Every value is synthetic.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/diskstall"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin/plugintest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// uaScript offers a non-stream exchange with its usage record (5 hook events), a header-only
// stream (header-init and completion: 2) and a stream of a header-init, 6 chunks and a completion
// (8 in full capture; 2 in metadata capture, which takes no chunks).
func uaScript(p *plugin.Plugin, prefix string) {
	meta := plugintest.Meta("/v1/messages")
	ns := prefix + "-ns"
	p.Hook(protocol.MethodRequestInterceptBefore, plugintest.BeforeAuth(ns, "claude", plugintest.ClaudeBody("synthetic"), nil, meta))
	p.Hook(protocol.MethodRequestInterceptAfter, plugintest.AfterAuth(ns))
	p.Hook(protocol.MethodResponseInterceptAfter, plugintest.Response(ns, "claude", claudeReply("synthetic"), meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete(ns, "succeeded", meta))
	p.Hook(protocol.MethodUsageHandle, plugintest.Usage("u-"+ns, "sk-cpa-synthetic", "synthetic-source", ""))
	hdr := prefix + "-hdr"
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(hdr, "claude", -1, nil, meta))
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete(hdr, "failed", meta))
	st := prefix + "-st"
	p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(st, "claude", -1, nil, meta))
	for i := 0; i < 6; i++ {
		p.Hook(protocol.MethodResponseInterceptStreamChunk, plugintest.Chunk(st, "claude", i, claudeDelta(fmt.Sprint("part ", i)), meta))
	}
	p.Hook(protocol.MethodRequestComplete, plugintest.Complete(st, "succeeded", meta))
}

// uaEvents is what uaScript counts in each capture mode.
var uaEvents = map[string]uint64{"full": 15, "metadata": 9, "off": 0}

func uaYAML(dataDir, capture, tokenEnv string) string {
	y := fmt.Sprintf("enabled: true\npriority: 100\narchive-url: http://127.0.0.1:9\nsource-token-env: %s\nhmac-key-env: CA_TEST_HMAC_KEY\ncapture: %s\n", tokenEnv, capture)
	if dataDir != "" {
		y += "data-dir: " + dataDir + "\n"
	}
	return y
}

func uaPlugin(t *testing.T, yaml string, prep func(*plugin.Plugin)) *plugin.Plugin {
	t.Helper()
	os.Setenv("CA_TEST_SOURCE_TOKEN", "synthetic-source-token")
	os.Setenv("CA_TEST_HMAC_KEY", plugintest.HMACKey)
	os.Unsetenv("CA_TEST_MISSING_TOKEN")
	p := plugin.New(nil)
	if prep != nil {
		prep(p)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, plugintest.Lifecycle(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(p.Shutdown)
	return p
}

func uaCount(p *plugin.Plugin) uint64 { return p.Engine().Stats.Dropped()["storage_unavailable"] }

// TestStorageUnavailableCounted covers the paths where no runtime starts.
func TestStorageUnavailableCounted(t *testing.T) {
	type tc struct {
		name, capture, state, reason string
		yaml                         func(t *testing.T) string
		prep                         func(*plugin.Plugin)
	}
	data := func(t *testing.T) string { return filepath.Join(t.TempDir(), "data") }
	cases := []tc{
		{"missing_secret", "full", plugin.StateConfigError, "secret_unavailable", func(t *testing.T) string { return uaYAML(data(t), "full", "CA_TEST_MISSING_TOKEN") }, nil},
		{"missing_secret_metadata", "metadata", plugin.StateConfigError, "secret_unavailable", func(t *testing.T) string { return uaYAML(data(t), "metadata", "CA_TEST_MISSING_TOKEN") }, nil},
		{"no_working_directory", "full", plugin.StateUnavailable, "working_directory_unavailable", func(t *testing.T) string { return uaYAML("", "full", "CA_TEST_SOURCE_TOKEN") },
			func(p *plugin.Plugin) { plugin.SetDefaultDataDir(p, "") }},
		{"data_dir_is_a_file", "full", plugin.StateUnavailable, "outbox_unavailable", func(t *testing.T) string {
			f := filepath.Join(t.TempDir(), "file")
			os.WriteFile(f, []byte("synthetic"), 0o600)
			return uaYAML(f, "full", "CA_TEST_SOURCE_TOKEN")
		}, nil},
		{"outbox_locked", "metadata", plugin.StateUnavailable, "outbox_unavailable", func(t *testing.T) string {
			dir := data(t)
			ob, err := outbox.Open(outbox.Options{Dir: dir, CapBytes: 1 << 40, Retention: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ob.Close() })
			return uaYAML(dir, "metadata", "CA_TEST_SOURCE_TOKEN")
		}, nil},
		{"capture_off", "off", plugin.StateUnconfigured, "capture_off", func(t *testing.T) string { return uaYAML(data(t), "off", "CA_TEST_SOURCE_TOKEN") }, nil},
		{"unconfigured", "off", plugin.StateUnconfigured, "archive_url_missing", func(t *testing.T) string { return "enabled: true\npriority: 100\n" }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := uaPlugin(t, c.yaml(t), c.prep)
			if st, why := p.State(); st != c.state || why != c.reason {
				t.Fatalf("state %s/%s, want %s/%s (as 0.1.5)", st, why, c.state, c.reason)
			}
			uaScript(p, c.name)
			if got, want := uaCount(p), uaEvents[c.capture]; got != want {
				t.Fatalf("storage_unavailable %d, want %d hook events", got, want)
			}
			if d := p.Engine().Stats.Dropped(); d["stopped"] != 0 {
				t.Fatalf("dropped %v", d)
			}
		})
	}
}

// TestStorageUnavailableDuringStalledStart: hooks that arrive while a start's outbox open is
// stalled are counted, on a reconfigure of a running plugin (the gap between the old runtime's stop
// and the replacement's start) and on a reconfigure from capture off (the first open). Once the
// runtime runs, the same hooks are captured and nothing more is counted.
func TestStorageUnavailableDuringStalledStart(t *testing.T) {
	for _, c := range []struct{ name, from, to string }{
		{"on_to_on", "full", "full"}, {"off_to_full", "off", "full"}, {"off_to_metadata", "off", "metadata"},
	} {
		t.Run(c.name, func(t *testing.T) {
			setStopTimeout(t, 2*time.Second)
			if err := diskstall.Install(); err != nil {
				t.Fatal(err)
			}
			h := plugintest.New(t, nil, "capture: "+c.from+"\n")
			p := h.Plugin
			if c.from != "off" {
				plugintest.WaitFor(t, "the runtime", func() bool { ob, _, _ := p.Runtime(); return ob != nil })
			}
			steps := armSyncs(t, filepath.Join(h.Dir, "data"), 12)
			done := make(chan struct{})
			yaml := strings.Replace(h.YAML, "capture: "+c.from+"\n", "capture: "+c.to+"\nworkers: 1\n", 1)
			go func() {
				defer close(done)
				_, _ = p.Handle(protocol.MethodPluginReconfigure, plugintest.Lifecycle(yaml))
			}()
			steps.step("outbox.OpenContext", "(*Plugin).configure")
			before := uaCount(p)
			uaScript(p, "gap-"+c.name)
			got := uaCount(p) - before
			steps.release()
			<-done
			if want := uaEvents[c.to]; got != want {
				t.Fatalf("storage_unavailable %d during the stalled open, want %d hook events", got, want)
			}
			if st, _ := p.State(); st != plugin.StateRunning {
				t.Fatalf("state %s after the open returned", st)
			}
			after := uaCount(p)
			uaScript(p, "run-"+c.name)
			if uaCount(p) != after {
				t.Fatalf("storage_unavailable counted while running: %d then %d", after, uaCount(p))
			}
		})
	}
}
