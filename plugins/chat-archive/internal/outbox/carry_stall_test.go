package outbox

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/diskstall"
)

// lockedClock is a clock a test moves while another goroutine reads it.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestCarryStampPrecedesStalledCommit pins the K10 carry-freshness gap (design revision 3): SaveCarry
// stamps saved_at before its transaction begins, so a carry save stalled in its commit for longer
// than the take's max age is stored already stale, and the next instance's take finds nothing,
// although the save completed just before the take.
func TestCarryStampPrecedesStalledCommit(t *testing.T) {
	if err := diskstall.Install(); err != nil {
		t.Fatal(err)
	}
	c := &lockedClock{t: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "data")
	o, err := Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	hold := make(chan struct{})
	h := diskstall.Arm(diskstall.Rule{Dir: dir, Ops: diskstall.Sync, Files: diskstall.WAL, Hold: hold})
	t.Cleanup(h.Disarm)
	saved := make(chan error, 1)
	go func() {
		_, err := o.SaveCarry(context.Background(), nil, []Numbering{{RequestID: "carry-stall", NextInputSeq: 1, NextResponse: 1}})
		saved <- err
	}()
	if !h.WaitFor(5*time.Second, func(s diskstall.Stats) bool { return s.Waiting == 1 }) {
		t.Fatal("the carry save never reached the held sync")
	}
	c.add(3 * time.Minute) // the stall outlasts CarryMaxAge (2 min)
	close(hold)
	h.Disarm()
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	nums, err := o.TakeNumbering(context.Background(), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(nums) != 0 {
		t.Fatalf("the take found %d rows; 0.1.5 stamps the save before its stalled commit, so none is fresh", len(nums))
	}
}
