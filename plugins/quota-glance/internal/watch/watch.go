// Package watch turns quota-cache's snapshot writes into reload events.
//
// There is no event bus between CPA plugins and no host callback that reports
// a cache refresh, so the file itself is the signal.
package watch

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// DefaultDebounce absorbs the several events one atomic rename can produce.
	DefaultDebounce = 200 * time.Millisecond
	// DefaultBackstop bounds how long a missed event can go unnoticed.
	// fsnotify does not deliver reliably on every filesystem, and a dashboard
	// that silently stops updating is worse than one that polls occasionally.
	DefaultBackstop = time.Minute
)

type Options struct {
	Path     string
	Debounce time.Duration
	Backstop time.Duration
	// OnChange runs off the watch goroutine's event loop, one call per
	// debounced change. It must not block for long.
	OnChange func()
	// Logf reports watcher trouble. It never receives the token or any file
	// contents.
	Logf func(message string)
}

type Watcher struct {
	opts     Options
	fs       *fsnotify.Watcher
	dir      string
	stop     chan struct{}
	done     chan struct{}
	begin    chan struct{}
	beginOne sync.Once
	once     sync.Once
	stateMu  sync.Mutex
	state    State
}

// State is what .../health reports about the watcher.
type State struct {
	Watching  bool      `json:"watching"`
	Directory string    `json:"directory"`
	LastEvent time.Time `json:"last_event"`
	LastError string    `json:"last_error,omitempty"`
	Reloads   uint64    `json:"reloads"`
	Backstops uint64    `json:"backstops"`
}

// Start begins watching the directory containing Path.
//
// The directory, not the file: quota-cache commits by writing a temporary file
// and renaming it over the target, which replaces the inode. A watch registered
// on the file path follows the old inode and goes deaf after the first refresh,
// which looks exactly like a cache that stopped updating.
func Start(opts Options) (*Watcher, error) {
	if opts.Path == "" || opts.OnChange == nil {
		return nil, errors.New("watch requires a path and a change handler")
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.Backstop <= 0 {
		opts.Backstop = DefaultBackstop
	}
	if opts.Logf == nil {
		opts.Logf = func(string) {}
	}
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, errors.New("cache path cannot be resolved")
	}
	opts.Path = path
	fs, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.New("filesystem watcher cannot be created")
	}
	w := &Watcher{
		opts: opts, fs: fs, dir: filepath.Dir(path),
		stop: make(chan struct{}), done: make(chan struct{}), begin: make(chan struct{}),
	}
	if err := fs.Add(w.dir); err != nil {
		// Not fatal. The backstop still polls, so the dashboard updates within
		// a minute instead of immediately.
		w.setError("snapshot directory cannot be watched; falling back to polling")
	} else {
		w.setWatching(true)
	}
	go w.run()
	return w, nil
}

// Begin releases the startup read. It is separate from Start so the caller can
// finish wiring itself up first: OnChange fires on the watcher's own goroutine
// and would otherwise race the assignment of the watcher it reports on.
// Calling it more than once is harmless; never calling it means no events.
func (w *Watcher) Begin() {
	w.beginOne.Do(func() { close(w.begin) })
}

func (w *Watcher) Close() {
	// Unblock a run loop still waiting to begin, so Close never hangs on a
	// watcher whose owner failed partway through wiring.
	w.Begin()
	w.once.Do(func() {
		close(w.stop)
		<-w.done
		_ = w.fs.Close()
	})
}

func (w *Watcher) State() State {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	state := w.state
	state.Directory = w.dir
	return state
}

func (w *Watcher) setError(message string) {
	w.stateMu.Lock()
	w.state.LastError, w.state.Watching = message, false
	w.stateMu.Unlock()
	w.opts.Logf(message)
}

func (w *Watcher) setWatching(watching bool) {
	w.stateMu.Lock()
	w.state.Watching = watching
	if watching {
		w.state.LastError = ""
	}
	w.stateMu.Unlock()
}

// fingerprint identifies a file version without reading it. Size and modtime
// are enough to notice a rename that fsnotify did not report.
type fingerprint struct {
	size    int64
	modTime time.Time
	present bool
}

func fingerprintOf(path string) fingerprint {
	info, err := os.Stat(path)
	if err != nil {
		return fingerprint{}
	}
	return fingerprint{size: info.Size(), modTime: info.ModTime(), present: true}
}

func (w *Watcher) run() {
	defer close(w.done)

	select {
	case <-w.begin:
	case <-w.stop:
		return
	}
	// Read once before any event so a restart serves data immediately rather
	// than waiting for the next poll to change something.
	seen := fingerprintOf(w.opts.Path)
	w.fire(false)

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	backstop := time.NewTicker(w.opts.Backstop)
	defer backstop.Stop()
	defer debounce.Stop()

	for {
		select {
		case <-w.stop:
			return

		case event, ok := <-w.fs.Events:
			if !ok {
				return
			}
			// A rename surfaces as Create on the destination, and can arrive
			// alongside Chmod and Write. Anything touching our file or its
			// directory collapses into one debounced reload.
			if filepath.Clean(event.Name) != w.opts.Path {
				continue
			}
			w.stateMu.Lock()
			w.state.LastEvent = time.Now()
			w.stateMu.Unlock()
			if !pending {
				pending = true
				debounce.Reset(w.opts.Debounce)
			}

		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			if err != nil {
				w.setError("filesystem watcher reported an error")
			}

		case <-debounce.C:
			pending = false
			seen = fingerprintOf(w.opts.Path)
			w.fire(false)

		case <-backstop.C:
			// Re-add if the directory was replaced underneath the watch;
			// fsnotify follows the inode, not the path.
			if err := w.fs.Add(w.dir); err != nil {
				w.setError("snapshot directory cannot be watched; falling back to polling")
			} else {
				w.setWatching(true)
			}
			current := fingerprintOf(w.opts.Path)
			if current != seen {
				seen = current
				w.fire(true)
			}
		}
	}
}

func (w *Watcher) fire(viaBackstop bool) {
	w.stateMu.Lock()
	w.state.Reloads++
	if viaBackstop {
		w.state.Backstops++
	}
	w.stateMu.Unlock()
	w.opts.OnChange()
}
