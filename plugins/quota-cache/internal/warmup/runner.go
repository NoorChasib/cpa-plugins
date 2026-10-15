package warmup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// Identity is one Claude credential as CPA listed it. ID is CPA's credential
// ID, which a model request is pinned to; Plan is the plan the cache last
// recorded for the account.
type Identity struct {
	AuthIndex, ID, Email, Plan string
	Disabled                   bool
}
type Runner interface {
	Warm(context.Context, Options, Identity) error
}
type RunnerFunc func(context.Context, Options, Identity) error

func (f RunnerFunc) Warm(ctx context.Context, o Options, id Identity) error { return f(ctx, o, id) }

// Host is the part of CPA's host API a warm-up uses.
type Host interface {
	GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error)
	HTTPCancel(context.Context, string) error
	ExecuteModel(context.Context, protocol.HostModelExecutionRequest) (protocol.HostModelExecutionResponse, error)
}

// CPA sends a warm-up through CPA's own Claude executor, pinned to one
// credential, so CPA refreshes and holds its token as for any other request.
// The response is checked for its type only and is neither logged nor saved;
// callers receive only static error codes.
type CPA struct {
	host Host
	// busy admits one model request at a time. A request Warm stopped waiting
	// for keeps it until CPA returns, so no second warm-up starts while one may
	// still be running upstream.
	busy chan struct{}
}

func NewCPA(host Host) *CPA { return &CPA{host: host, busy: make(chan struct{}, 1)} }

// subscribed reports whether a plan the cache recorded is a paid Claude
// subscription. An unknown plan is not.
func subscribed(plan string) bool {
	plan = strings.ToLower(strings.TrimSpace(plan))
	return plan == "pro" || plan == "max" || strings.HasPrefix(plan, "max_") || plan == "team" || plan == "enterprise"
}

func (r *CPA) Warm(ctx context.Context, o Options, id Identity) error {
	if !authIndex.MatchString(id.AuthIndex) {
		return errors.New("identity_unknown")
	}
	if r == nil || r.host == nil {
		return errors.New("host_unsupported")
	}
	// A stop or disable that lands before the request is sent proves nothing
	// was sent; its error must not read as a host or account problem.
	if ctx.Err() != nil {
		return errors.New("runner_unavailable")
	}
	select {
	case r.busy <- struct{}{}:
	default:
		return errors.New("runner_busy")
	}
	sent := false
	defer func() {
		if !sent {
			<-r.busy
		}
	}()
	// CPA before v7.3.3 drops auth_id without an error and would answer from
	// whichever account it chose. host.http.cancel came later, in v7.3.16, so a
	// host that knows it honours the pin. The fresh random ID names no
	// operation, so nothing is cancelled.
	probe := make([]byte, 16)
	if _, err := rand.Read(probe); err != nil {
		return errors.New("runner_unavailable")
	}
	if err := r.host.HTTPCancel(ctx, "quota-cache-warmup-probe-"+hex.EncodeToString(probe)); err != nil {
		if ctx.Err() != nil {
			return errors.New("runner_unavailable")
		}
		return errors.New("host_unsupported")
	}
	auth, err := r.host.GetRuntime(ctx, id.AuthIndex)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("runner_unavailable")
		}
		return errors.New("account_unavailable")
	}
	provider := auth.Provider
	if provider == "" {
		provider = auth.Type
	}
	switch {
	case auth.AuthIndex != id.AuthIndex || strings.TrimSpace(auth.ID) == "" || (id.ID != "" && auth.ID != id.ID):
		return errors.New("identity_unknown")
	case !strings.EqualFold(strings.TrimSpace(provider), "claude") || auth.AccountType != "oauth" || auth.RuntimeOnly || !subscribed(id.Plan):
		return errors.New("subscription_login_required")
	// CPA leaves Unavailable set after a cooldown ends, until the account's
	// next request or refresh, and selects by the deadline instead. So does
	// this; no deadline means CPA holds the account back for good.
	case auth.Disabled || auth.Unavailable && (auth.NextRetryAfter.IsZero() || auth.NextRetryAfter.After(time.Now())):
		return errors.New("account_unavailable")
	}
	if ctx.Err() != nil {
		return errors.New("runner_unavailable")
	}
	// No thinking field: CPA passes output_config.effort through as it is.
	// No system prompt, tools or alternative model.
	body, err := json.Marshal(map[string]any{
		"model":         o.Model,
		"max_tokens":    32,
		"messages":      []map[string]string{{"role": "user", "content": "Reply only OK."}},
		"output_config": map[string]string{"effort": o.Effort},
	})
	if err != nil {
		return errors.New("runner_unavailable")
	}
	request := protocol.HostModelExecutionRequest{EntryProtocol: "claude", ExitProtocol: "claude", Model: o.Model, Body: body, ForcedProvider: "claude", AuthID: auth.ID}
	done := make(chan error, 1)
	sent = true
	go func() {
		// The only part of a warm-up that may outlive Warm. It touches nothing
		// but its own buffered channel and the admission slot, which it frees
		// before answering, so a caller that hears the answer finds it free.
		response, err := r.host.ExecuteModel(ctx, request)
		var message struct {
			Type string `json:"type"`
		}
		var result error
		if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &message) != nil || message.Type != "message" {
			result = errors.New("request_failed")
		}
		<-r.busy
		done <- result
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		select {
		case err := <-done:
			return err
		default:
		}
		// CPA gives a model callback no deadline and cannot stop one once it is
		// sent, so the request may still reach the provider.
		return errors.New("outcome_unknown")
	}
}
