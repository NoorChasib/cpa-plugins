// Package uploader moves outbox envelopes to the archive: have/need, piece PUTs
// for large pieces, CAB1 commits, per-seq outcomes, local dead letters with tombstones, epoch
// replay, and the whole-request error rules (a 401 or 403 is never retried in a loop). It also
// runs the heartbeat (directly over HTTP, never queued), the in-memory media-fetch sender, and
// the saving of the dropped counters to the outbox, which the next boot reports if this one
// crashes (outbox.BootCounters).
package uploader

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Timing knobs (variables so tests can shorten them).
var (
	AuthPause        = 15 * time.Minute
	TooOldPause      = time.Hour
	MaxBackoff       = 5 * time.Minute
	IdleWait         = time.Second
	StopDeadline     = 5 * time.Second
	HeartbeatEvery   = 60 * time.Second
	FinalHeartbeat   = 2 * time.Second
	DropSaveGap      = 100 * time.Millisecond // a burst of drops is saved in one write
	DropSaveRetry    = time.Second            // after a failed save
	MissingRefLimit  = 3
	haveCacheEntries = 200_000
)

// Options configure an uploader.
type Options struct {
	BaseURL, Token string
	HTTPClient     *client.Client // optional; built from BaseURL and Token when nil
	UserAgent      string
	Outbox         *outbox.Outbox
	Stats          *hooks.Stats
	OnExact        func(wire.ExactSet) // the server's exact set changed (rebuild the masker)
	OnDead         func(key string)    // an envelope was dead-lettered (prefix map)
	OnAck          func(key string)    // the archive acked an envelope ok or duplicate (prefix map)
	Heartbeat      func(state string) wire.Heartbeat

	// Boot names this boot (BootID, StartedAt, Component, Version; N and Dropped are filled in).
	// With a BootID the uploader saves the dropped counters to the outbox whenever they change
	// (SaveDrops), and before its heartbeats reports every other boot's saved record in a heartbeat
	// under that boot's id, deleting the record once the archive took it.
	Boot outbox.BootCounters
}

// StateRecovered is the heartbeat state of a previous boot's saved counters, reported by the next
// boot (outbox.BootCounters).
const StateRecovered = "recovered"

// Uploader is one running uploader.
type Uploader struct {
	opts   Options
	cl     *client.Client
	limits atomic.Pointer[wire.Limits]
	have   *haveCache
	wake   chan struct{}

	replay  atomic.Bool
	exactQ  atomic.Bool
	synced  bool
	exactRv atomic.Uint32

	mu       sync.Mutex
	st       wire.UploaderStats
	pauseTil time.Time
	missing  map[uint64]int

	media *Media

	lastN      atomic.Uint64 // n of the last heartbeat built
	saveMu     sync.Mutex
	savedTotal uint64                // dropped total of the last saved record (saveMu)
	savedN     uint64                // n of the last saved record (saveMu)
	boots      []outbox.BootCounters // other boots' records still to report (heartbeat loop)
	bootsRead  bool                  // boots was loaded (heartbeat loop)

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// New builds an uploader (not started).
func New(opts Options) *Uploader {
	u := &Uploader{opts: opts, have: newHaveCache(haveCacheEntries), wake: make(chan struct{}, 1), missing: map[uint64]int{}, stop: make(chan struct{})}
	l := wire.DefaultLimits
	u.limits.Store(&l)
	u.cl = opts.HTTPClient
	if u.cl == nil {
		u.cl = client.New(opts.BaseURL, opts.Token, client.Options{UserAgent: opts.UserAgent, OnServerState: u.onState})
	}
	m := opts.Outbox.Meta()
	u.st.EpochSeen, u.st.ArchiveID, u.st.ReplayGapFrom, u.st.ReplayGapTo = m.EpochSeen, m.ArchiveID, m.ReplayGapFrom, m.ReplayGapTo
	u.st.LastErrorClass = "none"
	u.media = newMedia(u)
	return u
}

// Start launches the upload loop, the heartbeat loop and the media sender.
func (u *Uploader) Start() {
	u.ctx, u.cancel = context.WithCancel(context.Background())
	rec := u.opts.Stats.Panic
	safego.Loop(u.ctx, &u.wg, "uploader", rec, u.run)
	safego.Loop(u.ctx, &u.wg, "heartbeat", rec, u.heartbeats)
	safego.Loop(u.ctx, &u.wg, "media_fetch", rec, u.media.run)
	if u.opts.Boot.BootID != "" {
		safego.Loop(u.ctx, &u.wg, "drop_save", rec, u.saveDrops)
	}
}

// Stop is StopBy with no deadline but StopDeadline and FinalHeartbeat.
func (u *Uploader) Stop() { u.StopBy(time.Time{}) }

// StopBy stops every loop. In-flight HTTP gets StopDeadline, then its context is cancelled; the
// outbox is durable so nothing is lost. A final heartbeat (state stopping) is attempted within
// FinalHeartbeat. Both also end by deadline when it is not zero: the in-flight wait leaves
// FinalHeartbeat before it for the heartbeat, which is skipped once deadline has passed. Queued
// media fetches are dropped and counted. The owner calls SaveDrops after it stopped capture, whose
// shutdown counts dropped.stopped after this final heartbeat.
func (u *Uploader) StopBy(deadline time.Time) {
	u.once.Do(func() {
		wait, beat := StopDeadline, FinalHeartbeat
		if !deadline.IsZero() {
			wait = min(wait, max(time.Until(deadline)-FinalHeartbeat, 0))
		}
		close(u.stop)
		done := make(chan struct{})
		go func() { u.wg.Wait(); close(done) }()
		t := time.NewTimer(wait)
		select {
		case <-done:
		case <-t.C:
		}
		t.Stop()
		u.cancel()
		<-done
		u.media.dropAll()
		if !deadline.IsZero() {
			beat = min(beat, time.Until(deadline))
		}
		// The final heartbeat goes after every loop exited, so it is the last one sent.
		safego.Run("heartbeat", u.opts.Stats.Panic, func() {
			if u.opts.Heartbeat == nil || u.authPaused() || beat <= 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), beat)
			defer cancel()
			hb := u.heartbeat("stopping")
			_ = u.SaveDrops(ctx) // the saved n covers this heartbeat before it goes out (heartbeats)
			_, _ = u.cl.Heartbeat(ctx, hb)
		})
	})
}

// Notify wakes the upload loop after the writer committed envelopes.
func (u *Uploader) Notify() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// Media returns the media-fetch queue.
func (u *Uploader) Media() *Media { return u.media }

// Limits returns the server's limits (defaults until the first sources/self).
func (u *Uploader) Limits() wire.Limits { return *u.limits.Load() }

// Stats returns the heartbeat uploader block.
func (u *Uploader) Stats() wire.UploaderStats {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st
}

func (u *Uploader) stopping() bool {
	select {
	case <-u.stop:
		return true
	default:
		return false
	}
}

// onState triggers a replay on any response whose epoch or archive id differs (the archive was
// restored or replaced).
func (u *Uploader) onState(st wire.ServerState) {
	m := u.opts.Outbox.Meta()
	if m.ArchiveID != "" && (st.Epoch != m.EpochSeen || st.ArchiveID != m.ArchiveID) {
		u.replay.Store(true)
	}
}

func (u *Uploader) authPaused() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return time.Now().Before(u.pauseTil) && u.st.LastErrorClass == "401"
}

func (u *Uploader) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-u.stop:
	case <-t.C:
	}
}

func (u *Uploader) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-u.stop:
	case <-u.wake:
	case <-t.C:
	}
}

func (u *Uploader) run(ctx context.Context) {
	for {
		safego.Checkpoint("uploader")
		if ctx.Err() != nil || u.stopping() {
			return
		}
		u.mu.Lock()
		pause := time.Until(u.pauseTil)
		u.mu.Unlock()
		if pause > 0 {
			u.sleep(ctx, pause)
			continue
		}
		if !u.synced || u.replay.Load() || u.exactQ.Load() {
			if err := u.sync(ctx); err != nil {
				u.fail(err)
				continue
			}
		}
		lim := u.Limits()
		items, err := u.opts.Outbox.Pending(ctx, time.Now(), min(lim.CommitEnvelopes, wire.TargetCommitEnvelopes), min(lim.CommitRaw, wire.TargetCommitBytes))
		if err != nil {
			u.sleep(ctx, IdleWait)
			continue
		}
		if len(items) == 0 {
			u.wait(ctx, IdleWait)
			continue
		}
		if err := u.upload(ctx, items); err != nil {
			u.fail(err)
			continue
		}
	}
}

// sync reads sources/self: limits, exact set, and (on first contact or a trigger) the epoch.
func (u *Uploader) sync(ctx context.Context) error {
	self, err := u.cl.SourcesSelf(ctx)
	if err != nil {
		return err
	}
	if self.Limits.CommitRaw > 0 {
		l := self.Limits
		u.limits.Store(&l)
	}
	ob := u.opts.Outbox
	m := ob.Meta()
	switch {
	case m.ArchiveID == "":
		if err := ob.SetServer(ctx, self.Epoch, self.ArchiveID); err != nil {
			return err
		}
	case self.Epoch != m.EpochSeen || self.ArchiveID != m.ArchiveID:
		hw := self.Outboxes[m.OutboxID]
		from, to, err := ob.Replay(ctx, self.Epoch, self.ArchiveID, hw)
		if err != nil {
			return err
		}
		u.have.clear()
		u.mu.Lock()
		u.missing = map[uint64]int{}
		u.st.ReplayGapFrom, u.st.ReplayGapTo = from, to
		u.mu.Unlock()
		u.opts.Stats.SetFault("replay_gap", from != 0)
	}
	u.replay.Store(false)
	u.exactQ.Store(false)
	u.mu.Lock()
	u.st.EpochSeen, u.st.ArchiveID = self.Epoch, self.ArchiveID
	u.mu.Unlock()
	u.exactRv.Store(self.Exact.Rev)
	if u.opts.OnExact != nil {
		u.opts.OnExact(self.Exact)
	}
	u.synced = true
	return nil
}

// errClass maps an error to the heartbeat's last_error_class.
func errClass(err error) string {
	var ec *client.EpochChangedError
	var ra *client.RetryAfterError
	var se *client.StatusError
	switch {
	case errors.Is(err, client.ErrUnauthorized):
		return "401"
	case errors.As(err, &ec):
		return "409"
	case errors.Is(err, client.ErrTooLarge):
		return "413"
	case errors.As(err, &ra):
		return "429"
	case errors.As(err, &se):
		if se.StatusCode >= 500 {
			return "5xx"
		}
		return "4xx"
	case errors.Is(err, client.ErrMalformed), errors.Is(err, client.ErrUnsupportedProtocol):
		return "400"
	}
	return "net"
}

// fail applies the whole-request error rules that need no batch splitting.
func (u *Uploader) fail(err error) {
	if err == nil || u.stopping() {
		return
	}
	cls := errClass(err)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.st.LastErrorClass = cls
	if u.st.ConsecutiveFailures < 1<<31 {
		u.st.ConsecutiveFailures++
	}
	var ra *client.RetryAfterError
	var ec *client.EpochChangedError
	now := time.Now()
	switch {
	case errors.Is(err, client.ErrUnauthorized):
		// Never in a loop: stop uploading for 15 minutes.
		u.pauseTil = now.Add(AuthPause)
		u.opts.Stats.SetFault("auth_failed", true)
	case errors.Is(err, client.ErrUnsupportedProtocol):
		u.pauseTil = now.Add(TooOldPause)
		u.opts.Stats.SetFault("server_too_old", true)
	case errors.As(err, &ec):
		u.replay.Store(true)
	case errors.As(err, &ra) && ra.After > 0:
		u.pauseTil = now.Add(min(ra.After, MaxBackoff))
	default:
		u.pauseTil = now.Add(backoff(u.st.ConsecutiveFailures))
	}
}

func backoff(n uint32) time.Duration {
	d := time.Second << min(n, 9)
	d = min(d, MaxBackoff)
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

func (u *Uploader) succeeded() {
	u.mu.Lock()
	u.st.LastSuccessAt = time.Now().UTC()
	u.st.LastErrorClass = "none"
	u.st.ConsecutiveFailures = 0
	u.mu.Unlock()
	u.opts.Stats.SetFault("auth_failed", false)
	u.opts.Stats.SetFault("server_too_old", false)
}

// upload sends one batch, splitting it on 400 and 413 until the item at fault is alone; that
// item becomes a local dead letter with a tombstone.
func (u *Uploader) upload(ctx context.Context, items []outbox.Item) error {
	err := u.commit(ctx, items)
	if err == nil {
		u.succeeded()
		return nil
	}
	if errors.Is(err, client.ErrMalformed) || errors.Is(err, client.ErrTooLarge) {
		if len(items) > 1 {
			h := len(items) / 2
			if e := u.upload(ctx, items[:h]); e != nil {
				return e
			}
			return u.upload(ctx, items[h:])
		}
		it := items[0]
		if it.Tomb != nil {
			return err // a tombstone is never dead-lettered; retried like any pending item
		}
		reason := wire.TombRejectedLocal
		if errors.Is(err, client.ErrTooLarge) {
			reason = wire.TombRejectedTooLarge
		}
		if aerr := u.opts.Outbox.Apply(ctx, u.Stats().EpochSeen, 0, []outbox.Outcome{{Seq: it.Seq, Kind: outbox.OutDeadLocal, Reason: reason}}); aerr != nil {
			return aerr
		}
		u.dead(it)
		return nil
	}
	return err
}

func (u *Uploader) dead(it outbox.Item) {
	if u.opts.OnDead != nil && it.Kind == wire.KindExchangeBegin {
		u.opts.OnDead(it.Key)
	}
}

// commit runs need, PUTs and one commit for items, then applies the per-seq outcomes.
func (u *Uploader) commit(ctx context.Context, items []outbox.Item) error {
	ob := u.opts.Outbox
	lim := u.Limits()
	m := ob.Meta()
	var refs []wire.Hash
	seen := map[wire.Hash]bool{}
	for _, it := range items {
		for _, h := range it.Refs {
			if !seen[h] && !u.have.has(h) {
				seen[h] = true
				refs = append(refs, h)
			}
		}
	}
	var need []wire.Hash
	staged := map[wire.Hash]bool{}
	for i := 0; i < len(refs); i += lim.NeedHashes {
		chunk := refs[i:min(i+lim.NeedHashes, len(refs))]
		n, s, _, err := u.cl.Need(ctx, chunk)
		if err != nil {
			return err
		}
		need = append(need, n...)
		for _, h := range s {
			staged[h] = true
			u.have.remove(h)
		}
		needSet := map[wire.Hash]bool{}
		for _, h := range n {
			needSet[h] = true
		}
		for _, h := range chunk {
			if !needSet[h] && !staged[h] {
				u.have.add(h)
			}
		}
	}
	var envBytes int64
	for _, it := range items {
		envBytes += it.Size()
	}
	// Inline pieces fill the commit up to the producer target (8 MiB), not up
	// to commit_raw: the commit's CAB1 buffer and zstd copy sit outside every memory budget, so
	// a commit stays near the target even when the server allows 32 MiB. Larger needs go by PUT.
	inlineLimit := min(lim.CommitRaw-(1<<20), wire.TargetCommitBytes)
	var pieces []wire.BatchPiece
	inline := envBytes
	rejected := map[wire.Hash]bool{}
	for _, h := range need {
		sp, err := ob.StoredPiece(ctx, h)
		if err != nil {
			continue // a lost piece: the commit answers missing_ref for its envelope
		}
		size := sp.RawLen + wire.CAB1RecordOverhead
		if sp.RawLen > lim.PieceInline || inline+size > inlineLimit {
			// A PUT sends the stored bytes as they are: a compressed piece is never decoded and
			// re-encoded, so a large piece is held once, in its stored form.
			if sp.Zstd {
				err = u.cl.PutPieceZstd(ctx, h, sp.Kind, sp.ContentType, sp.Data)
			} else {
				err = u.cl.PutPiece(ctx, h, sp.Kind, sp.ContentType, sp.Data)
			}
			var se *client.StatusError
			if errors.As(err, &se) && se.StatusCode == http.StatusUnprocessableEntity {
				// The archive rejected the piece itself (mask_violation): it will refuse it on
				// every retry, so its envelopes are dead letters, not a reason to stall.
				rejected[h] = true
				continue
			}
			if err != nil {
				return err
			}
			continue
		}
		raw, err := ob.Raw(sp)
		if err != nil {
			continue // an unreadable piece: the commit answers missing_ref for its envelope
		}
		inline += size
		pieces = append(pieces, wire.BatchPiece{Hash: h, Kind: sp.Kind, ContentType: sp.ContentType, Raw: raw})
	}
	if len(rejected) > 0 {
		var err error
		if items, err = u.deadForPieces(ctx, items, rejected); err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
	}
	var envs [][]byte
	var tombs []wire.Tombstone
	for _, it := range items {
		if it.Tomb != nil {
			tombs = append(tombs, *it.Tomb)
		} else {
			envs = append(envs, it.Envelope)
		}
	}
	resp, err := u.cl.Commit(ctx, m.EpochSeen, m.ArchiveID, pieces, envs, tombs)
	if err != nil {
		return err
	}
	for _, h := range resp.Staged {
		staged[h] = true
		u.have.remove(h)
	}
	return u.apply(ctx, items, resp, staged)
}

// deadForPieces dead-letters (reason mask_violation, requeue-eligible) every envelope whose refs
// hold a piece the archive rejected on PUT, and returns the items left to commit.
func (u *Uploader) deadForPieces(ctx context.Context, items []outbox.Item, rejected map[wire.Hash]bool) ([]outbox.Item, error) {
	var keep, dead []outbox.Item
	var outs []outbox.Outcome
	for _, it := range items {
		hit := false
		for _, h := range it.Refs {
			if rejected[h] {
				hit = true
				break
			}
		}
		if hit && it.Tomb == nil {
			dead = append(dead, it)
			outs = append(outs, outbox.Outcome{Seq: it.Seq, Kind: outbox.OutDead, Reason: string(wire.ReasonMaskViolation)})
			continue
		}
		keep = append(keep, it)
	}
	if err := u.opts.Outbox.Apply(ctx, u.Stats().EpochSeen, 0, outs); err != nil {
		return nil, err
	}
	for _, it := range dead {
		u.dead(it)
	}
	return keep, nil
}

func (u *Uploader) apply(ctx context.Context, items []outbox.Item, resp wire.CommitResponse, staged map[wire.Hash]bool) error {
	ob := u.opts.Outbox
	m := ob.Meta()
	bySeq := map[uint64]wire.CommitResult{}
	for _, r := range resp.Results {
		bySeq[r.Seq] = r
	}
	now := time.Now()
	var outs []outbox.Outcome
	var deadItems []outbox.Item
	var ackedKeys []string
	var rekey []uint64
	u.mu.Lock()
	for _, it := range items {
		r, ok := bySeq[it.Seq]
		oc := outbox.Outcome{Seq: it.Seq, Tomb: it.Tomb != nil}
		switch {
		case !ok:
			oc.Kind, oc.NextAttempt = outbox.OutRetry, now.Add(backoff(uint32(it.Attempts+1)))
		case r.Status == wire.StatusOK || r.Status == wire.StatusDuplicate || r.Status == wire.StatusConflict:
			oc.Kind = outbox.OutAck
			if r.Status != wire.StatusConflict && it.Tomb == nil {
				ackedKeys = append(ackedKeys, it.Key) // a conflict holds another body: never a base
			}
			if r.Status == wire.StatusConflict && it.Tomb == nil && it.Kind == wire.KindExchangeDone && bareCompletionKey(it.Key) {
				// The archive holds another lifecycle's completion of a RequestID CPA reused.
				rekey = append(rekey, it.Seq)
			}
			delete(u.missing, it.Seq)
			for _, h := range it.Refs {
				if !staged[h] {
					u.have.add(h)
				}
			}
		case r.Status == wire.StatusRejected && it.Tomb == nil:
			oc.Kind, oc.Reason = outbox.OutDead, string(r.Reason)
			deadItems = append(deadItems, it)
		case r.Status == wire.StatusRetry && r.Reason == wire.ReasonMissingRef && it.Tomb == nil:
			for _, h := range r.Missing {
				u.have.remove(h)
			}
			u.missing[it.Seq]++
			if u.missing[it.Seq] >= MissingRefLimit {
				delete(u.missing, it.Seq)
				oc.Kind, oc.Reason = outbox.OutDeadLocal, wire.TombMissingRef
				deadItems = append(deadItems, it)
			} else {
				oc.Kind, oc.NextAttempt = outbox.OutRetry, now
			}
		default:
			oc.Kind, oc.NextAttempt = outbox.OutRetry, now.Add(backoff(uint32(it.Attempts+1)))
		}
		outs = append(outs, oc)
	}
	u.mu.Unlock()
	// Queue each re-keyed completion before its conflict is acked: if the process stops in
	// between, the conflict is sent again and answered again, and the second insert is a no-op.
	for _, seq := range rekey {
		if ok, err := ob.Rekey(ctx, seq); err == nil && ok {
			u.opts.Stats.Count("completion_rekeyed.archive", 1)
			u.opts.Stats.Count("reused_request_id", 1)
			u.Notify()
		}
	}
	if err := ob.Apply(ctx, resp.Epoch, resp.HW[m.OutboxID], outs); err != nil {
		return err
	}
	for _, it := range deadItems {
		u.dead(it)
	}
	if u.opts.OnAck != nil {
		for _, k := range ackedKeys {
			u.opts.OnAck(k)
		}
	}
	return nil
}

// bareCompletionKey reports whether key is "ex/<rid>/completion".
func bareCompletionKey(key string) bool {
	rid, ok := strings.CutPrefix(key, "ex/")
	if !ok {
		return false
	}
	rid, ok = strings.CutSuffix(rid, "/completion")
	return ok && rid != "" && !strings.Contains(rid, "/")
}

// heartbeats sends a heartbeat every HeartbeatEvery, directly over HTTP, after reporting any other
// boot's saved counters (reportBoots). A changed exact_rev makes the upload loop refresh the exact set.
//
// Once this boot has a saved record, each heartbeat re-saves it with its n and the counters read after
// it was built, before it is sent (SaveDrops). So the record's n is at least every n this boot sent, and
// its counters at least every sent heartbeat's: the next boot's recovered heartbeat (n+1) never repeats
// an n, never carries fewer drops than the archive already has for this boot, and a drop counted just
// before a heartbeat (inside DropSaveGap) is never missing from the record. That is one write a minute
// at most, and none while the boot has counted no drop.
func (u *Uploader) heartbeats(ctx context.Context) {
	if u.opts.Heartbeat == nil {
		select {
		case <-ctx.Done():
		case <-u.stop:
		}
		return
	}
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		safego.Checkpoint("heartbeat")
		if !u.authPaused() {
			hb := u.heartbeat("")
			_ = u.SaveDrops(ctx) // a failed save is retried by the next drop or heartbeat
			u.reportBoots(ctx, hb)
			if !u.authPaused() {
				hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				resp, err := u.cl.Heartbeat(hctx, hb)
				cancel()
				if err == nil {
					if resp.ExactRev != u.exactRv.Load() {
						u.exactQ.Store(true)
						u.Notify()
					}
				} else if errors.Is(err, client.ErrUnauthorized) {
					u.fail(err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-u.stop:
			return
		case <-t.C:
		}
	}
}

// heartbeat builds one heartbeat and remembers its n for the saved counters.
func (u *Uploader) heartbeat(state string) wire.Heartbeat {
	hb := u.opts.Heartbeat(state)
	u.lastN.Store(hb.N)
	return hb
}

// reportBoots sends, before cur (this boot's heartbeat of the same tick, so the archive's newest
// heartbeat stays this boot's), one heartbeat per other boot whose counters the outbox holds, and
// deletes each record once the archive took it. A record that fails to send stays for the next tick,
// and the records after it wait with it (the archive is unreachable or busy). A record the archive
// refuses for good (refusedForGood: a 4xx other than 401, 403 and 429, such as an archive that does
// not take state recovered) is deleted and counted in recovered_refused, and the next record goes, so
// one such record never holds the others back or is sent every minute forever.
// The recovered heartbeat carries that boot's id, start, version and dropped counters; its outbox,
// uploader and mask fields are cur's (they describe the outbox and the sender, which the archive's
// checks read from a source's newest heartbeat), and every other per-boot counter is empty.
func (u *Uploader) reportBoots(ctx context.Context, cur wire.Heartbeat) {
	if u.opts.Boot.BootID == "" {
		return
	}
	ob := u.opts.Outbox
	if !u.bootsRead {
		recs, err := ob.BootCounters(ctx)
		if err != nil {
			return // read again next tick
		}
		u.bootsRead = true
		for _, r := range recs {
			if r.BootID != u.opts.Boot.BootID {
				u.boots = append(u.boots, r)
			}
		}
	}
	for len(u.boots) > 0 {
		r := u.boots[0]
		hb := cur
		hb.BootID, hb.N, hb.StartedAt, hb.Component, hb.Version, hb.State = r.BootID, r.N+1, r.StartedAt, r.Component, r.Version, StateRecovered
		hb.Dropped = r.Dropped
		if hb.Dropped == nil {
			hb.Dropped = map[string]uint64{}
		}
		hb.LastHookAt, hb.LastUsageAt = map[string]time.Time{}, nil
		hb.Calls, hb.HookErrors, hb.Panics = map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
		hb.HookP50us, hb.HookP99us = map[string]uint32{}, map[string]uint32{}
		hb.Runtime, hb.Masker, hb.Importer, hb.Faults = wire.RuntimeStats{}, wire.MaskerStats{}, nil, []string{}
		hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := u.cl.Heartbeat(hctx, hb)
		cancel()
		if err != nil {
			if errors.Is(err, client.ErrUnauthorized) {
				u.fail(err)
				return
			}
			if !refusedForGood(err) {
				return
			}
			if err = ob.DeleteBootCounters(ctx, r.BootID); err != nil {
				return
			}
			u.opts.Stats.Count("recovered_refused", 1)
			u.boots = u.boots[1:]
			continue
		}
		if err = ob.DeleteBootCounters(ctx, r.BootID); err != nil {
			return // reported again next tick: the archive takes the same total twice as no new drops
		}
		u.boots = u.boots[1:]
	}
}

// refusedForGood reports whether a heartbeat error is the archive's final answer for that heartbeat: a
// 400 (other than unsupported_protocol, which a newer archive fixes), a 413, or another 4xx answer that
// is not a timeout. 401 and 403 (ErrUnauthorized), 429 and 503 (busy), 5xx and network errors are not.
func refusedForGood(err error) bool {
	if errors.Is(err, client.ErrMalformed) || errors.Is(err, client.ErrTooLarge) {
		return true
	}
	var se *client.StatusError
	return errors.As(err, &se) && se.StatusCode >= 400 && se.StatusCode < 500 && se.StatusCode != http.StatusRequestTimeout
}

// saveDrops saves the dropped counters whenever a drop signals, a burst in one write.
func (u *Uploader) saveDrops(ctx context.Context) {
	sig := u.opts.Stats.DropSignal()
	for {
		safego.Checkpoint("drop_save")
		var retry <-chan time.Time
		if err := u.SaveDrops(ctx); err != nil && ctx.Err() == nil && !u.stopping() {
			retry = time.After(DropSaveRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-u.stop:
			return
		case <-sig:
		case <-retry:
		}
		u.sleep(ctx, DropSaveGap)
	}
}

// SaveDrops saves this boot's dropped counters, with the n of the last heartbeat built, to the outbox
// (outbox.BootCounters) unless none were counted or neither they nor that n changed since the last
// save. Without Options.Boot it does nothing.
func (u *Uploader) SaveDrops(ctx context.Context) error {
	if u.opts.Boot.BootID == "" {
		return nil
	}
	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	d := u.opts.Stats.Dropped()
	var total uint64
	for _, v := range d {
		total += v
	}
	n := u.lastN.Load()
	if total == 0 || total == u.savedTotal && n == u.savedN {
		return nil
	}
	b := u.opts.Boot
	b.N, b.Dropped = n, d
	if err := u.opts.Outbox.SaveBootCounters(ctx, b); err != nil {
		return err
	}
	u.savedTotal, u.savedN = total, n
	return nil
}

// haveCache is the in-memory have-cache: a FIFO-bounded set of hashes the archive holds outside
// the staging tier (a staged piece may still be removed, so it is never cached).
type haveCache struct {
	mu   sync.Mutex
	set  map[wire.Hash]uint32
	ring []wire.Hash
	next uint32
	full bool
}

func newHaveCache(n int) *haveCache {
	return &haveCache{set: map[wire.Hash]uint32{}, ring: make([]wire.Hash, n)}
}

func (c *haveCache) has(h wire.Hash) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.set[h]
	return ok
}

func (c *haveCache) add(h wire.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.set[h]; ok {
		return
	}
	if c.full {
		old := c.ring[c.next]
		if i, ok := c.set[old]; ok && i == c.next {
			delete(c.set, old)
		}
	}
	c.ring[c.next] = h
	c.set[h] = c.next
	c.next++
	if int(c.next) == len(c.ring) {
		c.next, c.full = 0, true
	}
}

func (c *haveCache) remove(h wire.Hash) {
	c.mu.Lock()
	delete(c.set, h)
	c.mu.Unlock()
}

func (c *haveCache) clear() {
	c.mu.Lock()
	c.set = map[wire.Hash]uint32{}
	c.next, c.full = 0, false
	c.mu.Unlock()
}

func (c *haveCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.set)
}
