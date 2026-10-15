package warmup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Manager owns one bounded runner and a write-ahead attempt journal. A lost
// response never causes a restart or repeated button press to send a duplicate.
type Manager struct {
	mu        sync.Mutex
	opts      Options
	path      string
	runner    Runner
	roster    func(context.Context) ([]Identity, error)
	refresh   func(string, time.Time)
	accounts  map[string]client.WarmupAccount
	wake      chan struct{}
	manual    bool
	lastStart time.Time
	cancel    context.CancelFunc
	journalOK bool
	saved     []byte
	lastSaved time.Time
}

func New(path string, opts Options, runner Runner, roster func(context.Context) ([]Identity, error), refresh func(string, time.Time)) *Manager {
	m := &Manager{path: path, opts: opts, runner: runner, roster: roster, refresh: refresh, accounts: map[string]client.WarmupAccount{}, wake: make(chan struct{}, 1), journalOK: true}
	if state, err := client.LoadWarmup(path); err == nil {
		for _, account := range state.Accounts {
			if authIndex.MatchString(account.AuthIndex) {
				m.accounts[account.AuthIndex] = account
				if account.LastAttempt.After(m.lastStart) {
					m.lastStart = account.LastAttempt
				}
			}
		}
	} else if _, err := os.Lstat(client.WarmupPath(path)); !errors.Is(err, os.ErrNotExist) {
		m.journalOK = false
	}
	return m
}

func (m *Manager) Configure(opts Options) {
	m.mu.Lock()
	m.opts = opts
	if !opts.Enabled && m.cancel != nil {
		m.cancel()
	}
	if !opts.Enabled && len(m.accounts) > 0 {
		m.saveLocked(time.Now().UTC())
	}
	m.mu.Unlock()
	m.signal()
}

// Disable stops inference when CPA rejects the plugin configuration. The
// durable attempt guard remains even if cancellation leaves an unknown result.
func (m *Manager) Disable() {
	m.mu.Lock()
	opts := m.opts
	m.mu.Unlock()
	opts.Enabled = false
	m.Configure(opts)
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// WarmAll deliberately ignores working hours, but never active windows,
// exhaustion, provider cooldowns or the durable duplicate-attempt guard.
func (m *Manager) WarmAll() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.opts.Enabled || !m.journalOK {
		return false
	}
	m.manual = true
	m.signal()
	return true
}

func (m *Manager) statusLocked(now time.Time) client.WarmupState {
	state := client.WarmupState{Schema: 1, Enabled: m.opts.Enabled, Model: m.opts.Model, Effort: m.opts.Effort, Hours: m.opts.Hours, Timezone: m.opts.Timezone, UpdatedAt: now, Accounts: []client.WarmupAccount{}}
	if !m.journalOK {
		state.LastError = "warmup_journal_unavailable"
	}
	for _, account := range m.accounts {
		state.Accounts = append(state.Accounts, account)
	}
	slices.SortFunc(state.Accounts, func(a, b client.WarmupAccount) int {
		if a.AuthIndex < b.AuthIndex {
			return -1
		}
		if a.AuthIndex > b.AuthIndex {
			return 1
		}
		return 0
	})
	return state
}

func (m *Manager) Status(now time.Time) client.WarmupState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(now)
}

// saveLocked must succeed before any inference is attempted. A malformed or
// unwritable journal disables inference rather than discarding its history.
func (m *Manager) saveLocked(now time.Time) bool {
	if !m.journalOK {
		return false
	}
	path := client.WarmupPath(m.path)
	state := m.statusLocked(now)
	signature := state
	signature.UpdatedAt = time.Time{}
	stable, err := json.Marshal(signature)
	if err == nil && bytes.Equal(stable, m.saved) && now.Sub(m.lastSaved) < time.Minute {
		return true
	}
	raw, err := json.Marshal(state)
	if err == nil && (len(state.Accounts) > 256 || len(raw) > 256<<10) {
		m.journalOK = false
		return false
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0700)
	}
	var file *os.File
	if err == nil {
		file, err = os.CreateTemp(filepath.Dir(path), ".warmup-*")
	}
	if err == nil {
		defer os.Remove(file.Name())
		err = file.Chmod(0600)
		if err == nil {
			_, err = file.Write(raw)
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(file.Name(), path)
		}
		if err == nil {
			var directory *os.File
			directory, err = os.Open(filepath.Dir(path))
			if err == nil {
				err = directory.Sync()
				_ = directory.Close()
			}
		}
	}
	if err != nil {
		m.journalOK = false
		return false
	}
	m.saved, m.lastSaved = stable, now
	return true
}

func (m *Manager) Run(ctx context.Context) {
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		wasEnabled := m.opts.Enabled
		m.opts.Enabled = false
		if wasEnabled || len(m.accounts) > 0 {
			m.saveLocked(time.Now().UTC())
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		m.Step(ctx, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}

func window(entry client.Entry, key string) (client.EntryWindow, bool) {
	for _, w := range entry.Windows {
		if w.Key == key {
			return w, true
		}
	}
	return client.EntryWindow{}, false
}

func usableObservation(w client.EntryWindow, now time.Time) bool {
	return !w.ObservedAt.IsZero() && !w.ObservedAt.After(now) && now.Sub(w.ObservedAt) <= 30*time.Minute
}

func eligibility(entry client.Entry, now time.Time) (state string, reset time.Time) {
	if entry.LastError != "" {
		return "waiting_for_quota", time.Time{}
	}
	session, hasSession := window(entry, client.WindowSession)
	weekly, hasWeekly := window(entry, client.WindowWeekly)
	if !hasSession || !hasWeekly || !usableObservation(session, now) || !usableObservation(weekly, now) {
		return "waiting_for_quota", time.Time{}
	}
	if !weekly.ResetAt.IsZero() && !weekly.ResetAt.After(now) && weekly.ObservedAt.Before(weekly.ResetAt) {
		return "waiting_for_quota", time.Time{}
	}
	if weekly.UsedPercent >= 99.5 {
		return "weekly_spent", weekly.ResetAt
	}
	if session.ResetAt.After(now) {
		return "active", session.ResetAt
	}
	if !session.ResetAt.IsZero() && session.ObservedAt.Before(session.ResetAt) {
		return "waiting_for_quota", time.Time{}
	}
	// Missing reset plus nonzero usage is ambiguous, not proof of an idle window.
	if session.ResetAt.IsZero() && session.UsedPercent != 0 {
		return "waiting_for_quota", time.Time{}
	}
	return "idle", time.Time{}
}

func turnoverNeedsRefresh(entry client.Entry, now time.Time) bool {
	if entry.LastError != "" {
		return false
	}
	for _, key := range []string{client.WindowSession, client.WindowWeekly} {
		if w, ok := window(entry, key); ok && !w.ResetAt.IsZero() && !w.ResetAt.After(now) && w.ObservedAt.Before(w.ResetAt) {
			return true
		}
	}
	return false
}

// Step is also the deterministic scheduling seam for tests. Only Run calls it
// in production. Its state lock is never held over host calls.
func (m *Manager) Step(ctx context.Context, now time.Time) {
	m.mu.Lock()
	opts, manual := m.opts, m.manual
	if !opts.Enabled || !m.journalOK {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	identities, err := m.roster(ctx)
	if err != nil || ctx.Err() != nil {
		return
	}
	snapshot, err := client.Load(m.path)
	if err != nil {
		return
	}
	slices.SortFunc(identities, func(a, b Identity) int {
		if a.AuthIndex < b.AuthIndex {
			return -1
		}
		if a.AuthIndex > b.AuthIndex {
			return 1
		}
		return 0
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reconfiguration or a stop can happen while the roster is read.
	opts = m.opts
	manual = m.manual
	if !opts.Enabled || ctx.Err() != nil {
		return
	}
	seen := map[string]bool{}
	var candidate *Identity
	due := false
	var refreshIDs []string
	for _, identity := range identities {
		if !authIndex.MatchString(identity.AuthIndex) || seen[identity.AuthIndex] {
			continue
		}
		seen[identity.AuthIndex] = true
		account := m.accounts[identity.AuthIndex]
		account.AuthIndex = identity.AuthIndex
		if strings.Contains(identity.Email, "@") {
			hash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(identity.Email))))
			account.IdentityHash = hex.EncodeToString(hash[:16])
			for _, sibling := range m.accounts {
				if sibling.AuthIndex != account.AuthIndex && sibling.IdentityHash == account.IdentityHash && sibling.LastAttempt.After(account.LastAttempt) && sibling.NextAttempt.After(account.NextAttempt) {
					account.NextAttempt = sibling.NextAttempt
				}
			}
		}
		entry, found := snapshot.Entries[client.Key("claude", identity.AuthIndex)]
		state, reset := "waiting_for_quota", time.Time{}
		if found {
			state, reset = eligibility(entry, now)
		}
		if identity.Disabled {
			state = "disabled"
		}
		if state == "waiting_for_quota" && turnoverNeedsRefresh(entry, now) {
			refreshIDs = append(refreshIDs, identity.AuthIndex)
		}
		account.State, account.ResetAt = state, reset
		if state == "active" && !account.LastAttempt.IsZero() && entry.ObservedAt.After(account.LastAttempt) {
			account.LastSuccess, account.NextAttempt, account.Error = entry.ObservedAt, reset, ""
		}
		if state == "idle" {
			switch {
			case snapshot.ProviderCooldown["claude"].After(now):
				account.State = "provider_cooldown"
			case account.NextAttempt.After(now):
				account.State = "waiting"
				if account.Error == "" {
					account.State = "awaiting_confirmation"
				}
			// CPA answered the last warm-up, yet no observation since showed an
			// active session. Anthropic may have billed it as extra usage
			// instead, and every automatic retry would do the same, so only
			// Warm all now tries this account again.
			case !manual && (account.Error == "" || account.Error == "unconfirmed") && !account.LastAttempt.IsZero() && account.LastSuccess.Before(account.LastAttempt):
				account.State, account.Error = "failed", "unconfirmed"
			case !manual && !opts.withinHours(now):
				account.State = "outside_hours"
			default:
				due = true
				if candidate == nil && !now.Before(m.lastStart.Add(opts.Spacing)) {
					copy := identity
					copy.Plan = entry.Plan
					candidate = &copy
				}
			}
		}
		m.accounts[identity.AuthIndex] = account
	}
	for id := range m.accounts {
		if !seen[id] {
			if !m.accounts[id].NextAttempt.After(now) {
				delete(m.accounts, id)
			} else {
				account := m.accounts[id]
				account.State = "not_listed"
				m.accounts[id] = account
			}
		}
	}
	if candidate == nil {
		if !due && len(refreshIDs) == 0 {
			m.manual = false
		}
		m.saveLocked(now)
		if m.refresh != nil {
			m.mu.Unlock()
			for _, id := range refreshIDs {
				m.refresh(id, now)
			}
			m.mu.Lock()
		}
		return
	}
	account := m.accounts[candidate.AuthIndex]
	account.State, account.Error, account.LastAttempt, account.NextAttempt = "warming", "", now, now.Add(5*time.Hour)
	m.accounts[candidate.AuthIndex], m.lastStart = account, now
	if !m.saveLocked(now) {
		return
	}
	// This bounds the wait, not the request: CPA cannot stop a model request
	// once it is sent, and the runner reports outcome_unknown when it stops
	// waiting.
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	m.cancel = cancel
	m.mu.Unlock()
	err = m.runner.Warm(runCtx, opts, *candidate)
	cancel()
	m.mu.Lock()
	m.cancel = nil
	account = m.accounts[candidate.AuthIndex]
	account.State = "awaiting_confirmation"
	if err != nil {
		account.State, account.Error = "failed", errorCode(err)
		// These failures prove inference was never attempted. They may recover
		// after setup; ambiguous inference failures retain the five-hour guard.
		switch account.Error {
		case "identity_unknown", "subscription_login_required", "account_unavailable", "host_unsupported", "runner_unavailable", "runner_busy":
			account.NextAttempt = now.Add(15 * time.Minute)
		}
	}
	m.accounts[candidate.AuthIndex] = account
	m.saveLocked(time.Now().UTC())
	if m.refresh != nil {
		m.mu.Unlock()
		m.refresh(candidate.AuthIndex, now)
		m.mu.Lock()
	}
}

func errorCode(err error) string {
	switch err.Error() {
	case "identity_unknown", "subscription_login_required", "account_unavailable", "host_unsupported", "runner_unavailable", "runner_busy", "outcome_unknown":
		return err.Error()
	default:
		return "request_failed"
	}
}
