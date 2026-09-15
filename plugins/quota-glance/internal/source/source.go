// Package source reads the quota-cache snapshot and the host's credential
// roster. It makes no network request of any kind: quota-cache is the only
// component in this system that ever contacts a provider, and a second poller
// would compete for the exact rate limits quota-cache exists to protect.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// Host is the subset of CPA callbacks this plugin uses. Identity is free and
// in-process; there is no management key anywhere in this plugin.
type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
}

// Result is one read of both inputs. Reason is empty on success and otherwise
// carries an aggregate stale reason, so a schema bump in quota-cache surfaces
// as itself rather than as a generic read failure.
type Result struct {
	Snapshot   qc.Snapshot
	Reason     string
	Identities []aggregate.Identity
}

var ErrUnreadable = errors.New("quota-cache snapshot cannot be read at the configured cache-path")

// Readable reports whether the snapshot path can be read at all. Startup uses
// it to refuse plainly rather than serve an empty document that looks like a
// working install with no quota.
func Readable(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ErrUnreadable
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrUnreadable
	}
	return file.Close()
}

// snapshotSchema peeks at the outer schema so an unsupported version is
// distinguishable from a missing or corrupt file. qc.Load reports both as
// unavailable, which is correct for it and not specific enough here.
func snapshotSchema(path string) (int, bool) {
	// Regular files only, checked before opening. A FIFO opened read-only
	// blocks until a writer appears, and this runs on the watcher goroutine,
	// which the plugin's shutdown path waits for while holding its lifecycle
	// lock — so one unopenable path would wedge the plugin permanently and
	// stop CPA unloading it. qc.Load guards the same way.
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, qc.MaxBytes+1))
	if err != nil || len(raw) > qc.MaxBytes {
		return 0, false
	}
	var envelope struct {
		Schema int `json:"schema"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return 0, false
	}
	return envelope.Schema, true
}

// Read loads the snapshot and roster.
//
// qc.Load is used deliberately in place of qc.ReadFresh. ReadFresh reports
// unavailable when an entry is stale, carries an error, or has a reset in the
// past, which is right for a consumer that must not act on stale evidence and
// wrong for a display: a dashboard should show a value with a staleness badge,
// never a blank. The staleness judgement is made in aggregate instead.
func Read(ctx context.Context, host Host, path string) Result {
	result := Result{}
	snapshot, err := qc.Load(path)
	if err != nil {
		// Only a positive, different version is a version bump. A file with no
		// schema key at all is not a quota-cache snapshot, and saying
		// "unsupported schema" about it would send the reader after the wrong
		// problem.
		if schema, ok := snapshotSchema(path); ok && schema > 0 && schema != 1 {
			result.Reason = aggregate.ReasonSchemaUnsupported
		} else {
			result.Reason = aggregate.ReasonCacheMissing
		}
	} else {
		result.Snapshot = snapshot
	}
	identities, rosterErr := identities(ctx, host)
	result.Identities = identities
	// A roster the host could not supply is a source failure, not an empty
	// roster. Reporting it as success would publish a document with no
	// credentials and let the caller latch that emptiness as its last good
	// state. A genuinely empty roster returns no error and is published as is.
	if rosterErr != nil && result.Reason == "" {
		result.Reason = aggregate.ReasonRosterUnavailable
	}
	return result
}

// identities converts the host roster. A credential whose provider quota-cache
// does not poll still appears, so the dashboard shows it as unsupported rather
// than silently omitting a credential the user paid for.
func identities(ctx context.Context, host Host) ([]aggregate.Identity, error) {
	out := []aggregate.Identity{}
	if host == nil {
		return out, errors.New("no host callback available")
	}
	roster, err := host.ListAuth(ctx)
	if err != nil {
		return out, errors.New("credential roster unavailable")
	}
	for _, entry := range roster {
		if entry.AuthIndex == "" || entry.RuntimeOnly {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		out = append(out, aggregate.Identity{
			AuthIndex:   entry.AuthIndex,
			Provider:    provider,
			Label:       entry.Label,
			Email:       entry.Email,
			Disabled:    entry.Disabled,
			Unavailable: entry.Unavailable,
			Recent:      recentOf(entry.RecentRequests),
		})
	}
	return out, nil
}

// recentOf carries CPA's rolling request counter through in the order the host
// reports it, oldest bucket first.
//
// It is copied rather than aliased because the host response is decoded per
// call and this slice outlives it inside a published document. Nothing is
// summed or rescaled here: the aggregate owns every judgement about what the
// counts mean, including the scale they are drawn against.
func recentOf(buckets []protocol.HostRecentRequestEntry) []aggregate.RecentRequest {
	if len(buckets) == 0 {
		return nil
	}
	out := make([]aggregate.RecentRequest, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, aggregate.RecentRequest{
			Label:   bucket.Time,
			Success: bucket.Success,
			Failed:  bucket.Failed,
		})
	}
	return out
}
