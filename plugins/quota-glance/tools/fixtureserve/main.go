// Command fixtureserve serves a snapshot fixture through the real plugin
// handler, so the routes can be exercised with curl without a CPA instance.
//
// It is a development tool: it stands in for CPA's request dispatch and nothing
// else. The handler, the document, the token check, and the ETag are the
// production ones.
package main

import (
	"context"
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
	token := flag.String("token", "dev-token", "fallback web token for the public summary route")
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	epoch := flag.Int64("now", 1789012800, "build clock, in Unix seconds")
	flag.Parse()

	now := time.Unix(*epoch, 0).UTC()
	result := source.Read(context.Background(), roster{files: rosterFor(*snapshot)}, *snapshot)
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

// rosterFor stands in for host.auth.list: every credential in the fixture, in
// an order that is deliberately not the display order.
func rosterFor(path string) []protocol.HostAuthFileEntry {
	snapshot := source.Read(context.Background(), nil, path)
	files := []protocol.HostAuthFileEntry{}
	for _, entry := range snapshot.Snapshot.Entries {
		files = append(files, protocol.HostAuthFileEntry{
			AuthIndex: entry.AuthIndex, Provider: entry.Provider, Name: entry.AuthIndex,
		})
	}
	return files
}
