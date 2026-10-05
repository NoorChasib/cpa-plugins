// Command fixtureserve serves a snapshot fixture through the real plugin
// handler, so the routes can be exercised with curl without a CPA instance.
//
// It is a development tool: it stands in for CPA's request dispatch and nothing
// else. The handler, the document, the token check, and the ETag are the
// production ones. It never contacts a provider: with -redeem set, a press on
// the button is answered by a stand-in that returns the ending named, so each
// one can be looked at without spending anything.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/api"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/redeem"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/source"
)

type roster struct{ files []protocol.HostAuthFileEntry }

func (r roster) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) { return r.files, nil }

func main() {
	snapshot := flag.String("snapshot", "testdata/snapshots/seven-credentials.json", "snapshot fixture to serve")
	rosterPath := flag.String("roster", "", "host.auth.list fixture: a JSON array of auth entries. Defaults to one plain entry per snapshot entry.")
	token := flag.String("token", "dev-token", "fallback web token for the public summary route")
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	epoch := flag.Int64("now", 1789012800, "build clock, in Unix seconds")
	ending := flag.String("redeem", "", "answer every confirmed redeem with this ending and offer the button: "+
		"an outcome (reset, nothingToReset, noCredit, failed, notLimited, cooldown, paused, ineligible, alreadyUsed) "+
		"or an error (outcome_unknown, retry_window_closed, provider_rate_limited, provider_refused, provider_unavailable, already_in_flight, credential_unusable). "+
		"Empty leaves redemption off")
	flag.Parse()
	stand, err := standInFor(*ending)
	if err != nil {
		log.Fatal(err)
	}

	now := time.Unix(*epoch, 0).UTC()
	files, err := rosterFor(*snapshot, *rosterPath)
	if err != nil {
		log.Fatal(err)
	}
	result := source.Read(context.Background(), roster{files: files}, *snapshot)
	if result.Reason != "" {
		log.Printf("snapshot could not be read: %s", result.Reason)
		os.Exit(1)
	}
	doc := aggregate.Build(aggregate.Input{
		Snapshot:   result.Snapshot,
		Identities: result.Identities,
		StaleAfter: 45 * time.Minute,
		Redeemable: stand != nil,
	}, now)

	served := api.New("quota-glance", *token)
	served.Publish(doc, api.Health{Version: "fixtureserve", CachePath: *snapshot, StaleAfter: "45m"})
	if stand != nil {
		served.SetRedeemer(stand)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The redeem body, bounded well past what the route accepts so the
		// route's own size check is the one that answers.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		res := served.Handle(protocol.ManagementRequest{
			Method: r.Method, Path: r.URL.Path, Headers: r.Header, Query: r.URL.Query(), Body: body,
		}, time.Now())
		for name, values := range res.Headers {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(res.Body)
	})
	fmt.Printf("serving %s on http://%s\n", *snapshot, *addr)
	fmt.Printf("  page     http://%s/v0/resource/plugins/quota-glance/app\n", *addr)
	fmt.Printf("  document http://%s/v0/management/plugins/quota-glance/summary\n", *addr)
	fmt.Printf("  fallback http://%s/v0/resource/plugins/quota-glance/summary  (Bearer %s)\n", *addr, *token)
	if stand != nil {
		fmt.Printf("  redeem   POST http://%s/v0/management/plugins/quota-glance/redeem  (answers %q, contacts nothing)\n", *addr, *ending)
		fmt.Printf("  spend    GET http://%s/v0/resource/plugins/quota-glance/spend  (Bearer %s + X-Quota-Glance-Spend)\n", *addr, *token)
	}
	fmt.Println("  (CPA authenticates the management tree in production; this stand-in does not)")
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// standIn answers every press with one fixed ending, in place of a redeemer
// that would reach a provider.
type standIn struct {
	result redeem.Result
	err    error
}

func (s standIn) Redeem(context.Context, string, string) (redeem.Result, error) {
	// The real redeemer names the instant its unresolved claim stops being
	// repeated. Ten minutes from the press is what a first unknown answer
	// would say.
	if s.err == redeem.ErrOutcomeUnknown {
		return redeem.Result{}, &redeem.OutcomeUnknownError{RetryUntil: time.Now().Add(10 * time.Minute)}
	}
	return s.result, s.err
}

// standInFor maps a -redeem value onto the ending the real redeemer would
// produce: an outcome, or the error the API turns into that code.
func standInFor(ending string) (*standIn, error) {
	switch ending {
	case "":
		return nil, nil
	case redeem.OutcomeReset, redeem.OutcomeNothingToReset, redeem.OutcomeAlreadyUsed:
		return &standIn{result: redeem.Result{Outcome: ending, WindowsReset: 2, RemainingCount: 1}}, nil
	case redeem.OutcomeNoCredit, redeem.OutcomeFailed, redeem.OutcomeNotLimited,
		redeem.OutcomeCooldown, redeem.OutcomePaused, redeem.OutcomeIneligible:
		return &standIn{result: redeem.Result{Outcome: ending, RemainingCount: 2}}, nil
	}
	for code, err := range map[string]error{
		"outcome_unknown":       redeem.ErrOutcomeUnknown,
		"retry_window_closed":   redeem.ErrRetryWindowClosed,
		"provider_rate_limited": redeem.ErrRateLimited,
		"provider_refused":      redeem.ErrRefused,
		"provider_unavailable":  redeem.ErrUnavailable,
		"already_in_flight":     redeem.ErrInFlight,
		"credential_unusable":   redeem.ErrNoAccessToken,
	} {
		if code == ending {
			return &standIn{err: err}, nil
		}
	}
	return nil, fmt.Errorf("-redeem %q is not an ending this stand-in knows", ending)
}

// rosterFor stands in for host.auth.list.
//
// Derived from the snapshot by default: every credential in the fixture, in an
// order that is deliberately not the display order. A roster file overrides
// that, because the roster carries facts the snapshot cannot — disabled,
// unavailable, a credential CPA knows about that quota-cache has never polled,
// and `recent_requests`, the routing activity under each address — and those
// are exactly the states worth looking at a page for. A derived roster carries
// no activity, so the page renders as it does against a CPA that reports none:
// pass -roster to see the strips.
func rosterFor(snapshotPath, rosterPath string) ([]protocol.HostAuthFileEntry, error) {
	if rosterPath != "" {
		raw, err := os.ReadFile(rosterPath)
		if err != nil {
			return nil, fmt.Errorf("roster fixture: %w", err)
		}
		files := []protocol.HostAuthFileEntry{}
		if err := json.Unmarshal(raw, &files); err != nil {
			return nil, fmt.Errorf("roster fixture: %w", err)
		}
		return files, nil
	}
	snapshot := source.Read(context.Background(), nil, snapshotPath)
	files := []protocol.HostAuthFileEntry{}
	for _, entry := range snapshot.Snapshot.Entries {
		// OpenRouter is read with quota-cache's own management key and is
		// never in CPA's roster; listing it would invent a credential.
		if entry.Provider == "openrouter" {
			continue
		}
		files = append(files, protocol.HostAuthFileEntry{
			AuthIndex: entry.AuthIndex, Provider: entry.Provider, Name: entry.AuthIndex,
		})
	}
	return files, nil
}
