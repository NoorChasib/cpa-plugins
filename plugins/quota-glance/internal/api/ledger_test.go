package api

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1789012800, 0)} }

func (c *fakeClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func pressID(n int) string { return fmt.Sprintf("press-%016d", n) }

// ledgerSpend stands in for spend: the first copy of a press does the work and
// records an answer, every later copy is handed it.
func ledgerSpend(l *ledger, id, credential string, calls *atomic.Int32, hold time.Duration) protocol.ManagementResponse {
	entry, fresh, err := l.begin(id, credential)
	if err != nil {
		return protocol.ManagementResponse{StatusCode: http.StatusConflict}
	}
	if !fresh {
		answer, ok := l.replay(entry)
		if !ok {
			return protocol.ManagementResponse{StatusCode: http.StatusConflict}
		}
		return answer
	}
	calls.Add(1)
	time.Sleep(hold)
	answer := jsonResponse(http.StatusOK, map[string]string{"outcome": "reset"})
	l.finish(entry, answer)
	return answer
}

// Eight copies of one press, at once: one spends, and all eight are told what
// that one spend did.
func TestLedgerConcurrentCopiesSpendOnceAndShareTheAnswer(t *testing.T) {
	l := newLedger()
	var calls atomic.Int32
	var wg sync.WaitGroup
	got := make([]protocol.ManagementResponse, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = ledgerSpend(l, pressID(1), "c1", &calls, 50*time.Millisecond)
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("one press spent %d times", calls.Load())
	}
	replayed := 0
	for _, answer := range got {
		if answer.StatusCode != http.StatusOK || string(answer.Body) != `{"outcome":"reset"}` {
			t.Fatalf("answer = %d %s", answer.StatusCode, answer.Body)
		}
		if answer.Headers.Get(replayedHeader) == "1" {
			replayed++
		}
	}
	if replayed != 7 {
		t.Fatalf("%d answers were marked as replays, want the 7 copies", replayed)
	}
}

// A press is one decision about one account. Its id cannot be spent again
// against another.
func TestLedgerBindsAPressToItsCredential(t *testing.T) {
	l := newLedger()
	var calls atomic.Int32
	ledgerSpend(l, pressID(1), "c1", &calls, 0)
	if _, _, err := l.begin(pressID(1), "c2"); !errors.Is(err, errPressMismatch) {
		t.Fatalf("err = %v, want errPressMismatch", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// Forgetting a press that is still in flight is exactly what would let its
// replay spend, so a ledger full of them refuses a new press instead.
func TestLedgerNeverEvictsAPressInFlight(t *testing.T) {
	l := newLedger()
	for i := range ledgerCapacity {
		if _, _, err := l.begin(pressID(i), "c"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := l.begin(pressID(ledgerCapacity), "c"); !errors.Is(err, errLedgerFull) {
		t.Fatalf("err = %v, want errLedgerFull with every entry in flight", err)
	}
	// Every one of them is still remembered.
	for i := range ledgerCapacity {
		if _, fresh, err := l.begin(pressID(i), "c"); err != nil || fresh {
			t.Fatalf("press %d was forgotten (fresh=%v, err=%v)", i, fresh, err)
		}
	}
}

// A finished press is remembered for its whole ten minutes even at capacity:
// forgetting it early would let a copy of it spend again. A full ledger
// refuses the new press instead, and makes room only as presses age out,
// oldest first, while an in-flight one never ages out at all.
func TestLedgerKeepsEveryFinishedPressForItsTenMinutesEvenWhenFull(t *testing.T) {
	clock := newFakeClock()
	l := newLedger()
	l.clock = clock.read
	entries := make([]*pressEntry, ledgerCapacity)
	for i := range ledgerCapacity {
		entry, _, err := l.begin(pressID(i), "c")
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = entry
	}
	// Press 0 stays in flight. The rest finish a second apart, so press 1 is
	// the first to reach its ten minutes.
	for i := 1; i < ledgerCapacity; i++ {
		clock.advance(time.Second)
		l.finish(entries[i], jsonResponse(http.StatusOK, map[string]int{"press": i}))
	}
	if _, _, err := l.begin(pressID(ledgerCapacity), "c"); !errors.Is(err, errLedgerFull) {
		t.Fatalf("err = %v, want errLedgerFull while every press is inside its ten minutes", err)
	}
	for i := range ledgerCapacity {
		if _, fresh, err := l.begin(pressID(i), "c"); err != nil || fresh {
			t.Fatalf("press %d was forgotten inside its ten minutes (fresh=%v, err=%v)", i, fresh, err)
		}
	}

	clock.advance(ledgerTTL - time.Duration(ledgerCapacity-2)*time.Second)
	if _, fresh, err := l.begin(pressID(ledgerCapacity), "c"); err != nil || !fresh {
		t.Fatalf("a new press was not admitted once one aged out: fresh=%v err=%v", fresh, err)
	}
	if _, ok := l.entries[pressID(0)]; !ok {
		t.Fatal("the in-flight press was forgotten")
	}
	if _, ok := l.entries[pressID(1)]; ok {
		t.Fatal("the press past its ten minutes was kept")
	}
	if _, ok := l.entries[pressID(2)]; !ok {
		t.Fatal("a press inside its ten minutes was forgotten")
	}
}

// Many callers pressing more distinct ids than the ledger holds, all at once:
// some presses are refused for want of room, but no id is ever spent twice
// inside its ten minutes.
func TestLedgerNeverSpendsAnIdTwiceUnderPressure(t *testing.T) {
	l := newLedger()
	l.clock = newFakeClock().read
	const ids = ledgerCapacity + 44
	var spends [ids]atomic.Int32
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 400 {
				n := (g*400 + i) % ids
				entry, fresh, err := l.begin(pressID(n), "c")
				if err != nil {
					continue
				}
				if !fresh {
					if answer, ok := l.replay(entry); ok && answer.Headers.Get(replayedHeader) != "1" {
						t.Error("a replay was not marked")
					}
					continue
				}
				spends[n].Add(1)
				l.finish(entry, jsonResponse(http.StatusOK, map[string]int{"press": n}))
			}
		}()
	}
	wg.Wait()
	for n := range ids {
		if got := spends[n].Load(); got > 1 {
			t.Fatalf("press %d spent %d times", n, got)
		}
	}
}

// The ten minutes run from the answer, not from the press: a slow provider
// does not shorten how long a press stays single-use.
func TestLedgerForgetsAPressTenMinutesAfterItsAnswer(t *testing.T) {
	clock := newFakeClock()
	l := newLedger()
	l.clock = clock.read
	entry, _, _ := l.begin(pressID(1), "c")
	clock.advance(5 * time.Minute)
	l.finish(entry, jsonResponse(http.StatusOK, map[string]string{"outcome": "reset"}))

	clock.advance(ledgerTTL - time.Second)
	if _, fresh, _ := l.begin(pressID(1), "c"); fresh {
		t.Fatal("the press was forgotten before ten minutes had passed since its answer")
	}
	clock.advance(time.Second)
	if _, fresh, _ := l.begin(pressID(1), "c"); !fresh {
		t.Fatal("the press was still remembered ten minutes after its answer")
	}
}

// Every copy gets its own answer. Marking one as a replay must not mark the
// stored answer, or another copy's.
func TestLedgerReplaysCopiesAndNeverTheStoredAnswer(t *testing.T) {
	l := newLedger()
	entry, _, _ := l.begin(pressID(1), "c")
	stored := jsonResponse(http.StatusOK, map[string]string{"outcome": "reset"})
	l.finish(entry, stored)
	first, ok := l.replay(entry)
	if !ok {
		t.Fatal("a finished press could not be replayed")
	}
	first.Headers.Set("X-Mutated", "yes")
	first.Body[0] = 'X'
	second, _ := l.replay(entry)
	if second.Headers.Get("X-Mutated") != "" || second.Body[0] != '{' {
		t.Fatalf("one replay changed another: %v %s", second.Headers, second.Body)
	}
	if stored.Headers.Get(replayedHeader) != "" {
		t.Fatal("the stored answer was marked as a replay")
	}
	if second.Headers.Get(replayedHeader) != "1" || second.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("replay headers = %v", second.Headers)
	}
}

// A copy that arrives while its press is still under way waits, but not past
// the wait: then it is told so, and spends nothing.
func TestLedgerReplayGivesUpAfterTheWait(t *testing.T) {
	l := newLedger()
	l.wait = 20 * time.Millisecond
	entry, _, _ := l.begin(pressID(1), "c")
	if _, ok := l.replay(entry); ok {
		t.Fatal("a press still in flight was replayed")
	}
	l.finish(entry, jsonResponse(http.StatusOK, map[string]string{"outcome": "reset"}))
	if _, ok := l.replay(entry); !ok {
		t.Fatal("a finished press could not be replayed")
	}
}
