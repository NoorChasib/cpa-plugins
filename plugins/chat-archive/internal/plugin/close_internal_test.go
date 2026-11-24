package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
)

// TestCloseAfterWriter (reviews 2 and 3 of the round-4 fixes): a stop whose deadline passed while the capture writer was
// still inside an outbox write saves the dropped counters and closes the outbox only once the writer returned, never
// while the writer owns it, and saves before it closes, so the drops the writer counts on its way out are on disk.
func TestCloseAfterWriter(t *testing.T) {
	calls := make(chan string, 4)
	SetCloseOutbox(t, func(*outbox.Outbox, context.Context) error {
		calls <- "close"
		return nil
	})
	done := make(chan struct{})
	go closeAfterWriter(nil, done, func(context.Context) error {
		calls <- "save"
		return nil
	})
	select {
	case c := <-calls:
		t.Fatalf("%s ran while the writer owned the outbox", c)
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	for _, want := range []string{"save", "close"} {
		select {
		case c := <-calls:
			if c != want {
				t.Fatalf("got %s, want %s (save the drops, then close)", c, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s after the writer returned", want)
		}
	}
}
