package uploader

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// MediaQueueSize bounds the in-memory media-fetch queue; a full queue drops the
// URL and counts dropped.media_fetch.
const MediaQueueSize = 256

// Media is the media-fetch sender: an in-memory queue of 256 MediaFetch values (the only place
// an unmasked media URL lives in the plugin), sent best-effort to POST /v1/media-fetch with
// backoff until 202 or expiry. Shutdown and a full queue drop and count media_fetch. URLs are
// never logged, persisted or put in status.
type Media struct {
	u  *Uploader
	in chan wire.MediaFetch

	mu      sync.Mutex
	pending []mediaItem
}

type mediaItem struct {
	mf       wire.MediaFetch
	next     time.Time
	attempts uint32
}

func newMedia(u *Uploader) *Media {
	return &Media{u: u, in: make(chan wire.MediaFetch, MediaQueueSize)}
}

// Enqueue queues one fetch without blocking; false when the queue is full.
func (m *Media) Enqueue(mf wire.MediaFetch) bool {
	if m.u.stopping() {
		return false
	}
	m.mu.Lock()
	n := len(m.pending)
	m.mu.Unlock()
	if n+len(m.in) >= MediaQueueSize {
		return false
	}
	select {
	case m.in <- mf:
		return true
	default:
		return false
	}
}

// Len returns the queued fetches.
func (m *Media) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) + len(m.in)
}

func (m *Media) dropAll() {
	m.mu.Lock()
	n := len(m.pending)
	m.pending = nil
	m.mu.Unlock()
	for {
		select {
		case <-m.in:
			n++
			continue
		default:
		}
		break
	}
	for i := 0; i < n; i++ {
		m.u.opts.Stats.Drop("media_fetch")
	}
}

func (m *Media) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		safego.Checkpoint("media_fetch")
		select {
		case <-ctx.Done():
			return
		case <-m.u.stop:
			return
		case mf := <-m.in:
			m.mu.Lock()
			m.pending = append(m.pending, mediaItem{mf: mf})
			m.mu.Unlock()
		case <-t.C:
		}
		m.sendDue(ctx)
	}
}

func (m *Media) sendDue(ctx context.Context) {
	now := time.Now()
	m.mu.Lock()
	items := m.pending
	m.pending = nil
	m.mu.Unlock()
	var keep []mediaItem
	for i, it := range items {
		if m.u.stopping() || ctx.Err() != nil {
			keep = append(keep, items[i:]...)
			break
		}
		if it.mf.ExpiresAt != nil && now.After(*it.mf.ExpiresAt) {
			m.u.opts.Stats.Drop("media_fetch")
			continue
		}
		if now.Before(it.next) || m.u.authPaused() {
			keep = append(keep, it)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.u.cl.MediaFetch(cctx, it.mf)
		cancel()
		if err == nil {
			continue
		}
		if errors.Is(err, client.ErrUnauthorized) {
			m.u.fail(err)
		}
		if errors.Is(err, client.ErrMalformed) || errors.Is(err, client.ErrTooLarge) {
			m.u.opts.Stats.Drop("media_fetch")
			continue
		}
		it.attempts++
		it.next = now.Add(backoff(it.attempts))
		keep = append(keep, it)
	}
	m.mu.Lock()
	m.pending = append(keep, m.pending...)
	m.mu.Unlock()
}
