package api

import (
	"bytes"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
)

// The press ledger is what makes a repeated press harmless.
//
// A press on the token door arrives as a GET, and a GET is the request every
// layer between the page and this plugin feels free to send twice: a browser
// resending after a dropped connection, a proxy retrying an idempotent method
// against another upstream, an edge replaying TLS early data. The spend itself
// runs on its own context, so the first copy carries on whatever happened to
// its connection, and without this the second copy would spend again. Every
// press therefore carries an id the page drew at random, and the ledger
// remembers how each id was answered: the first copy spends, and every later
// copy is handed that same answer, byte for byte.
//
// It sits above the redeemer's journal and changes nothing in it. The journal
// repeats a provider claim whose outcome is unknown, for a new press; the
// ledger repeats this plugin's answer, for the same press. An outcome_unknown
// answer is stored like any other, so the same press id is told the same thing
// again without a second provider call, while a new press goes through the
// journal and repeats the same claim.
//
// An entry holds the credential id the press named and the answer it got.
// Nothing in it authenticates anything: no header, and never the token.
//
// It is in memory, like the journal, so anything that ends this copy of the
// plugin forgets every press. The next copy of an old press is then a fresh
// one, which is no worse than before the ledger existed: the journal still
// covers an outcome that was never learned.
type ledger struct {
	mu      sync.Mutex
	entries map[string]*pressEntry
	// clock and wait are fields so a test can run the ten minutes and the
	// minute without waiting for either.
	clock func() time.Time
	wait  time.Duration
}

type pressEntry struct {
	credentialID string
	// done is closed once answer is final. A replay waits on it rather than
	// on the lock, so a slow provider holds up its own press and nobody
	// else's.
	done       chan struct{}
	answer     protocol.ManagementResponse
	finished   bool
	finishedAt time.Time
}

const (
	// ledgerTTL is how long a finished press is remembered. It is the
	// journal's retry window, so a press stays single-use for at least as long
	// as the claim behind it can still be repeated.
	ledgerTTL = 10 * time.Minute
	// ledgerCapacity bounds what an authenticated caller minting fresh press
	// ids can make this plugin hold. A full ledger refuses a new press rather
	// than forgetting an old one early. A press is forgotten only once it has
	// been finished for its ten minutes, because forgetting it sooner is
	// exactly what would let a copy of it spend again. A press naming an
	// account the document does not offer answers at once and costs nothing,
	// so evicting finished presses at capacity would let anyone holding the
	// token push a spent press out in moments. 256 presses in ten minutes is
	// far beyond what a person pressing a button does; reaching it costs a
	// refusal that spends nothing, never a second spend.
	ledgerCapacity = 256
	// replayWait is how long a repeat waits for the press it repeats: the
	// redeemer's whole bound and a little more, so the original has always
	// finished by then unless something is badly wrong. Past it the repeat is
	// told the press is still under way, which spends nothing.
	replayWait = redeem.Timeout + 5*time.Second
)

var (
	// errPressMismatch means the press id was first used for a different
	// credential. One press is one decision about one account; reusing its id
	// for another is a broken client or a forgery, and either way not a spend.
	errPressMismatch = errors.New("the press id was first used for another credential")
	// errLedgerFull means the ledger holds as many presses as it may, each
	// still in flight or inside its ten minutes, so none can be forgotten
	// safely to make room. Room returns as they age out.
	errLedgerFull = errors.New("every remembered press is still in flight or inside its ten minutes")
)

func newLedger() *ledger {
	return &ledger{entries: map[string]*pressEntry{}, clock: time.Now, wait: replayWait}
}

// begin reserves pressID for credentialID. Fresh means this is the first copy
// of the press: the caller spends, and must call finish whatever happens.
// Otherwise the entry is the earlier copy's, and replay hands out its answer.
func (l *ledger) begin(pressID, credentialID string) (entry *pressEntry, fresh bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	for id, entry := range l.entries {
		if entry.finished && !now.Before(entry.finishedAt.Add(ledgerTTL)) {
			delete(l.entries, id)
		}
	}
	if earlier, ok := l.entries[pressID]; ok {
		if earlier.credentialID != credentialID {
			return nil, false, errPressMismatch
		}
		return earlier, false, nil
	}
	// Everything still here after the sweep is in flight or inside its ten
	// minutes, so nothing here can be forgotten to make room.
	if len(l.entries) >= ledgerCapacity {
		return nil, false, errLedgerFull
	}
	entry = &pressEntry{credentialID: credentialID, done: make(chan struct{})}
	l.entries[pressID] = entry
	return entry, true, nil
}

// finish records the first copy's answer and releases every copy waiting on
// it. The TTL runs from here, not from begin: a press is remembered for ten
// minutes after its answer exists.
func (l *ledger) finish(entry *pressEntry, answer protocol.ManagementResponse) {
	l.mu.Lock()
	entry.answer, entry.finished, entry.finishedAt = answer, true, l.clock()
	l.mu.Unlock()
	close(entry.done)
}

// replay waits for the earlier copy's answer and returns it marked as a
// replay. False means the earlier copy is still under way past the wait.
//
// The answer is copied rather than shared: the stored one is handed to every
// later copy, and a header added to it in place would be a data race between
// them.
func (l *ledger) replay(entry *pressEntry) (protocol.ManagementResponse, bool) {
	timer := time.NewTimer(l.wait)
	defer timer.Stop()
	select {
	case <-entry.done:
	case <-timer.C:
		return protocol.ManagementResponse{}, false
	}
	l.mu.Lock()
	answer := entry.answer
	l.mu.Unlock()
	headers := answer.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set(replayedHeader, "1")
	return protocol.ManagementResponse{StatusCode: answer.StatusCode, Headers: headers, Body: bytes.Clone(answer.Body)}, true
}
