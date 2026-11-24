package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
)

// TestCloseAfterWriter (review 2 of the round-4 fixes): a stop whose deadline passed while the capture writer was still
// inside an outbox write closes the outbox only once the writer returned, never while the writer owns it.
func TestCloseAfterWriter(t *testing.T) {
	calls := make(chan struct{}, 2)
	SetCloseOutbox(t, func(*outbox.Outbox, context.Context) error {
		calls <- struct{}{}
		return nil
	})
	done := make(chan struct{})
	go closeAfterWriter(nil, done)
	select {
	case <-calls:
		t.Fatal("the outbox was closed while the writer owned it")
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the outbox was not closed after the writer returned")
	}
}
