package aggregate

import (
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// WarmupStatus translates the independent journal into the summary's public
// contract. The journal keeps its own format and internal identity hashes.
type WarmupStatus struct {
	Enabled        bool                  `json:"enabled"`
	Model          string                `json:"model"`
	Effort         string                `json:"effort"`
	Hours          string                `json:"hours"`
	Timezone       string                `json:"timezone"`
	UpdatedAtEpoch *int64                `json:"updatedAtEpoch"`
	LastError      string                `json:"lastError,omitempty"`
	Accounts       []WarmupAccountStatus `json:"accounts"`
}

type WarmupAccountStatus struct {
	AuthIndex        string `json:"authIndex"`
	State            string `json:"state"`
	LastAttemptEpoch *int64 `json:"lastAttemptEpoch"`
	LastSuccessEpoch *int64 `json:"lastSuccessEpoch"`
	NextAttemptEpoch *int64 `json:"nextAttemptEpoch"`
	ResetAtEpoch     *int64 `json:"resetAtEpoch"`
	Error            string `json:"error,omitempty"`
}

func warmupOf(state *qc.WarmupState, now time.Time) *WarmupStatus {
	if state == nil {
		return nil
	}
	status := &WarmupStatus{
		Enabled: state.Enabled, Model: state.Model, Effort: state.Effort,
		Hours: state.Hours, Timezone: state.Timezone,
		UpdatedAtEpoch: epochPointerOf(state.UpdatedAt), LastError: state.LastError,
		Accounts: make([]WarmupAccountStatus, 0, len(state.Accounts)),
	}
	if state.Enabled && (state.UpdatedAt.IsZero() || state.UpdatedAt.After(now.Add(time.Minute)) || now.Sub(state.UpdatedAt) > 2*time.Minute) {
		status.LastError = "warmup_status_stale"
	}
	for _, account := range state.Accounts {
		status.Accounts = append(status.Accounts, WarmupAccountStatus{
			AuthIndex: account.AuthIndex, State: account.State, Error: account.Error,
			LastAttemptEpoch: epochPointerOf(account.LastAttempt), LastSuccessEpoch: epochPointerOf(account.LastSuccess),
			NextAttemptEpoch: epochPointerOf(account.NextAttempt), ResetAtEpoch: epochPointerOf(account.ResetAt),
		})
	}
	return status
}
