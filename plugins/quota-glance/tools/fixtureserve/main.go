// Command fixtureserve serves a snapshot fixture through the real plugin
// handler, so the routes can be exercised with curl without a CPA instance.
//
// It is a development tool: it stands in for CPA's request dispatch and nothing
// else. The handler, the document, the token check, and the ETag are the
// production ones.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/api"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
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
	flag.Parse()

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
	}, now)

	served := api.New("quota-glance", *token)
	served.Publish(doc, api.Health{Version: "fixtureserve", CachePath: *snapshot, StaleAfter: "45m"})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		res := served.Handle(protocol.ManagementRequest{
			Method: r.Method, Path: r.URL.Path, Headers: r.Header, Query: r.URL.Query(),
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
	fmt.Println("  (CPA authenticates the management tree in production; this stand-in does not)")
	log.Fatal(http.ListenAndServe(*addr, nil))
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
		files = append(files, protocol.HostAuthFileEntry{
			AuthIndex: entry.AuthIndex, Provider: entry.Provider, Name: entry.AuthIndex,
		})
	}
	return files, nil
}
