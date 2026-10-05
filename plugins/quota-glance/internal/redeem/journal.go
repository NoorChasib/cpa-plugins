package redeem

import "time"

// retryWindow is how long an unresolved claim is repeated rather than replaced.
//
// It is the CPA console's own window for the same journal, and it exists
// because a provider matches a repeated request id against the original for a
// limited time only. Inside it, the press after an unknown outcome sends the
// same claim again, and the provider either reports what happened to it or
// acts on it once. Past it, the entry is dropped and the next press starts from
// a fresh inventory read — which, if the earlier claim did spend, finds that
// reset gone and either picks another the operator still holds or finds
// nothing to spend.
//
// The window runs from when the claim was first made, and a repeat does not
// extend it, for the same reason: the provider's clock started with the
// original. That is why an unknown answer names the instant the window closes
// (OutcomeUnknownError) rather than a length — the reader of the third answer
// has less of it left than the reader of the first. The journal is in memory,
// so anything that ends this copy of the plugin closes the window early: a CPA
// restart, and equally an update of the plugin or switching it off and on,
// which CPA carries out by loading a fresh copy without restarting itself.
const retryWindow = 10 * time.Minute

// pendingClaim is a spend request whose answer never arrived.
//
// It holds identifiers and counts only. Nothing in it authenticates anything:
// the access token is read from CPA afresh on every press and never kept here.
type pendingClaim struct {
	provider string
	// creditID is the Codex credit id or the Claude grant id the claim names.
	creditID string
	// requestID is the idempotency key the claim was sent with: Codex's
	// redeem_request_id, Claude's request_id.
	requestID string
	// account is who the claim was made for: the Claude organization uuid, or
	// the ChatGPT account id. A repeat is refused if the credential now
	// authenticates as anyone else.
	account   string
	createdAt time.Time
	// others and grantLeft split what the account held when the claim was
	// chosen — every other reset, and those left on the credit or grant the
	// claim names — so a repeat can still say what remains without re-reading
	// the inventory it deliberately skips.
	others    int
	grantLeft int
}

// unresolved returns the credential's unresolved claim, if one is still inside
// its retry window. An entry past the window, or one made for a different
// provider than the credential now reports, is dropped on the way out: neither
// can be repeated, and leaving it would only refuse a press later.
func (r *Redeemer) unresolved(authIndex, provider string) (pendingClaim, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claim, ok := r.pending[authIndex]
	if !ok {
		return pendingClaim{}, false
	}
	if claim.provider != provider || !r.withinWindowLocked(claim) {
		delete(r.pending, authIndex)
		return pendingClaim{}, false
	}
	return claim, true
}

// remember records a claim before it is sent. Entries past their window are
// swept on the way, so the journal holds at most one entry per credential that
// has had an unknown outcome in the last ten minutes.
func (r *Redeemer) remember(authIndex string, claim pendingClaim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string]pendingClaim{}
	}
	for key, existing := range r.pending {
		if !r.withinWindowLocked(existing) {
			delete(r.pending, key)
		}
	}
	r.pending[authIndex] = claim
}

// forget drops a claim whose answer settled it.
func (r *Redeemer) forget(authIndex string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, authIndex)
}

func (r *Redeemer) withinWindow(claim pendingClaim) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withinWindowLocked(claim)
}

func (r *Redeemer) withinWindowLocked(claim pendingClaim) bool {
	return r.clock().Sub(claim.createdAt) < retryWindow
}

func (r *Redeemer) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}
