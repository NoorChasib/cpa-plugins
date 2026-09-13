// Package cache owns the single writer and the provider request schedule.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

type Account struct{ Provider, AuthIndex string }
type Observation struct {
	Quota               *client.Quota
	Percent             float64
	ResetAt, ObservedAt time.Time
	RequestSent         bool
	HTTPStatus          int
}
type Fetcher interface {
	List(context.Context) ([]Account, error)
	Fetch(context.Context, Account) (Observation, error)
}

type RateLimited struct{ RetryAfter time.Time }

func (RateLimited) Error() string { return "provider rate limited" }

type Options struct {
	Path              string
	Interval, Spacing time.Duration
}

type Cache struct {
	mu         sync.Mutex
	scheduleMu sync.Mutex
	schedule   Options
	opts       Options
	fetcher    Fetcher
	data       client.Snapshot
	lock       *os.File
	closed     bool
	activityMu sync.RWMutex
	activity   Activity
}

type Activity struct {
	LastScan time.Time `json:"last_scan"`
	Accounts int       `json:"accounts"`
	Error    string    `json:"error,omitempty"`
}

// Activity stays responsive while Step is inside a provider callback. The
// persisted snapshot separately exposes the in-flight attempt and schedule.
func (c *Cache) Activity() Activity {
	c.activityMu.RLock()
	defer c.activityMu.RUnlock()
	return c.activity
}

func Open(opts Options, fetcher Fetcher) (*Cache, error) {
	if opts.Path == "" || opts.Interval < time.Minute || opts.Spacing < time.Second || fetcher == nil {
		return nil, errors.New("invalid cache options")
	}
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, err
	}
	opts.Path = path
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("cache directory must be private (0700)")
	}
	lock, err := lockWriter(path + ".lock")
	if err != nil {
		return nil, err
	}
	c := &Cache{opts: opts, schedule: opts, fetcher: fetcher, lock: lock, data: client.Snapshot{
		Schema: 1, Entries: map[string]client.Entry{}, ProviderCooldown: map[string]time.Time{},
	}}
	if _, err = os.Lstat(path); err == nil {
		c.data, err = client.Load(path)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		c.Close()
		return nil, errors.New("existing cache cannot be read; refusing to reset cooldowns")
	}
	return c, nil
}

func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		unlockWriter(c.lock)
	}
}

// SetSchedule queues a validated schedule without waiting for provider I/O.
// Step applies it between requests while retaining ownership of the writer.
func (c *Cache) SetSchedule(interval, spacing time.Duration) {
	c.scheduleMu.Lock()
	c.schedule.Interval, c.schedule.Spacing = interval, spacing
	c.scheduleMu.Unlock()
}

// applySchedule runs under the writer lock. Successful credentials adopt the
// new interval; failed/pending attempts retain their existing backoff floor.
// Provider cooldowns and already-admitted global spacing are never shortened.
func (c *Cache) applySchedule(now time.Time) error {
	c.scheduleMu.Lock()
	opts := c.schedule
	c.scheduleMu.Unlock()
	if opts == c.opts {
		return nil
	}
	var lastAttempt time.Time
	for key, entry := range c.data.Entries {
		if entry.LastAttempt.After(lastAttempt) {
			lastAttempt = entry.LastAttempt
		}
		if entry.LastAttempt.IsZero() {
			continue
		}
		next := entry.LastAttempt.Add(opts.Interval)
		if entry.LastError == "" || next.After(entry.NextAttempt) {
			entry.NextAttempt = next
			c.data.Entries[key] = entry
		}
	}
	if next := lastAttempt.Add(opts.Spacing); !lastAttempt.IsZero() && next.After(c.data.NextRequest) {
		c.data.NextRequest = next
	}
	// Do not consider the change applied until its schedule is durable.
	if err := c.save(now); err != nil {
		return err
	}
	c.opts = opts
	return nil
}

// Step performs at most one provider request. Concurrent callers serialize at
// this interface. Schedule and cooldowns are persisted before provider calls so
// restarts cannot repeatedly bypass admission. Dashboard/consumer reads never
// call Step.
func (c *Cache) Step(ctx context.Context, now time.Time) (result error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		c.activityMu.Lock()
		defer c.activityMu.Unlock()
		c.activity.Error = ""
		if result != nil && ctx.Err() == nil {
			c.activity.Error = result.Error()
		}
	}()
	if c.closed {
		return errors.New("cache stopped")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.applySchedule(now); err != nil {
		return err
	}
	if now.Before(c.data.NextRequest) {
		return nil
	}
	accounts, err := c.fetcher.List(ctx)
	if err != nil {
		return errors.New("credential discovery failed")
	}
	active := map[string]Account{}
	for _, a := range accounts {
		if a.AuthIndex != "" && (a.Provider == "claude" || a.Provider == "codex" || a.Provider == "xai") {
			active[client.Key(a.Provider, a.AuthIndex)] = a
		}
	}
	dirty := c.data.WrittenAt.IsZero()
	c.activityMu.Lock()
	c.activity.LastScan, c.activity.Accounts = now, len(active)
	c.activityMu.Unlock()
	for key, a := range active {
		if _, exists := c.data.Entries[key]; !exists {
			c.data.Entries[key] = client.Entry{Provider: a.Provider, AuthIndex: a.AuthIndex}
			dirty = true
		}
	}
	for key := range c.data.Entries {
		if _, ok := active[key]; !ok {
			delete(c.data.Entries, key)
			dirty = true
		}
	}
	keys := make([]string, 0, len(active))
	for key := range active {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := c.data.Entries[keys[i]].NextAttempt, c.data.Entries[keys[j]].NextAttempt
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.Before(b)
	})
	for _, key := range keys {
		a := active[key]
		entry := c.data.Entries[key]
		if now.Before(entry.NextAttempt) || now.Before(c.data.ProviderCooldown[a.Provider]) {
			continue
		}
		entry.Provider, entry.AuthIndex = a.Provider, a.AuthIndex
		entry.LastAttempt, entry.NextAttempt = now, now.Add(c.opts.Interval)
		entry.LastError = "refresh pending"
		c.data.Entries[key] = entry
		c.data.NextRequest = now.Add(c.opts.Spacing)
		if err := c.save(now); err != nil {
			return err
		}
		started := time.Now()
		observation, fetchErr := c.fetcher.Fetch(ctx, a)
		elapsed := time.Since(started)
		poll := client.Poll{Provider: a.Provider, AuthIndex: a.AuthIndex, StartedAt: now,
			FinishedAt: now.Add(elapsed), DurationMS: elapsed.Milliseconds(), RequestSent: observation.RequestSent,
			HTTPStatus: observation.HTTPStatus, Outcome: "success"}
		c.data.Totals.Attempts++
		if observation.RequestSent {
			c.data.Totals.Requests++
		}
		if fetchErr != nil {
			c.data.Totals.Failures++
			poll.Outcome = "failed"
			entry.Failures++
			entry.LastError = "quota fetch failed"
			// Start at the normal interval and exponentially back off to six hours.
			delay := c.opts.Interval
			for i := 1; i < entry.Failures && delay < 6*time.Hour; i++ {
				delay *= 2
			}
			if delay > 6*time.Hour {
				delay = 6 * time.Hour
			}
			entry.NextAttempt = now.Add(delay)
			var limited RateLimited
			if errors.As(fetchErr, &limited) {
				c.data.Totals.RateLimits++
				poll.Outcome = "rate_limited"
				entry.LastError = "provider rate limited"
				if limited.RetryAfter.After(entry.NextAttempt) {
					entry.NextAttempt = limited.RetryAfter
				}
				c.data.ProviderCooldown[a.Provider] = entry.NextAttempt
			}
		} else {
			c.data.Totals.Successes++
			entry.Percent, entry.ResetAt, entry.ObservedAt = observation.Percent, observation.ResetAt, observation.ObservedAt
			entry.Quota = observation.Quota
			entry.Failures, entry.LastError = 0, ""
		}
		poll.Error = entry.LastError
		c.data.History = append(c.data.History, poll)
		if len(c.data.History) > 100 {
			c.data.History = c.data.History[len(c.data.History)-100:]
		}
		c.data.Entries[key] = entry
		return c.save(now)
	}
	if dirty {
		return c.save(now)
	}
	return nil
}

func (c *Cache) save(now time.Time) error {
	c.data.WrittenAt = now
	raw, err := json.Marshal(c.data)
	if err != nil || len(raw) > client.MaxBytes {
		return errors.New("cache snapshot cannot be encoded")
	}
	file, err := os.CreateTemp(filepath.Dir(c.opts.Path), ".snapshot-*")
	if err != nil {
		return errors.New("cache snapshot cannot be written")
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, c.opts.Path)
	}
	if err != nil {
		return errors.New("cache snapshot cannot be committed")
	}
	dir, err := os.Open(filepath.Dir(c.opts.Path))
	if err == nil {
		err = dir.Sync()
		dir.Close()
	}
	if err != nil {
		return fmt.Errorf("cache directory sync failed")
	}
	return nil
}
