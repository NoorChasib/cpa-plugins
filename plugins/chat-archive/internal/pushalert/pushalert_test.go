package pushalert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEvaluateRateLimitsAndSendsCountsOnly(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	Endpoint = srv.URL
	a, ok := New("app-token-synthetic", "user-key-synthetic", 6)
	if !ok {
		t.Fatal("not configured")
	}
	if _, ok := New("", "x", 6); ok {
		t.Fatal("configured without a token")
	}
	now := time.Now()
	s := Snapshot{OldestPendingAgeS: 7 * 3600, Dropped: map[string]uint64{"stopped": 1}, Pressure: 2, Faults: []string{"auth_failed", "other"}}
	due := a.Evaluate(s, now)
	if _, ok := due["dropped"]; ok || len(due) != 3 {
		t.Fatalf("first evaluation = %v", due) // no dropped delta on the first snapshot
	}
	a.Send(context.Background(), due)
	s.Dropped["stopped"] = 4
	due = a.Evaluate(s, now.Add(time.Minute))
	if len(due) != 1 || !strings.Contains(due["dropped"], "stopped +3") {
		t.Fatalf("second evaluation = %v", due)
	}
	if again := a.Evaluate(s, now.Add(2*time.Minute)); len(again) != 0 {
		t.Fatalf("rate limit broken: %v", again)
	}
	if later := a.Evaluate(s, now.Add(2*time.Hour)); len(later) != 3 {
		t.Fatalf("after an hour = %v", later)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 || !strings.Contains(bodies[0], "chat-archive") {
		t.Fatalf("sent = %v", bodies)
	}
}
