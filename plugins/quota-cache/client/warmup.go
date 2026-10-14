package client

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// WarmupState is the private, credential-free status published beside the
// snapshot. It is informational; only Quota Cache may schedule warm-ups.
type WarmupState struct {
	Schema    int             `json:"schema"`
	Enabled   bool            `json:"enabled"`
	Model     string          `json:"model"`
	Effort    string          `json:"effort"`
	Hours     string          `json:"hours"`
	Timezone  string          `json:"timezone"`
	UpdatedAt time.Time       `json:"updated_at"`
	LastError string          `json:"last_error,omitempty"`
	Accounts  []WarmupAccount `json:"accounts"`
}

type WarmupAccount struct {
	AuthIndex    string    `json:"auth_index"`
	IdentityHash string    `json:"identity_hash,omitempty"`
	State        string    `json:"state"`
	LastAttempt  time.Time `json:"last_attempt,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	NextAttempt  time.Time `json:"next_attempt,omitempty"`
	ResetAt      time.Time `json:"reset_at,omitempty"`
	Error        string    `json:"error,omitempty"`
}

func WarmupPath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".warmup.json"
}

func LoadWarmup(snapshotPath string) (*WarmupState, error) {
	path := WarmupPath(snapshotPath)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return nil, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	var state WarmupState
	if err != nil || len(raw) > 256<<10 || json.Unmarshal(raw, &state) != nil || state.Schema != 1 || len(state.Accounts) > 256 {
		return nil, ErrUnavailable
	}
	seen := map[string]bool{}
	for _, account := range state.Accounts {
		if _, err := hex.DecodeString(account.AuthIndex); err != nil || len(account.AuthIndex) != 16 || seen[account.AuthIndex] {
			return nil, ErrUnavailable
		}
		if !account.LastAttempt.IsZero() && account.NextAttempt.Before(account.LastAttempt) {
			return nil, ErrUnavailable
		}
		seen[account.AuthIndex] = true
	}
	return &state, nil
}
