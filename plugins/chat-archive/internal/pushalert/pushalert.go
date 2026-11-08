// Package pushalert sends the plugin-side Pushover alerts: oldest pending envelope
// older than alert-backlog-hours, any dropped.* delta, outbox pressure ≥ 2, and the faults
// auth_failed, replay_gap and write_unavailable. At most one message per kind per hour. Messages
// carry counts and fault names only, never content, URLs or identifiers.
package pushalert

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Endpoint is Pushover's message API.
var Endpoint = "https://api.pushover.net/1/messages.json"

// Interval between alert kinds of the same name.
var Interval = time.Hour

// Snapshot is what the loop evaluates.
type Snapshot struct {
	OldestPendingAgeS int64
	Dropped           map[string]uint64
	Pressure          int
	Faults            []string
}

// Alerter evaluates snapshots and sends messages.
type Alerter struct {
	app, user   string
	backlog     time.Duration
	hc          *http.Client
	mu          sync.Mutex
	last        map[string]time.Time
	prevDropped map[string]uint64
	Sent        []string // kinds sent (tests)
}

// New returns an alerter; ok is false when Pushover is not configured.
func New(appToken, userKey string, backlogHours int) (*Alerter, bool) {
	if appToken == "" || userKey == "" {
		return nil, false
	}
	return &Alerter{app: appToken, user: userKey, backlog: time.Duration(backlogHours) * time.Hour,
		hc:   &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		last: map[string]time.Time{}}, true
}

// Evaluate returns the alert kinds and messages due for s, updating the rate-limit state.
func (a *Alerter) Evaluate(s Snapshot, now time.Time) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	due := map[string]string{}
	if a.backlog > 0 && time.Duration(s.OldestPendingAgeS)*time.Second > a.backlog {
		due["backlog"] = fmt.Sprintf("chat-archive: oldest pending envelope is %d h old", s.OldestPendingAgeS/3600)
	}
	if a.prevDropped != nil {
		var names []string
		for k, v := range s.Dropped {
			if v > a.prevDropped[k] {
				names = append(names, fmt.Sprintf("%s +%d", k, v-a.prevDropped[k]))
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			due["dropped"] = "chat-archive dropped: " + strings.Join(names, ", ")
		}
	}
	a.prevDropped = map[string]uint64{}
	for k, v := range s.Dropped {
		a.prevDropped[k] = v
	}
	if s.Pressure >= 2 {
		due["pressure"] = fmt.Sprintf("chat-archive: outbox pressure level %d", s.Pressure)
	}
	for _, f := range s.Faults {
		switch f {
		case "auth_failed", "replay_gap", "write_unavailable":
			due[f] = "chat-archive fault: " + f
		}
	}
	for k := range due {
		if t, ok := a.last[k]; ok && now.Sub(t) < Interval {
			delete(due, k)
			continue
		}
		a.last[k] = now
	}
	return due
}

// Send posts each due message once (no retry loop).
func (a *Alerter) Send(ctx context.Context, due map[string]string) {
	kinds := make([]string, 0, len(due))
	for k := range due {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		form := url.Values{"token": {a.app}, "user": {a.user}, "title": {"chat-archive"}, "message": {due[k]}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := a.hc.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		a.mu.Lock()
		a.Sent = append(a.Sent, k)
		a.mu.Unlock()
	}
}
