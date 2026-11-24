package plugin

import (
	"context"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
)

// SetCloseOutbox replaces the stop's outbox close for the test.
func SetCloseOutbox(t *testing.T, f func(*outbox.Outbox, context.Context) error) {
	old := closeOutbox
	closeOutbox = f
	t.Cleanup(func() { closeOutbox = old })
}

// SetOutboxWrite makes the capture writer of every runtime started during the test call f's write for its outbox.
func SetOutboxWrite(t *testing.T, f func(ob *outbox.Outbox) func(context.Context, []outbox.NewEnvelope) (outbox.WriteResult, error)) {
	old := outboxWrite
	outboxWrite = f
	t.Cleanup(func() { outboxWrite = old })
}
