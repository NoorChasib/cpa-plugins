package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/config"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/store"
	"github.com/NoorChasib/cpa-plugin-token-usage/internal/usage"
)

type backend interface {
	WriteBatch(context.Context, []usage.Event, time.Time) (store.BatchResult, error)
	SaveDiagnostics(context.Context, map[string]string, string, time.Time, bool) error
	ProbeWrite(context.Context, string, time.Time) error
	Maintain(context.Context, time.Time) (bool, error)
	DiskBytes() (int64, error)
	Query(context.Context, store.Filter, time.Time) (store.QueryResult, error)
	Close() error
}

const (
	faultDiskUnavailable uint32 = 1 << iota
	faultDiskBudget
	faultWrite
	faultDiagnostics
	faultMaintenance
	faultIdentity
	faultDisk = faultDiskUnavailable | faultDiskBudget
)

const (
	writeProbeInterval = 5 * time.Second
	cleanupDelay       = 10 * time.Millisecond
	cleanupTimeout     = 250 * time.Millisecond
)

var faultReasons = [...]struct {
	mask   uint32
	reason string
}{
	{faultIdentity, "event_id_exhausted"},
	{faultDiskUnavailable, "disk_unavailable"},
	{faultDiskBudget, "disk_budget"},
	{faultWrite, "write_unavailable"},
	{faultDiagnostics, "diagnostics_unavailable"},
	{faultMaintenance, "maintenance_unavailable"},
}

type counters struct{ observed, admitted, rejected, droppedQueue, droppedStorage, droppedStopped, rejectedRetention, rejectedCardinality, storageErrors, retries atomic.Uint64 }
type Collector struct {
	cfg                                      config.Config
	db                                       backend
	now                                      func() time.Time
	run                                      string
	admission                                sync.RWMutex
	stopped                                  bool
	queue                                    chan usage.Event
	done                                     chan struct{}
	stopOnce                                 sync.Once
	closeErr                                 error
	operations                               sync.RWMutex
	closed                                   bool
	counts                                   counters
	sequence                                 atomic.Uint64
	lastFailure, committed, lastPersisted    atomic.Value
	faults                                   atomic.Uint32
	cleanupPending                           atomic.Bool
	nextWriteProbe                           time.Time // Worker-owned, wall-clock recovery throttle.
	diskBytes, coverageStart, retentionFloor atomic.Int64
	unclean                                  uint64
}

func (c *counters) fields() map[string]*atomic.Uint64 {
	return map[string]*atomic.Uint64{"observed_events": &c.observed, "admitted_events": &c.admitted, "rejected_events": &c.rejected, "dropped_queue": &c.droppedQueue, "dropped_storage": &c.droppedStorage, "dropped_stopped": &c.droppedStopped, "rejected_retention": &c.rejectedRetention, "rejected_cardinality": &c.rejectedCardinality, "storage_errors": &c.storageErrors, "write_retries": &c.retries}
}
func increment(v *atomic.Uint64, n uint64) {
	for {
		old := v.Load()
		next := old + n
		if next < old {
			next = ^uint64(0)
		}
		if v.CompareAndSwap(old, next) {
			return
		}
	}
}
func (c *counters) snapshot() map[string]string {
	m := map[string]string{}
	for k, v := range c.fields() {
		m[k] = strconv.FormatUint(v.Load(), 10)
	}
	return m
}

func New(cfg config.Config, now func() time.Time) (*Collector, error) {
	if now == nil {
		now = time.Now
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("event identity unavailable")
	}
	run := hex.EncodeToString(random[:])
	db, initial, err := store.Open(cfg, now())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	initial.Unclean, err = db.StartRun(ctx, run, now())
	if err != nil {
		db.Close()
		return nil, store.ErrUnavailable
	}
	return start(cfg, db, initial, run, now), nil
}
func start(cfg config.Config, db backend, initial store.Initial, run string, now func() time.Time) *Collector {
	c := &Collector{cfg: cfg, db: db, now: now, run: run, queue: make(chan usage.Event, cfg.QueueCapacity), done: make(chan struct{}), unclean: initial.Unclean}
	for k, v := range c.counts.fields() {
		n, _ := strconv.ParseUint(initial.Diagnostics[k], 10, 64)
		v.Store(n)
	}
	c.lastFailure.Store("")
	c.committed.Store(initial.Committed)
	c.lastPersisted.Store(initial.LastPersisted)
	c.coverageStart.Store(initial.CoverageStart)
	c.retentionFloor.Store(initial.RetentionFloor)
	c.checkDisk()
	go c.work()
	return c
}
func (c *Collector) Observe(raw []byte) (usage.Event, error) {
	increment(&c.counts.observed, 1)
	c.admission.RLock()
	defer c.admission.RUnlock()
	if c.stopped {
		increment(&c.counts.droppedStopped, 1)
		return usage.Event{}, errors.New("collection stopped")
	}
	e, err := usage.Decode(raw, c.now())
	if err != nil {
		increment(&c.counts.rejected, 1)
		return e, err
	}
	floor := c.now().Add(-c.cfg.RawRetention).UnixNano()
	if retained := c.retentionFloor.Load(); retained > floor {
		floor = retained
	}
	if e.RequestedAt.UnixNano() < floor {
		increment(&c.counts.rejected, 1)
		increment(&c.counts.rejectedRetention, 1)
		return e, errors.New("event outside retention")
	}
	if c.faults.Load() != 0 {
		increment(&c.counts.droppedStorage, 1)
		return e, store.ErrUnavailable
	}
	var sequence uint64
	for {
		old := c.sequence.Load()
		if old == ^uint64(0) {
			c.updateFault(faultIdentity, faultIdentity)
			increment(&c.counts.droppedStorage, 1)
			return e, store.ErrUnavailable
		}
		sequence = old + 1
		if c.sequence.CompareAndSwap(old, sequence) {
			break
		}
	}
	e.ID = fmt.Sprintf("%s-%016x", c.run, sequence)
	select {
	case c.queue <- e:
		increment(&c.counts.admitted, 1)
		return e, nil
	default:
		increment(&c.counts.droppedQueue, 1)
		return e, errors.New("collection queue full")
	}
}
func (c *Collector) RejectOversized() {
	increment(&c.counts.observed, 1)
	c.admission.RLock()
	defer c.admission.RUnlock()
	if c.stopped {
		increment(&c.counts.droppedStopped, 1)
	} else {
		increment(&c.counts.rejected, 1)
	}
}

// Change only this operation's fault bits, preserving concurrent causes (including
// terminal identity exhaustion). A successful disk stat is not a writer probe.
func (c *Collector) updateFault(mask, value uint32) {
	for {
		old := c.faults.Load()
		if c.faults.CompareAndSwap(old, old&^mask|value) {
			break
		}
	}
	for _, f := range faultReasons {
		if value&f.mask != 0 {
			c.lastFailure.Store(f.reason)
			break
		}
	}
}
func (c *Collector) checkDisk() bool {
	size, err := c.db.DiskBytes()
	if err != nil {
		increment(&c.counts.storageErrors, 1)
		c.updateFault(faultDisk, faultDiskUnavailable)
		return false
	}
	c.diskBytes.Store(size)
	if size > c.cfg.MaxDiskBytes {
		c.updateFault(faultDisk, faultDiskBudget)
		return false
	}
	c.updateFault(faultDisk, 0)
	return true
}
func (c *Collector) retry(fn func(context.Context) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = fn(ctx)
		cancel()
		if err == nil {
			return nil
		}
		increment(&c.counts.storageErrors, 1)
		if !store.Retryable(err) || attempt == 2 {
			return err
		}
		increment(&c.counts.retries, 1)
		time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
	}
	return err
}
func (c *Collector) flush(batch []usage.Event) {
	if len(batch) == 0 {
		return
	}
	// Already-admitted events must not become repeated recovery experiments.
	if c.faults.Load()&(faultWrite|faultDisk) != 0 {
		increment(&c.counts.droppedStorage, uint64(len(batch)))
		return
	}
	var result store.BatchResult
	err := c.retry(func(ctx context.Context) error {
		var err error
		result, err = c.db.WriteBatch(ctx, batch, c.now())
		return err
	})
	if err != nil {
		increment(&c.counts.droppedStorage, uint64(len(batch)))
		if errors.Is(err, store.ErrBudget) {
			c.updateFault(faultDisk, faultDiskBudget)
		} else {
			c.updateFault(faultWrite, faultWrite)
			c.nextWriteProbe = time.Now().Add(writeProbeInterval)
		}
		return
	}
	increment(&c.counts.rejected, uint64(result.Rejected))
	increment(&c.counts.rejectedCardinality, uint64(result.Rejected))
	c.committed.Store(result.Committed)
	c.lastPersisted.Store(c.now().UTC().Format(time.RFC3339Nano))
	c.coverageStart.Store(result.CoverageStart)
	c.updateFault(faultWrite, 0)
	c.checkDisk()
}

func (c *Collector) recoverWrite(at time.Time) {
	if c.faults.Load()&faultWrite == 0 || c.faults.Load()&faultDisk != 0 || at.Before(c.nextWriteProbe) {
		return
	}
	err := c.retry(func(ctx context.Context) error { return c.db.ProbeWrite(ctx, c.run, c.now()) })
	c.nextWriteProbe = time.Now().Add(writeProbeInterval)
	if err != nil {
		c.updateFault(faultWrite, faultWrite)
		if errors.Is(err, store.ErrBudget) {
			c.updateFault(faultDisk, faultDiskBudget)
		}
		return
	}
	c.checkDisk()
	c.updateFault(faultWrite, 0)
}
func (c *Collector) save(finish bool) {
	at := c.now()
	err := c.retry(func(ctx context.Context) error {
		return c.db.SaveDiagnostics(ctx, c.counts.snapshot(), c.run, at, finish)
	})
	if err != nil {
		c.updateFault(faultDiagnostics, faultDiagnostics)
		return
	}
	if floor := at.Add(-c.cfg.RawRetention).UnixNano(); floor > c.retentionFloor.Load() {
		c.retentionFloor.Store(floor)
	}
	c.updateFault(faultDiagnostics, 0)
}

func (c *Collector) maintain() time.Duration {
	at := c.now()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	more, err := c.db.Maintain(ctx, at)
	cancel()
	if err != nil {
		increment(&c.counts.storageErrors, 1)
		c.updateFault(faultMaintenance, faultMaintenance)
		// A failed chunk backs off to the configured interval, not a tight loop.
		return c.cfg.MaintenanceInterval
	}
	c.updateFault(faultMaintenance, 0)
	if floor := at.Add(-c.cfg.RawRetention).UnixNano(); floor > c.retentionFloor.Load() {
		c.retentionFloor.Store(floor)
	}
	c.cleanupPending.Store(more)
	c.checkDisk()
	if more {
		return cleanupDelay
	}
	return c.cfg.MaintenanceInterval
}
func (c *Collector) work() {
	defer close(c.done)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	maintenance := time.NewTimer(c.cfg.MaintenanceInterval)
	defer maintenance.Stop()
	batch := make([]usage.Event, 0, c.cfg.BatchSize)
	for {
		select {
		case e, ok := <-c.queue:
			if !ok {
				c.flush(batch)
				c.save(true)
				return
			}
			batch = append(batch, e)
			if len(batch) >= c.cfg.BatchSize {
				c.flush(batch)
				clear(batch)
				batch = batch[:0]
				c.save(false)
			}
		case <-ticker.C:
			c.checkDisk()
			c.recoverWrite(time.Now())
			c.flush(batch)
			clear(batch)
			batch = batch[:0]
			c.save(false)
		case <-maintenance.C:
			// Each step is one bounded chunk, then yields to fresh events, flushes,
			// shutdown and independent readers before any prompt continuation.
			c.flush(batch)
			clear(batch)
			batch = batch[:0]
			maintenance.Reset(c.maintain())
			c.save(false)
		}
	}
}
func (c *Collector) Stop() error {
	c.stopOnce.Do(func() {
		c.admission.Lock()
		c.stopped = true
		close(c.queue)
		c.admission.Unlock()
		<-c.done
		c.operations.Lock()
		defer c.operations.Unlock()
		c.closed = true
		c.closeErr = c.db.Close()
	})
	return c.closeErr
}
func (c *Collector) Stopped() bool {
	c.admission.RLock()
	defer c.admission.RUnlock()
	return c.stopped
}
func (c *Collector) Query(ctx context.Context, f store.Filter) (store.QueryResult, error) {
	c.operations.RLock()
	defer c.operations.RUnlock()
	// Admission faults do not prove the committed history is unreadable.
	// Let the query-only connection decide; never manufacture empty results.
	if c.closed {
		return store.QueryResult{}, store.ErrUnavailable
	}
	return c.db.Query(ctx, f, c.now())
}
func (c *Collector) Coverage() store.Coverage {
	return store.MakeCoverage(c.coverageStart.Load(), c.retentionFloor.Load(), c.now(), c.cfg.RawRetention)
}
func (c *Collector) Snapshot() map[string]any {
	d := c.counts.snapshot()
	d["committed_events"] = c.committed.Load().(string)
	fault := ""
	active := []string{}
	mask := c.faults.Load()
	for _, f := range faultReasons {
		if mask&f.mask != 0 {
			active = append(active, f.reason)
		}
	}
	if len(active) != 0 {
		fault = active[0]
	}
	degraded := mask != 0 || c.counts.droppedQueue.Load() > 0 || c.counts.droppedStorage.Load() > 0 || c.counts.droppedStopped.Load() > 0 || c.counts.rejected.Load() > 0 || c.counts.storageErrors.Load() > 0 || c.unclean > 0
	stopped := c.Stopped()
	state := "running"
	if degraded {
		state = "degraded"
	}
	if stopped {
		state = "stopped"
	}
	return map[string]any{"state": state, "degraded": degraded, "reason": fault, "active_faults": active, "retention_cleanup_pending": c.cleanupPending.Load(), "last_failure_reason": c.lastFailure.Load().(string), "diagnostics": d, "queue_depth": len(c.queue), "disk_bytes": strconv.FormatInt(c.diskBytes.Load(), 10), "last_persisted_at": c.lastPersisted.Load().(string), "previous_unclean_runs": strconv.FormatUint(c.unclean, 10), "upstream_completeness": "unknown", "diagnostics_scope": "lifetime_best_effort"}
}
