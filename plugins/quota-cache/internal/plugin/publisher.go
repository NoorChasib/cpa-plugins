package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/credits"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// The publisher's clock. It looks at its inputs every publishEvery, and while
// the cache runs it writes the credit state at least every heartbeatEvery, so
// a reader can tell a running publisher from one that stopped: Quota Glance
// takes a snapshot written more than client.CreditPublishLagLimit after the
// state's computed_at as a file no publisher maintains. A failure to publish
// is logged at most every publishWarnEvery.
const (
	publishEvery     = 10 * time.Second
	heartbeatEvery   = 60 * time.Second
	publishWarnEvery = 10 * time.Minute
)

// How hard one publish tries. publishIfDue computes only when an input
// changed, the last state reached its valid_until, or the heartbeat is due;
// publishForced always computes; publishFinal also always writes. Every mode
// writes when the state changed, beyond computed_at and the countdowns.
type publishMode int

const (
	publishIfDue publishMode = iota
	publishForced
	publishFinal
)

// publisher computes the credit state beside one snapshot and writes it to
// client.CreditsPath, for as long as the cache that holds the snapshot's
// writer lock runs: every file it writes is written under that lock, because
// it is stopped before the cache closes.
//
// Locks: publishMu is held across one compute and its write. It is taken
// after the plugin's mu and linkMu when those are held, never before them, and
// the settings and organizations stores' leaf locks are taken only inside
// prepare, which runs before publishMu is taken, and inside the store's own
// atomic readers, which take none.
type publisher struct {
	// path is the snapshot's, and version quota-cache's, published.
	path, version string
	store         *settings.Store
	// items is the item list to compute from and where it came from.
	items func() ([]creditItem, client.CreditOrganizations)
	// prepare runs at the start of every tick, before publishMu is taken:
	// the settings and organizations files' syncs and the move from the
	// configuration. Nil in a test that needs none.
	prepare func(now time.Time)
	log     func(level, message string, fields map[string]any)
	// watched are the files whose change makes a tick compute: the snapshot,
	// the meter, the settings file, its import record, the organizations file
	// and Quota Glance's settings.json candidates.
	watched []string
	// limit is the largest encoding written, client.MaxCreditsBytes; write
	// commits a file. Tests lower the one and replace the other.
	limit int
	write func(path string, raw []byte) error

	publishMu sync.Mutex
	stopped   bool
	// seen are the watched files' stamps at the last compute, in watched
	// order, followed by the import record's source when it is not one of
	// them.
	seen []fileStamp
	// snapshotAt is the written_at of the last snapshot that could be read;
	// haveSnapshot is false until there has been one.
	snapshotAt   time.Time
	haveSnapshot bool
	// written is the comparable encoding of the last state written, and
	// writtenAt when it was written.
	written   []byte
	writtenAt time.Time

	warnMu   sync.Mutex
	warnedAt time.Time

	// lastState is the last state computed, for the status and save routes,
	// and lastMeter the meter file read for it, for a new reading's baseline.
	// Either is nil until the first compute.
	lastState atomic.Pointer[client.CreditState]
	lastMeter atomic.Pointer[client.APIMeter]

	looping bool
	done    chan struct{}
	exited  chan struct{}
}

func newPublisher(snapshotPath, version string, store *settings.Store, items func() ([]creditItem, client.CreditOrganizations), log func(string, string, map[string]any)) *publisher {
	watched := []string{snapshotPath, client.MeterPath(snapshotPath), settings.FilePath(snapshotPath), settings.RecordPath(snapshotPath), client.OrganizationsPath(snapshotPath)}
	for _, candidate := range client.LegacySettingsCandidates(snapshotPath) {
		if !slices.Contains(watched, candidate) {
			watched = append(watched, candidate)
		}
	}
	return &publisher{
		path: snapshotPath, version: version, store: store, items: items, log: log, watched: watched,
		limit: client.MaxCreditsBytes, write: writeCreditsFile,
		done: make(chan struct{}), exited: make(chan struct{}),
	}
}

// start begins the ticks. It is called once, by the configure that opened the
// cache.
func (p *publisher) start() {
	p.looping = true
	go p.run()
}

func (p *publisher) run() {
	defer close(p.exited)
	ticker := time.NewTicker(publishEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			now := time.Now().UTC()
			p.guarded(now, func() { p.tick(now) })
		}
	}
}

// tick is one look at the inputs: prepare, then a publish if one is due.
func (p *publisher) tick(now time.Time) {
	if p.prepare != nil {
		p.prepare(now)
	}
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	if !p.stopped {
		p.publishLocked(now, publishIfDue)
	}
}

// PublishNow computes the state now and writes it if it changed, before it
// returns. A save calls it before answering, so the page's next read sees the
// save, and a configure calls it because the items may have changed. After
// Stop it does nothing.
func (p *publisher) PublishNow() {
	now := time.Now().UTC()
	p.guarded(now, func() {
		p.publishMu.Lock()
		defer p.publishMu.Unlock()
		if !p.stopped {
			p.publishLocked(now, publishForced)
		}
	})
}

// Stop ends the ticks, then computes and writes the state one last time. The
// meter has stopped by then and its file says when, so the last file carries
// api_credits.meter.stoppedAtEpoch: a reader marks the figures as missing
// spend five minutes after the stop, exactly as it would reading the meter
// itself. Later calls do nothing.
func (p *publisher) Stop() {
	if p.looping {
		p.looping = false
		close(p.done)
		<-p.exited
	}
	now := time.Now().UTC()
	p.guarded(now, func() {
		p.publishMu.Lock()
		defer p.publishMu.Unlock()
		if !p.stopped {
			p.stopped = true
			p.publishLocked(now, publishFinal)
		}
	})
}

// LastState is the state last computed, nil before the first.
func (p *publisher) LastState() *client.CreditState { return p.lastState.Load() }

// LastMeter is the meter file the last compute read, nil when there was none
// or it could not be read. A new Console reading takes its baseline from it,
// so the spend since the reading is computed from the same file.
func (p *publisher) LastMeter() *client.APIMeter { return p.lastMeter.Load() }

// guarded runs f, the publish at now, and turns a panic into a warning: in a
// native plugin a panic that escapes aborts CPA, and the next tick may well
// publish. The panic value is never logged; it can hold a path or a value.
func (p *publisher) guarded(now time.Time, f func()) {
	defer func() {
		if recover() != nil {
			p.warn("panic", now)
		}
	}()
	f()
}

// warn logs that the publish at now could not write the state, with why:
// never a value, an amount, a path or a name. It does so at most once every
// publishWarnEvery by the publishes' own clock, and again at once if that
// clock was set back past the last warning, so a clock set back never
// silences it.
func (p *publisher) warn(reason string, now time.Time) {
	p.warnMu.Lock()
	due := p.warnedAt.IsZero() || now.Sub(p.warnedAt) >= publishWarnEvery || now.Before(p.warnedAt)
	if due {
		p.warnedAt = now
	}
	p.warnMu.Unlock()
	if due && p.log != nil {
		p.log("warn", "quota-cache could not publish its credit state", map[string]any{"reason": reason})
	}
}

// publishLocked is one publish in mode. The caller holds publishMu.
func (p *publisher) publishLocked(now time.Time, mode publishMode) {
	now = now.UTC().Truncate(time.Second)
	stamps := p.stamps()
	last := p.lastState.Load()
	heartbeat := p.writtenAt.IsZero() || now.Sub(p.writtenAt) >= heartbeatEvery
	expired := last != nil && last.ValidUntil != nil && !now.Before(*last.ValidUntil)
	if mode == publishIfDue && !heartbeat && !expired && slices.EqualFunc(stamps, p.seen, fileStamp.same) {
		return
	}
	state, ok := p.compute(now)
	if !ok {
		return
	}
	p.seen = stamps
	p.lastState.Store(state)
	comparable, err := json.Marshal(comparableOf(state))
	if err != nil {
		p.warn("encoding", now)
		return
	}
	if mode != publishFinal && !heartbeat && !expired && bytes.Equal(comparable, p.written) {
		return
	}
	raw, err := json.Marshal(state)
	switch {
	case err != nil:
		p.warn("encoding", now)
		return
	case len(raw) > p.limit:
		// The file is left as it was: readers age it out, and Quota Glance
		// takes it as abandoned once the snapshot moves on without it.
		p.warn("too_large", now)
		return
	}
	if err := p.write(client.CreditsPath(p.path), raw); err != nil {
		p.warn("unwritable", now)
		return
	}
	p.written, p.writtenAt = comparable, now
}

// compute works out the state at now from the files as they are. False when
// no snapshot has ever been read, so there is nothing to publish beside yet.
func (p *publisher) compute(now time.Time) (*client.CreditState, bool) {
	// Only written_at is read, so one that cannot be read now, mid-write
	// elsewhere or after a fault, stands in with the last one that could.
	if snapshot, err := client.Load(p.path); err == nil {
		p.snapshotAt, p.haveSnapshot = snapshot.WrittenAt, true
	} else if !p.haveSnapshot {
		return nil, false
	}
	meter, err := client.LoadMeter(client.MeterPath(p.path))
	meterError := ""
	switch {
	case errors.Is(err, client.ErrMeterMissing):
		meter, meterError = nil, client.CreditMeterErrorMissing
	case err != nil:
		meter, meterError = nil, client.CreditMeterErrorUnreadable
	}
	p.lastMeter.Store(meter)
	values, status := p.store.Snapshot()
	items, organizations := p.items()
	in := credits.Input{
		Now: now, Snapshot: client.Snapshot{WrittenAt: p.snapshotAt}, Meter: meter, MeterError: meterError,
		Settings: values, SettingsState: status.State, Import: status.Import, Organizations: organizations, Version: p.version,
	}
	for _, item := range items {
		credit := item.credit
		in.Items = append(in.Items, credits.Item{ID: item.id, Credit: &credit})
	}
	return credits.Compute(in), true
}

// stamps are the watched files' stamps now, and the import record's source
// last when it is not watched: a record left by another layout can name a
// file that is not a candidate here.
func (p *publisher) stamps() []fileStamp {
	stamps := make([]fileStamp, 0, len(p.watched)+1)
	for _, path := range p.watched {
		stamps = append(stamps, stampOf(path))
	}
	if imp := p.store.Status().Import; imp != nil && imp.SourcePath != nil && !slices.Contains(p.watched, *imp.SourcePath) {
		stamps = append(stamps, stampOf(*imp.SourcePath))
	}
	return stamps
}

// comparableOf is what decides whether a state is worth writing: the state
// with computed_at zeroed and every countdown cleared, since those move with
// the clock alone and every reader recomputes them (client.CreditsAt). It
// shares nothing that it changes with state.
func comparableOf(state *client.CreditState) *client.CreditState {
	out := *state
	out.ComputedAt = time.Time{}
	if d := state.APICredits; d != nil {
		copied := *d
		copied.Accounts = slices.Clone(d.Accounts)
		for i := range copied.Accounts {
			copied.Accounts[i].RenewsInSeconds = nil
		}
		if r := d.Pool.NextRefill; r != nil {
			refill := *r
			refill.RefillInSeconds = 0
			copied.Pool.NextRefill = &refill
		}
		copied.Pool.FullInSeconds = nil
		out.APICredits = &copied
	}
	return &out
}

// fileStamp is what a tick compares to tell whether a file changed: whether
// it is there, and its identity, size, modification time and mode.
type fileStamp struct {
	// exists is false only when Lstat says there is no file.
	exists bool
	// info is Lstat's answer, nil when it failed.
	info fs.FileInfo
}

func stampOf(path string) fileStamp {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileStamp{}
	}
	if err != nil {
		return fileStamp{exists: true}
	}
	return fileStamp{exists: true, info: info}
}

// same reports whether a and b describe the same file, unchanged.
func (a fileStamp) same(b fileStamp) bool {
	if a.exists != b.exists || (a.info == nil) != (b.info == nil) {
		return false
	}
	if a.info == nil {
		return true
	}
	return os.SameFile(a.info, b.info) && a.info.Size() == b.info.Size() &&
		a.info.ModTime().Equal(b.info.ModTime()) && a.info.Mode() == b.info.Mode()
}

// writeCreditsFile writes raw over path the way the snapshot and the meter are
// written: a temporary file named ".<name>-*" beside it at 0600, written,
// synced, renamed over it, and the directory synced. A reader never sees half
// a file.
func writeCreditsFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0o600); err == nil {
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// The rename is visible from here on; a directory that cannot be synced
	// is no reason to report the write lost.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
