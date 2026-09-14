// The tests live in package watch so they can remove the underlying watch the
// way a filesystem would, which is the failure the backstop exists to cover.
package watch

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// commit writes by atomic rename, exactly as quota-cache does.
func commit(t *testing.T, path, body string) {
	t.Helper()
	temp, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temp.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, path string, backstop time.Duration) (*Watcher, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	w, err := Start(Options{
		Path:     path,
		Debounce: 40 * time.Millisecond,
		Backstop: backstop,
		OnChange: func() { calls.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	// The owner releases the startup read once it has finished wiring itself
	// up, exactly as the runtime does.
	w.Begin()
	// Start reads once before any event, so a restart serves data immediately.
	waitFor(t, &calls, 1)
	return w, &calls
}

func waitFor(t *testing.T, calls *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("reloads = %d; want %d", calls.Load(), want)
}

// quota-cache commits by renaming a temporary file over the target, which
// replaces the inode. Watching the file path would follow the old inode and go
// deaf after the first refresh, so the watch is on the directory.
func TestAtomicRenameProducesExactlyOneDebouncedReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	commit(t, path, `{"schema":1}`)
	w, calls := start(t, path, time.Hour)

	commit(t, path, `{"schema":1,"n":1}`)
	waitFor(t, calls, 2)
	// One rename can surface as several events; they must collapse into one.
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("one rename produced %d reloads; want 1 after the startup read", got-1)
	}

	// A second rename replaces the inode again. A file watch would be deaf by
	// now; a directory watch still fires.
	commit(t, path, `{"schema":1,"n":2}`)
	waitFor(t, calls, 3)
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 3 {
		t.Fatalf("reloads = %d; want 3", got)
	}
	if state := w.State(); !state.Watching || state.Directory != dir {
		t.Fatalf("state = %+v", state)
	}
}

// Several rapid commits inside one debounce interval collapse. The dashboard
// needs the latest document, not one rebuild per write.
func TestBurstOfWritesCollapses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	commit(t, path, `{"schema":1}`)
	_, calls := start(t, path, time.Hour)

	for i := 0; i < 8; i++ {
		commit(t, path, `{"schema":1,"n":`+string(rune('0'+i))+`}`)
	}
	waitFor(t, calls, 2)
	time.Sleep(250 * time.Millisecond)
	if got := calls.Load(); got > 3 {
		t.Fatalf("8 rapid commits produced %d reloads; debounce is not collapsing them", got-1)
	}
}

// fsnotify does not deliver reliably on every filesystem. When the watch is
// removed underneath it, the plugin must still notice a change, or the
// dashboard silently freezes on data that looks current.
func TestStatBackstopFiresWhenTheWatchIsRemovedUnderneathIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	commit(t, path, `{"schema":1}`)
	w, calls := start(t, path, 150*time.Millisecond)

	// Remove the watch the way a filesystem that stops delivering would.
	if err := w.fs.Remove(dir); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()

	// Change the file without a usable event. Size differs, so the stat
	// fingerprint changes.
	commit(t, path, `{"schema":1,"changed":true,"padding":"aaaaaaaaaaaaaaaa"}`)
	waitFor(t, calls, before+1)

	state := w.State()
	if state.Backstops == 0 {
		t.Fatal("the change was not picked up by the stat backstop")
	}
	// The backstop also re-adds the watch, so a transient failure recovers.
	if !state.Watching {
		t.Fatalf("watch was not re-added: %+v", state)
	}
}

// A quiet file must not produce reloads: the backstop compares a fingerprint
// rather than firing on every tick.
func TestBackstopDoesNotFireWithoutAChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	commit(t, path, `{"schema":1}`)
	_, calls := start(t, path, 50*time.Millisecond)

	time.Sleep(400 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("reloads = %d; an unchanged file must not trigger a rebuild", got)
	}
}

// A missing snapshot is not fatal: the watcher keeps running so the dashboard
// recovers by itself when quota-cache writes again.
func TestMissingFileIsNotFatalAndRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	_, calls := start(t, path, 100*time.Millisecond)

	commit(t, path, `{"schema":1}`)
	waitFor(t, calls, 2)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, calls, 3)
}

// Close must not hang on a watcher that was never released, which is the state
// a partially-failed configure leaves behind.
func TestCloseBeforeBeginDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	commit(t, path, `{"schema":1}`)
	var calls atomic.Int64
	w, err := Start(Options{Path: path, OnChange: func() { calls.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on a watcher that was never begun")
	}
}

func TestStartRejectsIncompleteOptions(t *testing.T) {
	if _, err := Start(Options{OnChange: func() {}}); err == nil {
		t.Fatal("a watch without a path was accepted")
	}
	if _, err := Start(Options{Path: "/tmp/x"}); err == nil {
		t.Fatal("a watch without a handler was accepted")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	commit(t, path, `{"schema":1}`)
	w, _ := start(t, path, time.Hour)
	w.Close()
	w.Close()
}
