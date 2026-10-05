package redeem

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// fakeHost answers per URL and records everything, so a test can assert not
// only what came back but whether the spend request was made at all. For an
// irreversible action that second question is the one that matters.
type fakeHost struct {
	mu      sync.Mutex
	auth    string
	authErr error
	bodies  map[string]string
	status  map[string]int
	errs    map[string]error
	// script holds replies consumed in order, per URL, ahead of the static
	// answers above: how a test makes the first claim time out and the second
	// one land.
	script map[string][]reply
	// onRequest runs before each answer, outside the lock: how a test moves
	// the clock while a press is mid-flight.
	onRequest func(protocol.HostHTTPRequest)

	requests []protocol.HostHTTPRequest
	// budgets is the time each request's context had left when it arrived,
	// or -1 when it had no deadline.
	budgets []time.Duration
}

type reply struct {
	status int
	body   string
	err    error
}

func hostWith(bodies map[string]string) *fakeHost {
	return &fakeHost{bodies: bodies, status: map[string]int{}, errs: map[string]error{}, script: map[string][]reply{}}
}

func (h *fakeHost) GetAuth(context.Context, string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.authErr != nil {
		return nil, h.authErr
	}
	if h.auth == "" {
		return []byte(`{"access_token":"synthetic-token","account_id":"synthetic-account"}`), nil
	}
	return []byte(h.auth), nil
}

func (h *fakeHost) HTTPDo(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	hook := h.onRequest
	h.mu.Unlock()
	if hook != nil {
		hook(request)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, request)
	budget := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}
	h.budgets = append(h.budgets, budget)
	if queue := h.script[request.URL]; len(queue) > 0 {
		next := queue[0]
		h.script[request.URL] = queue[1:]
		if next.err != nil {
			return protocol.HostHTTPResponse{}, next.err
		}
		if next.status == 0 {
			next.status = 200
		}
		return protocol.HostHTTPResponse{StatusCode: next.status, Body: []byte(next.body)}, nil
	}
	if err := h.errs[request.URL]; err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	status := h.status[request.URL]
	if status == 0 {
		status = 200
	}
	return protocol.HostHTTPResponse{StatusCode: status, Body: []byte(h.bodies[request.URL])}, nil
}

func (h *fakeHost) queue(url string, replies ...reply) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.script[url] = append(h.script[url], replies...)
}

func (h *fakeHost) setAuth(doc string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.auth = doc
}

func (h *fakeHost) posted() bool { return h.postCount() > 0 }

func (h *fakeHost) postCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, request := range h.requests {
		if request.Method == "POST" {
			n++
		}
	}
	return n
}

// calls counts the requests made to one URL.
func (h *fakeHost) calls(url string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, request := range h.requests {
		if request.URL == url {
			n++
		}
	}
	return n
}

// postBodies decodes every spend request, oldest first.
func (h *fakeHost) postBodies(t *testing.T) []map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []map[string]string{}
	for _, request := range h.requests {
		if request.Method != "POST" {
			continue
		}
		var body map[string]string
		if json.Unmarshal(request.Body, &body) != nil {
			t.Fatalf("POST body is not JSON: %q", request.Body)
		}
		out = append(out, body)
	}
	return out
}

// postBody is the one spend request a test expects.
func (h *fakeHost) postBody(t *testing.T) map[string]string {
	t.Helper()
	bodies := h.postBodies(t)
	if len(bodies) == 0 {
		t.Fatal("no POST was made")
	}
	return bodies[0]
}

// stepClock is a clock a test moves by hand.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// redeemerOn is a redeemer on host whose retry window runs on clock.
func redeemerOn(host Host, clock *stepClock) *Redeemer {
	r := New(host)
	r.now = clock.Now
	return r
}

func newClock() *stepClock { return &stepClock{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)} }

// mustDecode reads a fixture the way a response is read, numbers and all.
func mustDecode(t *testing.T, text string) map[string]any {
	t.Helper()
	root, err := decode([]byte(text))
	if err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}
	return root
}
