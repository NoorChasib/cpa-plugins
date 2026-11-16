package outbox

import (
	"context"
	"sort"
	"time"
)

// openRequestsTable holds the open requests a stopping plugin instance saved for the next one. Open
// creates it on a database made before it existed; older binaries ignore it, so user_version stays 1.
const openRequestsTable = `CREATE TABLE IF NOT EXISTS open_requests(
  request_id TEXT PRIMARY KEY CHECK(length(request_id) BETWEEN 1 AND 200),
  next_input_seq INTEGER NOT NULL,
  next_ordinal INTEGER NOT NULL,
  next_response INTEGER NOT NULL,
  responses INTEGER NOT NULL,
  before_auth_seen INTEGER NOT NULL,
  begin_written INTEGER NOT NULL,
  touched_at INTEGER NOT NULL,
  saved_at INTEGER NOT NULL) WITHOUT ROWID`

// OpenRequest is one open request's numbering and progress as a stopping plugin instance saved it
// (hooks.Carried). Request ids are CPA's; no content is stored.
type OpenRequest struct {
	RequestID    string
	NextInputSeq uint32
	NextOrdinal  uint32
	NextResponse uint32
	Responses    uint32
	BeforeAuth   bool
	BeginWritten bool
	TouchedAt    time.Time
}

// MaxOpenRequests bounds the requests one save keeps: past it, the least recently touched are left out.
const MaxOpenRequests = 16384

// SaveOpenRequests replaces the saved open requests with reqs in one durable transaction and returns
// how many it saved. A request id that is empty or longer than 200 bytes is left out.
func (o *Outbox) SaveOpenRequests(ctx context.Context, reqs []OpenRequest) (int, error) {
	if o.closed.Load() {
		return 0, ErrClosed
	}
	keep := make([]OpenRequest, 0, len(reqs))
	for _, r := range reqs {
		if n := len(r.RequestID); n >= 1 && n <= 200 {
			keep = append(keep, r)
		}
	}
	if len(keep) > MaxOpenRequests {
		sort.Slice(keep, func(i, j int) bool { return keep[i].TouchedAt.After(keep[j].TouchedAt) })
		keep = keep[:MaxOpenRequests]
	}
	now := o.opts.Now().UTC().UnixNano()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM open_requests"); err != nil {
		return 0, err
	}
	for _, r := range keep {
		if _, err = tx.ExecContext(ctx, `INSERT INTO open_requests(request_id,next_input_seq,next_ordinal,next_response,responses,
before_auth_seen,begin_written,touched_at,saved_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.RequestID, r.NextInputSeq, r.NextOrdinal,
			r.NextResponse, r.Responses, r.BeforeAuth, r.BeginWritten, r.TouchedAt.UTC().UnixNano(), now); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(keep), nil
}

// TakeOpenRequests returns the open requests saved within maxAge of now and deletes every saved
// one, in one transaction, so a later boot never adopts them again.
func (o *Outbox) TakeOpenRequests(ctx context.Context, maxAge time.Duration) ([]OpenRequest, error) {
	if o.closed.Load() {
		return nil, ErrClosed
	}
	since := o.opts.Now().Add(-maxAge).UTC().UnixNano()
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT request_id,next_input_seq,next_ordinal,next_response,responses,before_auth_seen,
begin_written,touched_at FROM open_requests WHERE saved_at>=?`, since)
	if err != nil {
		return nil, err
	}
	var out []OpenRequest
	for rows.Next() {
		var r OpenRequest
		var touched int64
		if err = rows.Scan(&r.RequestID, &r.NextInputSeq, &r.NextOrdinal, &r.NextResponse, &r.Responses, &r.BeforeAuth,
			&r.BeginWritten, &touched); err != nil {
			rows.Close()
			return nil, err
		}
		r.TouchedAt = time.Unix(0, touched).UTC()
		out = append(out, r)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM open_requests"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
