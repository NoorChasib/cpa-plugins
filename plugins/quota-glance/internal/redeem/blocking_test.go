package redeem

import (
	"context"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// blockingHost holds every request open until its gate is closed, so a test can
// observe a redemption that is genuinely mid-flight rather than racing to guess
// when one is.
type blockingHost struct {
	fakeHost
	gate chan struct{}

	enteredMu sync.Mutex
	entered   int
}

func (h *blockingHost) HTTPDo(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.enteredMu.Lock()
	h.entered++
	h.enteredMu.Unlock()
	<-h.gate
	return h.fakeHost.HTTPDo(ctx, request)
}

func (h *blockingHost) count() int {
	h.enteredMu.Lock()
	defer h.enteredMu.Unlock()
	return h.entered
}

// waitUntilEntered blocks until the first request is inside the host, which is
// the moment the credential is claimed and the attempt is under way.
func (h *blockingHost) waitUntilEntered() { h.waitForRequests(1) }

func (h *blockingHost) waitForRequests(n int) {
	deadline := time.Now().Add(5 * time.Second)
	for h.count() < n {
		if time.Now().After(deadline) {
			panic("timed out waiting for the host to be reached")
		}
		time.Sleep(time.Millisecond)
	}
}
