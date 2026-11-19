package monitor

import (
	"context"
	"errors"
	"fmt"
	quotaclient "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/quota"
	"github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover/internal/state"
)

// QuotaHost is the optional host surface required for weekly-quota polling.
// It is a separate interface so hosts (and test fakes) that only support
// health monitoring keep working; quota alerts are then reported as
// unavailable rather than failing the plugin.
type QuotaHost interface {
	// GetAuth returns the raw physical credential JSON. The document contains
	// OAuth tokens and is passed straight to the quota package, which decodes
	// only the fields needed for one usage request.
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
}

const (
	quotaWarningKind   = "quota_warning"
	quotaExhaustedKind = "quota_exhausted"
	quotaHostWarning   = "quota-alerts is enabled but the host does not expose host.auth.get/host.http.do; weekly quota polling is disabled"

	// quotaCacheMaxAge is the oldest quota-cache observation an alert may use.
	quotaCacheMaxAge = 30 * time.Minute
	// familyRetention bounds how long a family that a later read no longer
	// reports keeps its latches when its reset instant is unknown. A family
	// with a known reset keeps them until that reset passes.
	familyRetention = 7 * 24 * time.Hour
)

// quotaFamily is one Antigravity quota family that has its own weekly window.
// quota-cache publishes the default Gemini family's weekly window as
// WindowWeekly and every other family's as WindowModelWeekly with the token in
// EntryWindow.Model. Token is also the key in state.QuotaState.Families.
type quotaFamily struct {
	Token string
	Name  string
}

// familyGemini is the default Gemini family. quota-cache gives it no Model
// token because it is the account's regular weekly window.
const familyGemini = "gemini"

// antigravityFamilies is the closed set of families alerted on, in display
// order. A model_weekly window with any other Model token is ignored.
var antigravityFamilies = []quotaFamily{
	{Token: familyGemini, Name: "Gemini"},
	{Token: quotaclient.AntigravityModelShared, Name: "Claude & GPT"},
	{Token: quotaclient.AntigravityModelArgon, Name: "Gemini Argon"},
	{Token: quotaclient.AntigravityModelCarbon, Name: "Gemini Carbon"},
	{Token: quotaclient.AntigravityModelHydrogen, Name: "Gemini Hydrogen"},
}

// familyName returns the display name of a family token, and its position in
// display order.
func familyName(token string) (string, int, bool) {
	for rank, family := range antigravityFamilies {
		if family.Token == token {
			return family.Name, rank, true
		}
	}
	return "", len(antigravityFamilies), false
}

// perFamilyQuota reports whether provider's weekly usage is alerted per quota
// family rather than as one account-wide window.
func perFamilyQuota(provider string) bool {
	return provider == quotaclient.ProviderAntigravity
}

// familyObservation is one family's weekly window as quota-cache observed it.
type familyObservation struct {
	Family     string
	Percent    float64
	ResetAt    time.Time
	ObservedAt time.Time
}

// QuotaStatus is the account-level quota view in status JSON.
type QuotaStatus struct {
	Percent         *float64  `json:"percent,omitempty"`
	ResetAt         time.Time `json:"reset_at,omitempty"`
	ObservedAt      time.Time `json:"observed_at,omitempty"`
	LastPollAt      time.Time `json:"last_poll_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	WarningSentAt   time.Time `json:"warning_sent_at,omitempty"`
	ExhaustedSentAt time.Time `json:"exhausted_sent_at,omitempty"`
	// Families lists the per-family weekly windows of an Antigravity account,
	// in display order. The fields above stay empty for such an account
	// except LastPollAt and LastError, which cover the whole read.
	Families []QuotaFamilyStatus `json:"families,omitempty"`
}

// QuotaFamilyStatus is one quota family's weekly window in status JSON.
type QuotaFamilyStatus struct {
	Family          string    `json:"family"`
	Name            string    `json:"name"`
	Percent         *float64  `json:"percent,omitempty"`
	ResetAt         time.Time `json:"reset_at,omitempty"`
	ObservedAt      time.Time `json:"observed_at,omitempty"`
	WarningSentAt   time.Time `json:"warning_sent_at,omitempty"`
	ExhaustedSentAt time.Time `json:"exhausted_sent_at,omitempty"`
}

func quotaStatusOf(account *state.Account) *QuotaStatus {
	q := account.Quota
	if q.ObservedAt.IsZero() && q.LastPollAt.IsZero() && len(q.Families) == 0 {
		return nil
	}
	status := &QuotaStatus{
		ResetAt:         q.ResetAt,
		ObservedAt:      q.ObservedAt,
		LastPollAt:      q.LastPollAt,
		LastError:       q.LastError,
		WarningSentAt:   q.WarningSentAt,
		ExhaustedSentAt: q.ExhaustedSentAt,
	}
	if !q.ObservedAt.IsZero() {
		percent := q.Percent
		status.Percent = &percent
	}
	for token, window := range q.Families {
		name, _, ok := familyName(token)
		if !ok {
			continue
		}
		family := QuotaFamilyStatus{
			Family:          token,
			Name:            name,
			ResetAt:         window.ResetAt,
			ObservedAt:      window.ObservedAt,
			WarningSentAt:   window.WarningSentAt,
			ExhaustedSentAt: window.ExhaustedSentAt,
		}
		if !window.ObservedAt.IsZero() {
			percent := window.Percent
			family.Percent = &percent
		}
		status.Families = append(status.Families, family)
	}
	sort.Slice(status.Families, func(i, j int) bool {
		_, left, _ := familyName(status.Families[i].Family)
		_, right, _ := familyName(status.Families[j].Family)
		return left < right
	})
	return status
}

// quotaLoop polls provider usage endpoints on the configured interval. It
// starts after the baseline health reconciliation so every account row
// already exists, then wakes on the ticker or on an explicit request.
func (m *Monitor) quotaLoop() {
	defer m.loopWG.Done()
	select {
	case <-m.ctx.Done():
		return
	case <-m.baselined:
	}
	// Publish the schedule before the first poll so a snapshot taken during
	// the startup read already shows when the next one is due.
	m.setNextQuotaPoll(m.now().Add(m.cfg.QuotaPollInterval))
	_ = m.PollQuota(m.ctx, "startup")
	ticker := time.NewTicker(m.cfg.QuotaPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			_ = m.PollQuota(m.ctx, "periodic")
			m.setNextQuotaPoll(m.now().Add(m.cfg.QuotaPollInterval))
		case <-m.quotaSignals:
			_ = m.PollQuota(m.ctx, "management")
		}
	}
}

// RequestQuotaPoll asks the quota loop to poll soon without blocking the
// caller. It is a no-op when quota alerts are disabled.
func (m *Monitor) RequestQuotaPoll() {
	if m.quotaHost == nil || !m.cfg.QuotaAlerts {
		return
	}
	select {
	case m.quotaSignals <- struct{}{}:
	default:
	}
}

func (m *Monitor) setNextQuotaPoll(next time.Time) {
	m.stateMu.Lock()
	if !m.retiring.Load() && !m.retired {
		m.status.NextQuotaPoll = next.UTC()
	}
	m.stateMu.Unlock()
}

// PollQuota reads every supported account's weekly window once and applies
// threshold latches. Provider errors are recorded per account and never abort
// the pass; a roster failure is recorded on the status snapshot.
func (m *Monitor) PollQuota(ctx context.Context, trigger string) error {
	if m.quotaHost == nil || !m.cfg.QuotaAlerts {
		return nil
	}
	if !m.beginOperation() {
		return ErrStopping
	}
	defer m.operations.Done()
	if m.isRetired() {
		return ErrStopping
	}
	now := m.now().UTC()
	roster, err := m.host.ListAuth(ctx)
	if err != nil {
		if m.isRetired() {
			return ErrStopping
		}
		m.recordQuotaPollFailure(now, "host.auth.list failed; previous quota state was retained")
		return err
	}
	candidates := health.Discover(roster, m.cfg)

	m.stateMu.RLock()
	loaded := m.stateLoaded
	targets := make([]protocol.HostAuthFileEntry, 0, len(candidates))
	for _, candidate := range candidates {
		if !m.quotaTracked(candidate.Provider) {
			continue
		}
		if _, ok := m.data.Accounts[health.AccountKey(candidate.Provider, candidate.AuthIndex)]; ok {
			targets = append(targets, candidate)
		}
	}
	m.stateMu.RUnlock()
	if !loaded {
		return nil
	}

	type result struct {
		entry       protocol.HostAuthFileEntry
		observation quota.Observation
		families    []familyObservation
		err         error
	}
	results := make(chan result, len(targets))
	semaphore := make(chan struct{}, m.cfg.MaxConcurrentChecks)
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- result{entry: target, err: ctx.Err()}
				return
			}
			defer func() { <-semaphore }()
			if perFamilyQuota(target.Provider) {
				families, readErr := m.readQuotaFamilies(target, now)
				results <- result{entry: target, families: families, err: readErr}
				return
			}
			observation, fetchErr := m.fetchQuota(ctx, target, now)
			results <- result{entry: target, observation: observation, err: fetchErr}
		}()
	}
	wg.Wait()
	close(results)
	if m.isRetired() {
		return ErrStopping
	}
	items := make([]result, 0, len(targets))
	for item := range results {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return health.AccountKey(items[i].entry.Provider, items[i].entry.AuthIndex) < health.AccountKey(items[j].entry.Provider, items[j].entry.AuthIndex)
	})

	m.stateMu.Lock()
	if m.retiring.Load() || m.retired {
		m.stateMu.Unlock()
		return ErrStopping
	}
	failed := 0
	for _, item := range items {
		account := m.data.Accounts[health.AccountKey(item.entry.Provider, item.entry.AuthIndex)]
		if account == nil {
			continue
		}
		account.Quota.LastPollAt = now
		if item.err != nil {
			failed++
			account.Quota.LastError = sanitizeQuotaError(item.err)
			continue
		}
		account.Quota.LastError = ""
		if perFamilyQuota(item.entry.Provider) {
			m.applyQuotaFamiliesLocked(account, item.families, now)
			continue
		}
		m.applyQuotaLocked(account, item.observation, now)
	}
	persistErr := m.persistLocked()
	m.status.LastQuotaPoll = now
	m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	if failed > 0 {
		m.status.LastQuotaPollError = fmt.Sprintf("%d of %d usage reads failed; see account rows", failed, len(items))
	} else {
		m.status.LastQuotaPollError = ""
	}
	m.stateMu.Unlock()
	if persistErr != nil {
		m.host.Log(ctx, "warn", "account-health quota state persistence failed", map[string]any{"trigger": safeField(trigger)})
	}
	if failed > 0 {
		m.host.Log(ctx, "warn", "one or more weekly quota reads failed", map[string]any{"trigger": safeField(trigger), "failed": failed})
	}
	return persistErr
}

// quotaTracked reports whether provider's weekly usage is read in the
// configured mode. Antigravity is read only from quota-cache: standalone mode
// never calls Google, so there its accounts keep credential-health monitoring
// but are left out of quota polling without an error.
func (m *Monitor) quotaTracked(provider string) bool {
	if perFamilyQuota(provider) {
		return m.cfg.QuotaCachePath != ""
	}
	return quota.Supported(provider)
}

// readQuotaFamilies reads an Antigravity account's per-family weekly windows
// from quota-cache. Each window is judged fresh on its own terms by
// ReadFreshWindows, so an account whose summary reports no default Gemini
// weekly window, which ReadFresh refuses, still has its other families read.
// Session windows, raw windows and unknown families are ignored; a family
// reported twice keeps its first reading, as quota-cache does.
func (m *Monitor) readQuotaFamilies(entry protocol.HostAuthFileEntry, now time.Time) ([]familyObservation, error) {
	windows, err := quotaclient.ReadFreshWindows(m.cfg.QuotaCachePath, entry.Provider, entry.AuthIndex, now, quotaCacheMaxAge)
	if err != nil {
		return nil, err
	}
	families := make([]familyObservation, 0, len(antigravityFamilies))
	seen := make(map[string]struct{}, len(antigravityFamilies))
	for _, window := range windows {
		var token string
		switch window.Key {
		case quotaclient.WindowWeekly:
			token = familyGemini
		case quotaclient.WindowModelWeekly:
			token = window.Model
			if token == familyGemini {
				continue
			}
		default:
			continue
		}
		if _, _, ok := familyName(token); !ok {
			continue
		}
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}
		families = append(families, familyObservation{Family: token, Percent: window.UsedPercent, ResetAt: window.ResetAt, ObservedAt: window.ObservedAt})
	}
	if len(families) == 0 {
		return nil, quota.ErrNoWeeklyWindow
	}
	return families, nil
}

// fetchQuota reads one account's usage window, bounding the wait on the
// synchronous host callbacks with the configured timeout. A callback that
// outlives the timeout keeps its native admission and drains at shutdown.
func (m *Monitor) fetchQuota(ctx context.Context, entry protocol.HostAuthFileEntry, now time.Time) (quota.Observation, error) {
	if m.cfg.QuotaCachePath != "" {
		cached, err := quotaclient.ReadFresh(m.cfg.QuotaCachePath, entry.Provider, entry.AuthIndex, now, quotaCacheMaxAge)
		if err != nil {
			return quota.Observation{}, err
		}
		return quota.Observation{Provider: cached.Provider, Percent: cached.Percent, ResetAt: cached.ResetAt, ObservedAt: cached.ObservedAt}, nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, m.cfg.QuotaHTTPTimeout)
	defer cancel()
	type outcome struct {
		observation quota.Observation
		err         error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := m.quotaHost.GetAuth(timeoutCtx, entry.AuthIndex)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		observation, err := quota.Fetch(timeoutCtx, m.quotaHost, entry.Provider, raw, now)
		done <- outcome{observation: observation, err: err}
	}()
	select {
	case result := <-done:
		return result.observation, result.err
	case <-timeoutCtx.Done():
		return quota.Observation{}, timeoutCtx.Err()
	}
}

// applyQuotaLocked records a single-window observation and enqueues
// threshold notifications. Caller holds stateMu.
func (m *Monitor) applyQuotaLocked(account *state.Account, observation quota.Observation, now time.Time) {
	observedAt := now
	if m.cfg.QuotaCachePath != "" {
		observedAt = observation.ObservedAt
	}
	m.applyQuotaWindowLocked(account, "", observation.Percent, observation.ResetAt, observedAt, now)
}

// applyQuotaFamiliesLocked records each family's weekly window under its own
// latches. A family the read did not report keeps its latches until its
// window is over, so a family that drops out of one read and returns with the
// same reset is not alerted twice. Caller holds stateMu.
func (m *Monitor) applyQuotaFamiliesLocked(account *state.Account, families []familyObservation, now time.Time) {
	reported := make(map[string]struct{}, len(families))
	for _, family := range families {
		reported[family.Family] = struct{}{}
		m.applyQuotaWindowLocked(account, family.Family, family.Percent, family.ResetAt, family.ObservedAt, now)
	}
	for token, window := range account.Quota.Families {
		if _, ok := reported[token]; ok {
			continue
		}
		_, _, known := familyName(token)
		over := (!window.ResetAt.IsZero() && !window.ResetAt.After(now)) ||
			(window.ResetAt.IsZero() && (window.ObservedAt.IsZero() || now.Sub(window.ObservedAt) > familyRetention))
		if !known || over {
			delete(account.Quota.Families, token)
		}
	}
	if len(account.Quota.Families) == 0 {
		account.Quota.Families = nil
	}
}

// applyQuotaWindowLocked records one window's observation and enqueues its
// threshold notifications. family "" is the account's single window; any other
// value is a quota family with latches of its own. Caller holds stateMu.
func (m *Monitor) applyQuotaWindowLocked(account *state.Account, family string, percent float64, resetAt, observedAt, now time.Time) {
	window, _ := account.Quota.Window(family)
	windowKey := quotaWindowKey(family, resetAt)
	previousPercent := window.Percent
	hadObservation := !window.ObservedAt.IsZero()
	window.Percent = percent
	window.ResetAt = resetAt
	window.ObservedAt = observedAt

	// A new window (different reset instant, or a large drop in usage when the
	// provider reports no reset instant) clears the latches so the next window
	// alerts again.
	newWindow := window.WindowKey != windowKey
	if !newWindow && hadObservation && percent < m.cfg.QuotaWarningPercent && previousPercent >= m.cfg.QuotaWarningPercent {
		newWindow = true
	}
	if newWindow {
		window.WindowKey = windowKey
		window.WarningSentAt = time.Time{}
		window.ExhaustedSentAt = time.Time{}
		window.WarningAttemptAt = time.Time{}
		window.ExhaustedAttemptAt = time.Time{}
	}
	// The message is built from the stored window, so store it first.
	account.Quota.SetWindow(family, window)

	switch {
	case percent >= m.cfg.QuotaExhaustedPercent:
		if window.ExhaustedSentAt.IsZero() && due(window.ExhaustedAttemptAt, notificationRetryAfter, now) {
			if m.enqueueQuotaLocked(account, family, quotaExhaustedKind, windowKey, now) {
				window.ExhaustedAttemptAt = now
			}
		}
	case percent >= m.cfg.QuotaWarningPercent:
		if window.WarningSentAt.IsZero() && window.ExhaustedSentAt.IsZero() && due(window.WarningAttemptAt, notificationRetryAfter, now) {
			if m.enqueueQuotaLocked(account, family, quotaWarningKind, windowKey, now) {
				window.WarningAttemptAt = now
			}
		}
	}
	account.Quota.SetWindow(family, window)
}

// quotaWindowKey identifies the window that latches belong to: the reset
// instant, and for a quota family the family token in front of it. The
// single-window key is unchanged from earlier versions, so persisted latches
// keep matching.
func quotaWindowKey(family string, resetAt time.Time) string {
	key := ""
	if !resetAt.IsZero() {
		key = resetAt.UTC().Format(time.RFC3339)
	}
	if family == "" {
		return key
	}
	return family + "@" + key
}

func (m *Monitor) enqueueQuotaLocked(account *state.Account, family, kind, windowKey string, now time.Time) bool {
	if m.retiring.Load() || m.retired {
		return false
	}
	message := m.quotaMessage(account, family, kind)
	job := notifier.Job{
		Message: message,
		Valid: func() bool {
			m.stateMu.RLock()
			defer m.stateMu.RUnlock()
			window, ok := account.Quota.Window(family)
			if !m.accountCurrentLocked(account) || !ok || window.WindowKey != windowKey {
				return false
			}
			if kind == quotaExhaustedKind {
				return window.ExhaustedSentAt.IsZero()
			}
			return window.WarningSentAt.IsZero() && window.ExhaustedSentAt.IsZero()
		},
		Callback: m.quotaDeliveryCallback(account, family, kind, windowKey, now),
	}
	if !m.dispatcher.Enqueue(job) {
		m.data.LastNotificationErr = "notification queue is unavailable"
		return false
	}
	return true
}

func (m *Monitor) quotaDeliveryCallback(account *state.Account, family, kind, windowKey string, attemptAt time.Time) func(notifier.DeliveryResult) {
	return func(result notifier.DeliveryResult) {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		if m.retiring.Load() || m.retired {
			return
		}
		window, ok := account.Quota.Window(family)
		current := m.accountCurrentLocked(account) && ok && window.WindowKey == windowKey
		switch {
		case result.Unattempted:
			if current {
				if kind == quotaExhaustedKind && window.ExhaustedAttemptAt.Equal(attemptAt) {
					window.ExhaustedAttemptAt = time.Time{}
				} else if kind == quotaWarningKind && window.WarningAttemptAt.Equal(attemptAt) {
					window.WarningAttemptAt = time.Time{}
				}
				account.Quota.SetWindow(family, window)
			}
			return
		case result.Accepted:
			m.data.LastSuccessfulSend = result.At
			m.data.LastNotificationErr = ""
			if current {
				if kind == quotaExhaustedKind {
					window.ExhaustedSentAt = result.At
				} else {
					window.WarningSentAt = result.At
				}
				account.Quota.SetWindow(family, window)
			}
		default:
			m.data.LastNotificationErr = result.Error
		}
		_ = m.persistLocked()
		m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	}
}

// quotaMessage builds a threshold message. For a quota family the provider
// label and the limit both name the family, because the account's other
// families keep their own limits.
func (m *Monitor) quotaMessage(account *state.Account, family, kind string) notifier.Message {
	provider := titleProvider(account.Provider)
	window, _ := account.Quota.Window(family)
	percent := window.Percent
	remaining := 100 - percent
	if remaining < 0 {
		remaining = 0
	}
	reset := "unknown"
	if !window.ResetAt.IsZero() {
		reset = m.formatTime(window.ResetAt)
	}
	limit := "its weekly limit"
	routing := "Other healthy accounts will continue routing if available."
	if name, _, ok := familyName(family); ok && family != "" {
		provider = fmt.Sprintf("%s (%s)", provider, name)
		limit = fmt.Sprintf("its %s weekly limit", name)
		routing = "Its other model families have separate limits, and other healthy accounts will continue routing if available."
	}
	var title, body string
	if kind == quotaExhaustedKind {
		title = fmt.Sprintf("CLIProxyAPI: %s weekly limit reached", provider)
		body = fmt.Sprintf("%s has used %s of %s.\nResets: %s\n%s", account.Label, formatPercent(percent), limit, reset, routing)
	} else {
		title = fmt.Sprintf("CLIProxyAPI: %s weekly limit almost used", provider)
		body = fmt.Sprintf("%s has used %s of %s (%s remaining).\nResets: %s", account.Label, formatPercent(percent), limit, formatPercent(remaining), reset)
	}
	if m.cfg.ManagementURL != "" {
		body += "\nManagement: " + m.cfg.ManagementURL
	}
	return notifier.Message{
		Kind:       kind,
		Title:      title,
		Body:       body,
		Priority:   m.cfg.QuotaNotificationPriority,
		Provider:   provider,
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     kind,
	}
}

func formatPercent(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d%%", int64(value))
	}
	return fmt.Sprintf("%.1f%%", value)
}

func (m *Monitor) recordQuotaPollFailure(now time.Time, message string) {
	m.stateMu.Lock()
	if !m.retiring.Load() && !m.retired {
		m.status.LastQuotaPoll = now
		m.status.LastQuotaPollError = message
	}
	m.stateMu.Unlock()
}

// sanitizeQuotaError maps provider/host errors to closed, static text. The
// quota package already returns static errors; host callback and context
// errors are reduced here so no upstream or filesystem detail reaches status.
func sanitizeQuotaError(err error) string {
	var statusErr quota.HTTPStatusError
	switch {
	case errors.As(err, &statusErr):
		switch {
		case statusErr.StatusCode == 401 || statusErr.StatusCode == 403:
			return fmt.Sprintf("usage endpoint rejected the credential (HTTP %d)", statusErr.StatusCode)
		case statusErr.StatusCode == 429:
			return "usage endpoint rate limited the poll (HTTP 429)"
		case statusErr.StatusCode >= 500:
			return fmt.Sprintf("usage endpoint unavailable (HTTP %d)", statusErr.StatusCode)
		default:
			return statusErr.Error()
		}
	case errors.Is(err, context.DeadlineExceeded):
		return "usage request timed out"
	case errors.Is(err, context.Canceled), errors.Is(err, ErrStopping):
		return "usage request cancelled"
	case errors.Is(err, quota.ErrUnsupportedProvider), errors.Is(err, quota.ErrNoAccessToken), errors.Is(err, quota.ErrNoAccountID), errors.Is(err, quota.ErrInvalidResponse), errors.Is(err, quota.ErrNoWeeklyWindow):
		return err.Error()
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "host.auth.get"):
		return "host.auth.get failed"
	case strings.Contains(text, "host.http.do"):
		return "host.http.do failed"
	case strings.Contains(text, "shutting down"):
		return "usage request cancelled"
	}
	return "usage request failed"
}
