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

// TestCarryAdoptableAcrossInterveningRun pins the K10 carry-binding gap (design revision 4): saved
// carry rows record only saved_at, not the run that produced them. A run that opened the outbox and
// took no carry (its take failed, or it never reached it) leaves the first run's rows in place, and
// a third run opening within the max age adopts them, although the second run came between and
// may have stored newer keys from that numbering.
func TestCarryAdoptableAcrossInterveningRun(t *testing.T) {
	ctx := context.Background()
	c := &lockedClock{t: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "data")
	open := func() *Outbox {
		o, err := Open(Options{Dir: dir, CapBytes: 1 << 40, Retention: 168 * time.Hour, Now: c.now})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	first := open()
	if _, err := first.SaveCarry(ctx, []OpenRequest{{RequestID: "carry-chain", NextInputSeq: 1, NextResponse: 1}},
		[]Numbering{{RequestID: "carry-chain", NextInputSeq: 1, NextResponse: 1}}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	c.add(10 * time.Second)
	second := open() // a run that never took the carry
	second.Close()
	c.add(10 * time.Second)
	third := open()
	defer third.Close()
	reqs, err := third.TakeOpenRequests(ctx, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	nums, err := third.TakeNumbering(ctx, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || len(nums) != 1 {
		t.Fatalf("the third run took %d open requests and %d numbering rows; 0.1.5 adopts the first run's carry past an intervening run", len(reqs), len(nums))
	}
}
