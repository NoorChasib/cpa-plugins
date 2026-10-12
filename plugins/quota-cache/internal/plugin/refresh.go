package plugin

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
)

// refreshStartupAge is how old a request found when the poller starts must be
// to count as applied already. The ids acted on are held in memory only, so
// a restart repeats a round only for a request made in the minute before it,
// while a reset spent just before the restart is still honoured.
const refreshStartupAge = time.Minute

// refreshCheckEvery is how often the poller looks at the refresh request file
// between ticks. A tick comes once per request-spacing, which may be up to an
// hour, and a request more than client.RefreshRequestMaxAge old when it is
// first seen is never acted on. Looking costs one Lstat.
const refreshCheckEvery = 2 * time.Second

// refreshWatch follows the refresh request file Quota Glance writes after a
// banked reset (client.RefreshRequestPath). Only the poller goroutine calls
// due: before each Step, and every refreshCheckEvery between them, so a
// round starts between polls and never waits on one. The status reads
// requestedAt.
type refreshWatch struct {
	path string
	// started is set by the first read that settles, which the startup rule
	// applies to.
	started bool
	// stamp is the file as last read; the file is read again only when it
	// changes, or when that read did not settle (retry), so a malformed file
	// is warned about once per change.
	stamp refreshStamp
	// retry is set when the last read failed for a reason other than what
	// the file holds: it could not be looked at, opened or read, as when CPA
	// is out of file descriptors. The next check reads it again whether or
	// not its stamp changed, so a file whose mode is fixed is read at once,
	// and the same failure on the same file is not warned about again.
	retry bool
	// load reads the file; tests replace it to fail a read.
	load func(path string) ([]client.RefreshRequest, error)
	// applied is every request id acted on, or passed over, with its time. An
	// id whose time is no longer live can never be acted on again, so it is
	// forgotten.
	applied map[string]time.Time
	// requestedAt is the time of the newest request that started a round, or
	// brought its credential forward; nil until one does.
	requestedAt atomic.Pointer[time.Time]
}

func newRefreshWatch(snapshotPath string) *refreshWatch {
	return &refreshWatch{path: client.RefreshRequestPath(snapshotPath), applied: map[string]time.Time{}, load: client.LoadRefreshRequests}
}

// refreshStamp is what tells the file has changed: whether it is there, and
// its identity (device and inode), size and modification time.
type refreshStamp struct {
	exists bool
	// info is Lstat's answer, nil when it failed.
	info fs.FileInfo
}

func stampRefresh(path string) refreshStamp {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return refreshStamp{}
	}
	if err != nil {
		return refreshStamp{exists: true}
	}
	return refreshStamp{exists: true, info: info}
}

func (a refreshStamp) same(b refreshStamp) bool {
	if a.exists != b.exists || (a.info == nil) != (b.info == nil) {
		return false
	}
	if a.info == nil {
		return true
	}
	return os.SameFile(a.info, b.info) && a.info.Size() == b.info.Size() && a.info.ModTime().Equal(b.info.ModTime())
}

// due returns the requests to act on now, oldest first: those not applied
// yet that are still live. The first read to settle, as the poller starts,
// marks every request at least refreshStartupAge old as applied without
// returning it. failed is the read's error, once per change of the file: a
// file that could not be read at all (client.ErrRefreshNotRead) is read again
// at the next check, and failing again unchanged is not reported again.
func (w *refreshWatch) due(now time.Time) (requests []client.RefreshRequest, failed error) {
	stamp := stampRefresh(w.path)
	if w.started && !w.retry && stamp.same(w.stamp) {
		return nil, nil
	}
	again := w.retry && stamp.same(w.stamp)
	w.stamp = stamp
	found, err := w.load(w.path)
	w.retry = errors.Is(err, client.ErrRefreshNotRead)
	if w.retry {
		if again {
			err = nil
		}
		return nil, err
	}
	for _, r := range found {
		if _, done := w.applied[r.ID]; done {
			continue
		}
		w.applied[r.ID] = r.At
		if !r.Live(now) || (!w.started && now.Sub(r.At) >= refreshStartupAge) {
			continue
		}
		requests = append(requests, r)
	}
	w.started = true
	for id, at := range w.applied {
		if now.Sub(at) > client.RefreshRequestMaxAge {
			delete(w.applied, id)
		}
	}
	return requests, err
}

// requested records the time of a request that was acted on.
func (w *refreshWatch) requested(at time.Time) {
	if last := w.requestedAt.Load(); last == nil || at.After(*last) {
		w.requestedAt.Store(&at)
	}
}

// followRefresh acts on the refresh requests the file holds that it has not
// acted on yet, at now, between the poller's Steps. Each request whose
// credential is a polled entry starts a round, or brings its credential to
// the front of the one under way, and is logged with how many credentials the
// round has left: never the auth index. It reports whether any request was
// acted on. A file that cannot be read is warned about once per change, and
// changes nothing; one that could not be opened at all is read again at every
// check until it can be.
func (p *Plugin) followRefresh(ctx context.Context, current *cache.Cache, watch *refreshWatch, now time.Time) (acted bool) {
	requests, failed := watch.due(now)
	switch {
	case errors.Is(failed, client.ErrRefreshNotRead):
		p.host.Log(ctx, "warn", "quota-cache cannot open the refresh request file Quota Glance writes; it tries again every two seconds", nil)
	case failed != nil:
		p.host.Log(ctx, "warn", "quota-cache cannot read the refresh request file Quota Glance writes; it is ignored until it changes", nil)
	}
	for _, r := range requests {
		if !current.RequestRound(client.Key(r.Provider, r.AuthIndex), now) {
			continue
		}
		acted = true
		watch.requested(r.At)
		p.host.Log(ctx, "info", "quota-cache refreshing every account after a banked reset", map[string]any{"provider": r.Provider, "accounts": current.RoundRemaining()})
	}
	return acted
}

// refreshStatus is the status route's refresh block: when the newest request
// acted on was made, null before any, and how many credentials its round has
// still to poll.
type refreshStatus struct {
	RequestedAt *time.Time `json:"requested_at"`
	Remaining   int        `json:"remaining"`
}

func refreshStatusOf(current *cache.Cache, watch *refreshWatch) refreshStatus {
	status := refreshStatus{Remaining: current.RoundRemaining()}
	if watch != nil {
		status.RequestedAt = watch.requestedAt.Load()
	}
	return status
}
