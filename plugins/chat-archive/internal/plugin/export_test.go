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
